package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"npm-auto-proxy/internal/config"
)

// newCacheRouter wires a single backend upstream with an optional tarball cache
// (readDirs are read-only cache dirs; writeDir, when non-empty, is the write dir).
func newCacheRouter(t *testing.T, backendURL string, readDirs []string, writeDir string) *Router {
	t.Helper()
	cfg := &config.Config{
		Upstreams: []config.Upstream{{Name: "b", URL: backendURL}},
		Routes:    []config.Route{{Prefix: "/", Upstreams: []string{"b"}}},
	}
	for _, p := range readDirs {
		cfg.Cache.Directories = append(cfg.Cache.Directories, config.CacheDirectory{Path: p, Type: "read"})
	}
	if writeDir != "" {
		cfg.Cache.Directories = append(cfg.Cache.Directories, config.CacheDirectory{Path: writeDir, Type: "write"})
	}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func doGetRec(r *Router, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// A tarball download is cached; the next request for the same tarball is served
// from cache and the upstream is not contacted again.
func TestCachePopulatesThenHits(t *testing.T) {
	var downloads int32
	body := []byte("this is a fake tarball payload")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	defer backend.Close()

	writeDir := t.TempDir()
	r := newCacheRouter(t, backend.URL, nil, writeDir)

	// First request: miss -> download + cache.
	rec := doGetRec(r, "/pkg/-/pkg-1.0.0.tgz")
	if rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("first GET: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Fatalf("downloads = %d after first GET, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(writeDir, "pkg/-/pkg-1.0.0.tgz")); err != nil {
		t.Fatalf("tarball not cached: %v", err)
	}

	// Second request: cache hit, upstream untouched.
	rec2 := doGetRec(r, "/pkg/-/pkg-1.0.0.tgz")
	if rec2.Code != http.StatusOK || rec2.Body.String() != string(body) {
		t.Fatalf("second GET: code=%d body=%q", rec2.Code, rec2.Body.String())
	}
	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Errorf("downloads = %d after cached GET, want 1 (should serve from cache)", got)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}
}

// Package metadata is never cached: every request reaches the upstream.
func TestCacheSkipsMetadata(t *testing.T) {
	var hits int32
	manifest := `{"versions":{"1.0.0":{"dist":{"tarball":"https://h/pkg/-/pkg-1.0.0.tgz"}}}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(manifest))
	}))
	defer backend.Close()

	writeDir := t.TempDir()
	r := newCacheRouter(t, backend.URL, nil, writeDir)

	doGetRec(r, "/pkg")
	doGetRec(r, "/pkg")
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("metadata upstream hits = %d, want 2 (metadata must not be cached)", got)
	}
	if _, err := os.Stat(filepath.Join(writeDir, "pkg")); !os.IsNotExist(err) {
		t.Errorf("metadata should not be written to cache, got err=%v", err)
	}
}

// A pre-populated read-only cache directory is consulted and served without any
// upstream traffic.
func TestCacheReadsFromReadOnlyDir(t *testing.T) {
	readDir := t.TempDir()
	rel := "pkg/-/pkg-1.0.0.tgz"
	if err := os.MkdirAll(filepath.Join(readDir, "pkg/-"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readDir, rel), []byte("from-read-dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	var downloads int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	r := newCacheRouter(t, backend.URL, []string{readDir}, t.TempDir())

	rec := doGetRec(r, "/pkg/-/pkg-1.0.0.tgz")
	if rec.Code != http.StatusOK || rec.Body.String() != "from-read-dir" {
		t.Fatalf("GET: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&downloads); got != 0 {
		t.Errorf("downloads = %d, want 0 (should serve from read dir)", got)
	}
	if rec.Header().Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", rec.Header().Get("X-Cache"))
	}
}

// Many concurrent requests for the same uncached tarball trigger exactly one
// upstream download (per-path lock deduplication). Run with -race.
func TestCacheConcurrentSamePathSingleDownload(t *testing.T) {
	var downloads int32
	body := make([]byte, 8192)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&downloads, 1)
		// A little delay widens the concurrency window so followers actually
		// overlap and would race without the per-path lock.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	defer backend.Close()

	r := newCacheRouter(t, backend.URL, nil, t.TempDir())

	const N = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := doGetRec(r, "/pkg/-/pkg-1.0.0.tgz")
			if rec.Code != http.StatusOK {
				t.Errorf("code=%d", rec.Code)
				return
			}
			if rec.Body.Len() != len(body) {
				t.Errorf("body len=%d want %d", rec.Body.Len(), len(body))
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Errorf("downloads = %d, want 1 (per-path lock should dedup concurrent identical requests); %s", got, fmt.Sprintf("N=%d", N))
	}
}
