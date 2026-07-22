package cache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIsCachablePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/pkg/-/pkg-1.0.0.tgz", true},
		{"/@scope/pkg/-/pkg-1.0.0.tgz", true},
		{"/a/b/c.tar.gz", true},
		{"/a/b/c.zip", true},
		{"/a/b/c.gz", true},
		{"/a/b/c.TGZ", true},   // case-insensitive
		{"/pkg", false},        // metadata
		{"/@scope/pkg", false}, // metadata
		{"/pkg/1.0.0", false},  // metadata
		{"/-/ping", false},     // registry special
		{"/-/v1/search", false},
		{"/", false},
	}
	for _, c := range cases {
		if got := isCachablePath(c.path); got != c.want {
			t.Errorf("isCachablePath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestCacheRelPathSafety(t *testing.T) {
	cases := []struct {
		path, query, want string
		ok                bool
	}{
		{"/pkg/-/pkg-1.0.0.tgz", "", "pkg/-/pkg-1.0.0.tgz", true},
		{"/@scope/pkg/-/pkg-1.0.0.tgz", "", "@scope/pkg/-/pkg-1.0.0.tgz", true},
		{"/pkg/-/pkg-1.0.0.tgz", "x=1", "", true}, // query hashed -> only structure asserted below
		{"/../etc/passwd", "", "", false},         // traversal rejected
		{"/a/../../etc", "", "", false},           // traversal rejected
		{"/", "", "", false},                      // empty after trim
	}
	for _, c := range cases {
		// For the query case the exact hash suffix depends on sha256("x=1"); only
		// assert structure for that one, exact values for the rest.
		got, ok := cacheRelPath(c.path, c.query)
		if ok != c.ok {
			t.Errorf("cacheRelPath(%q,%q) ok = %v, want %v", c.path, c.query, ok, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if c.query == "" {
			if got != c.want {
				t.Errorf("cacheRelPath(%q,%q) = %q, want %q", c.path, c.query, got, c.want)
			}
		} else {
			wantPrefix := "pkg/-/pkg-1.0.0.tgz_"
			if len(got) < len(wantPrefix)+8 || got[:len(wantPrefix)] != wantPrefix {
				t.Errorf("cacheRelPath(%q,%q) = %q, want prefix %q+8hex", c.path, c.query, got, wantPrefix)
			}
		}
	}
}

func TestServeHitAndMiss(t *testing.T) {
	dir := t.TempDir()
	// dir serves as both a read directory and the write directory.
	c, err := New([]string{dir}, dir, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rel := "pkg/-/pkg-1.0.0.tgz"
	if err := os.MkdirAll(filepath.Join(dir, "pkg/-"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte("tarball-body"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hit: served from cache, upstream fully bypassed.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	if !c.ServeHit(rec, req) {
		t.Fatal("expected cache hit")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "tarball-body" {
		t.Errorf("body = %q, want tarball-body", rec.Body.String())
	}
	if rec.Header().Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", rec.Header().Get("X-Cache"))
	}
	if rec.Header().Get("Content-Length") != "12" {
		t.Errorf("Content-Length = %q, want 12", rec.Header().Get("Content-Length"))
	}

	// Miss: unknown tarball, nothing written.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/other/-/other-2.0.0.tgz", nil)
	if c.ServeHit(rec2, req2) {
		t.Fatal("expected cache miss, got hit")
	}
	if rec2.Code != 200 || rec2.Body.Len() != 0 {
		t.Errorf("miss should not write a response, got code=%d body=%q", rec2.Code, rec2.Body.String())
	}

	// Non-cachable path (metadata) -> never a hit.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	if c.ServeHit(rec3, req3) {
		t.Fatal("metadata path should not hit cache")
	}
}

func TestCaptureCommit(t *testing.T) {
	dir := t.TempDir()
	c, err := New([]string{dir}, dir, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	rec := httptest.NewRecorder()
	ww, fin := c.Capture(rec, req)
	if ww == nil || fin == nil {
		t.Fatal("expected capture wrapper")
	}

	ww.Header().Set("Content-Type", "application/octet-stream")
	ww.WriteHeader(http.StatusOK)
	io.WriteString(ww, "streamed-tarball")
	fin(true)

	got, err := os.ReadFile(filepath.Join(dir, "pkg/-/pkg-1.0.0.tgz"))
	if err != nil {
		t.Fatalf("cached file not written: %v", err)
	}
	if string(got) != "streamed-tarball" {
		t.Errorf("cached body = %q, want streamed-tarball", got)
	}
	if rec.Body.String() != "streamed-tarball" {
		t.Errorf("client body = %q, want streamed-tarball", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("client status = %d, want 200", rec.Code)
	}
	// No leftover staging files.
	if entries, _ := os.ReadDir(filepath.Join(dir, ".tmp")); len(entries) != 0 {
		t.Errorf(".tmp not cleaned: %d entries", len(entries))
	}
}

func TestCaptureAbortNoFile(t *testing.T) {
	dir := t.TempDir()
	c, err := New([]string{dir}, dir, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	rec := httptest.NewRecorder()
	ww, fin := c.Capture(rec, req)
	io.WriteString(ww, "partial")
	fin(false) // download failed -> discard

	if _, err := os.Stat(filepath.Join(dir, "pkg/-/pkg-1.0.0.tgz")); !os.IsNotExist(err) {
		t.Errorf("expected no cached file after abort, got err=%v", err)
	}
	// Client still received the bytes (Forward streams live).
	if rec.Body.String() != "partial" {
		t.Errorf("client body = %q, want partial", rec.Body.String())
	}
}

func TestCaptureWriteErrorNoFile(t *testing.T) {
	dir := t.TempDir()
	c, err := New([]string{dir}, dir, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	ww, fin := c.Capture(&errWriter{}, req)
	io.WriteString(ww, "whatever") // client write fails -> must not commit
	fin(true)

	if _, err := os.Stat(filepath.Join(dir, "pkg/-/pkg-1.0.0.tgz")); !os.IsNotExist(err) {
		t.Errorf("expected no cached file on client write error, got err=%v", err)
	}
}

func TestCaptureDisabledForNonTarballAndNoWriteDir(t *testing.T) {
	// No write dir: capture is a no-op pass-through.
	cReadOnly, _ := New([]string{t.TempDir()}, "", false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	if ww, fin := cReadOnly.Capture(rec, req); ww != rec || fin != nil {
		t.Error("expected pass-through when no write dir")
	}

	// Write dir present but path is metadata: capture is a no-op.
	cRW, _ := New([]string{t.TempDir()}, t.TempDir(), false)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	if ww, fin := cRW.Capture(rec2, req2); ww != rec2 || fin != nil {
		t.Error("expected pass-through for non-cachable path")
	}
}

// errWriter is an http.ResponseWriter whose Write always fails, simulating a
// disconnected client mid-stream.
type errWriter struct{}

func (errWriter) Header() http.Header       { return http.Header{} }
func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (errWriter) WriteHeader(int)           {}
