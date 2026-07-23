package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"npm-auto-proxy/internal/config"
)

func TestRewriteBaseURL(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		host       string
		proto      string
		fwdHost    string
		want       string
	}{
		{"configured wins", "http://127.0.0.1:48180", "other:9", "", "", "http://127.0.0.1:48180"},
		{"configured trims trailing slash", "http://h/", "", "", "", "http://h"},
		{"direct request uses Host", "", "127.0.0.1:48180", "", "", "http://127.0.0.1:48180"},
		{"forwarded proto and host", "", "127.0.0.1:48180", "https", "npm-registry.example.com", "https://npm-registry.example.com"},
		{"forwarded host chain leftmost", "", "127.0.0.1:48180", "", "client.com, proxy.com", "http://client.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/pkg", nil)
			req.Host = c.host
			if c.proto != "" {
				req.Header.Set("X-Forwarded-Proto", c.proto)
			}
			if c.fwdHost != "" {
				req.Header.Set("X-Forwarded-Host", c.fwdHost)
			}
			if got := rewriteBaseURL(req, c.configured); got != c.want {
				t.Errorf("rewriteBaseURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsPackageMetadataPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/zzc-gopage", true},
		{"/@scope/pkg", true},
		{"/pkg/1.0.0", true},
		{"/pkg/-/pkg-1.0.0.tgz", false},
		{"/@scope/pkg/-/pkg-1.0.0.tgz", false},
		{"/-/ping", false},
		{"/-/v1/search", false},
		{"/", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isPackageMetadataPath(c.path); got != c.want {
			t.Errorf("isPackageMetadataPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestRewriteTarballURL(t *testing.T) {
	base := "http://127.0.0.1:48180"
	cases := []struct {
		orig, want string
	}{
		{"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz", "http://127.0.0.1:48180/pkg/-/pkg-1.0.0.tgz"},
		{"https://npm-registry.zuzuche.net/zzc-gopage/-/zzc-gopage-1.0.8.tgz", "http://127.0.0.1:48180/zzc-gopage/-/zzc-gopage-1.0.8.tgz"},
		{"https://h/pkg/-/pkg-1.0.0.tgz?foo=bar", "http://127.0.0.1:48180/pkg/-/pkg-1.0.0.tgz?foo=bar"},
	}
	for _, c := range cases {
		if got := rewriteTarballURL(c.orig, base); got != c.want {
			t.Errorf("rewriteTarballURL(%q) = %q, want %q", c.orig, got, c.want)
		}
	}
	// Invalid base -> original returned unchanged.
	if got := rewriteTarballURL("https://h/pkg/-/pkg.tgz", "://bad"); got != "https://h/pkg/-/pkg.tgz" {
		t.Errorf("invalid base: got %q, want orig", got)
	}
	if got := rewriteTarballURL("https://h/pkg/-/pkg.tgz", "http://"); got != "https://h/pkg/-/pkg.tgz" {
		t.Errorf("base without host: got %q, want orig", got)
	}
}

func TestRewriteTarballs(t *testing.T) {
	base := "http://127.0.0.1:48180"

	// Full manifest: both versions rewritten, non-tarball fields preserved.
	manifest := `{"name":"pkg","versions":{"1.0.0":{"dist":{"tarball":"https://h/pkg/-/pkg-1.0.0.tgz","shasum":"abc"}},"1.1.0":{"dist":{"tarball":"https://h/pkg/-/pkg-1.1.0.tgz"}}},"dist-tags":{"latest":"1.1.0"}}`
	out := rewriteTarballs([]byte(manifest), base)
	if out == nil {
		t.Fatal("expected rewritten bytes, got nil")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten output is not valid JSON: %v", err)
	}
	versions := doc["versions"].(map[string]any)
	tb00 := versions["1.0.0"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)
	tb10 := versions["1.1.0"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)
	if tb00 != "http://127.0.0.1:48180/pkg/-/pkg-1.0.0.tgz" {
		t.Errorf("1.0.0 tarball = %q", tb00)
	}
	if tb10 != "http://127.0.0.1:48180/pkg/-/pkg-1.1.0.tgz" {
		t.Errorf("1.1.0 tarball = %q", tb10)
	}
	if sh := versions["1.0.0"].(map[string]any)["dist"].(map[string]any)["shasum"].(string); sh != "abc" {
		t.Errorf("shasum not preserved: %q", sh)
	}

	// Non-JSON -> nil (caller passes original through).
	if rewriteTarballs([]byte("not json"), base) != nil {
		t.Error("expected nil for non-JSON body")
	}
	// JSON without any tarball URL -> nil (unchanged).
	if rewriteTarballs([]byte(`{"ok":true}`), base) != nil {
		t.Error("expected nil for JSON without dist")
	}
	// Single-version shorthand with top-level dist.
	single := `{"name":"pkg","version":"1.0.0","dist":{"tarball":"https://h/pkg/-/pkg-1.0.0.tgz"}}`
	if out2 := rewriteTarballs([]byte(single), base); out2 != nil {
		var d map[string]any
		json.Unmarshal(out2, &d)
		if d["dist"].(map[string]any)["tarball"].(string) != "http://127.0.0.1:48180/pkg/-/pkg-1.0.0.tgz" {
			t.Error("top-level dist tarball not rewritten")
		}
	} else {
		t.Error("expected rewritten bytes for single-version doc")
	}
}

// --- Forward end-to-end ---

func newRewriteRouter(t *testing.T, backendURL string, enabled bool, external string) *Router {
	t.Helper()
	cfg := &config.Config{
		Rewrite:   config.RewriteConfig{Enabled: enabled, ExternalURL: external},
		Upstreams: []config.Upstream{{Name: "b", URL: backendURL}},
		Routes:    []config.Route{{Prefix: "/", Upstreams: []string{"b"}}},
	}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func TestForwardRewritesMetadataTarballURL(t *testing.T) {
	manifest := `{"name":"pkg","versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"}}}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
		io.WriteString(w, manifest)
	}))
	defer backend.Close()

	r := newRewriteRouter(t, backend.URL, true, "")

	req := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	req.Host = "127.0.0.1:48180"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response not JSON: %v body=%q", err, rec.Body.String())
	}
	tb := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)
	if want := "http://127.0.0.1:48180/pkg/-/pkg-1.0.0.tgz"; tb != want {
		t.Errorf("tarball = %q, want %q", tb, want)
	}
	// Content-Length must be dropped (rewritten body length differs), but the
	// upstream ETag is preserved so downstream clients can issue conditional
	// requests and get 304s instead of re-downloading the manifest.
	if rec.Header().Get("ETag") != `"abc"` {
		t.Errorf("ETag should be preserved for 304 support, got %q", rec.Header().Get("ETag"))
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Errorf("Content-Length should be dropped, got %q", rec.Header().Get("Content-Length"))
	}
}

// TestForwardRewritesGzippedMetadata is the regression test for the bug where
// clients (Verdaccio/npm) send Accept-Encoding: gzip and the upstream returns a
// gzipped manifest: the proxy must still rewrite tarball URLs. Without stripping
// the client's Accept-Encoding, the body arrives still-gzipped, json.Unmarshal
// fails, and the original upstream bytes (unrewritten) pass straight through.
func TestForwardRewritesGzippedMetadata(t *testing.T) {
	manifest := `{"name":"pkg","versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"}}}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte(manifest))
		gz.Close()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", `"abc"`)
		w.Write(buf.Bytes())
	}))
	defer backend.Close()

	r := newRewriteRouter(t, backend.URL, true, "")

	req := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	req.Host = "127.0.0.1:48180"
	req.Header.Set("Accept-Encoding", "gzip") // exactly what Verdaccio/npm send
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The response must be decompressed plain JSON (the proxy stripped the
	// client's Accept-Encoding so Go's transport auto-decompressed).
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding should be removed, got %q", ce)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response not decompressed JSON: %v body=%q", err, rec.Body.String())
	}
	tb := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)
	if want := "http://127.0.0.1:48180/pkg/-/pkg-1.0.0.tgz"; tb != want {
		t.Errorf("tarball = %q, want %q (gzipped metadata not rewritten)", tb, want)
	}
}

// TestForwardMetadataConditional304 verifies that metadata responses support
// conditional requests: the first GET returns a rewritten body with the upstream
// ETag preserved, and a follow-up If-None-Match is forwarded so the upstream can
// answer 304, which the proxy passes through with no body.
func TestForwardMetadataConditional304(t *testing.T) {
	manifest := `{"name":"pkg","versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"}}}}`
	sawIfNoneMatch := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			sawIfNoneMatch <- inm
			w.Header().Set("ETag", `"abc"`)
			w.WriteHeader(http.StatusNotModified) // upstream says: client's copy is fresh
			return
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, manifest)
	}))
	defer backend.Close()

	r := newRewriteRouter(t, backend.URL, true, "")

	// First fetch: rewritten body, ETag preserved so the client can cache it.
	req1 := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	req1.Host = "127.0.0.1:48180"
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first fetch status = %d, want 200", rec1.Code)
	}
	if rec1.Header().Get("ETag") != `"abc"` {
		t.Fatalf("ETag not preserved on rewritten 200: got %q", rec1.Header().Get("ETag"))
	}

	// Second fetch with If-None-Match: proxy forwards it, upstream returns 304,
	// proxy forwards the 304 with ETag and an empty body.
	req2 := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	req2.Host = "127.0.0.1:48180"
	req2.Header.Set("If-None-Match", `"abc"`)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional fetch status = %d, want 304", rec2.Code)
	}
	if rec2.Header().Get("ETag") != `"abc"` {
		t.Errorf("304 missing ETag: got %q", rec2.Header().Get("ETag"))
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 should have an empty body, got %d bytes %q", rec2.Body.Len(), rec2.Body.String())
	}
	select {
	case v := <-sawIfNoneMatch:
		if v != `"abc"` {
			t.Errorf("If-None-Match forwarded as %q, want %q", v, `"abc"`)
		}
	default:
		t.Error("If-None-Match was not forwarded to the upstream")
	}
}

func TestForwardPassesTarballUnchanged(t *testing.T) {
	body := []byte("raw tarball bytes, do not touch")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
	}))
	defer backend.Close()

	r := newRewriteRouter(t, backend.URL, true, "")

	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	req.Host = "127.0.0.1:48180"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Errorf("tarball body changed: got %q want %q", rec.Body.String(), string(body))
	}
}

func TestForwardRewriteDisabled(t *testing.T) {
	manifest := `{"versions":{"1.0.0":{"dist":{"tarball":"https://h/pkg/-/pkg-1.0.0.tgz"}}}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, manifest)
	}))
	defer backend.Close()

	r := newRewriteRouter(t, backend.URL, false, "")

	req := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	req.Host = "127.0.0.1:48180"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	tb := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)
	if tb != "https://h/pkg/-/pkg-1.0.0.tgz" {
		t.Errorf("rewrite disabled but tarball changed: %q", tb)
	}
}
