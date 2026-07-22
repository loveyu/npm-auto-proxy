// Package cache implements an optional on-disk cache for downloaded tarballs.
//
// Only compressed package artifacts (tarballs: .tgz/.tar.gz/.zip/...) are cached;
// package metadata is never cached. The cache is keyed by request path and is
// permanent (no TTL, eviction, or size limit). Reads consult every configured
// directory in order; writes go to the single write directory.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// tarballSuffixes are the response path suffixes eligible for caching. A path
// matching any of these is treated as a downloadable compressed artifact; the
// package-metadata path shapes never carry these suffixes, so metadata is
// excluded by construction.
var tarballSuffixes = []string{
	".tgz", ".tar.gz", ".tar", ".gz", ".zip",
	".tar.bz2", ".tar.xz", ".bz2", ".xz",
}

// Cache is the runtime tarball cache. readDirs are searched in order on every
// lookup; writeDir (possibly "") is the only directory new tarballs are stored
// to. A write directory is also implicitly a read directory (it must appear in
// readDirs when configured).
type Cache struct {
	readDirs []string
	writeDir string
	tmpDir   string // writeDir + "/.tmp"; "" when there is no write dir
	debug    bool

	fmu     sync.Mutex              // guards flights
	flights map[string]*flightEntry // per-path download-serialization state
}

// flightEntry is the per-cache-key dedup state: a mutex serializing concurrent
// downloads of the same path, plus a reference count so the entry is removed from
// the flights map once the last interested goroutine releases it (no unbounded
// growth).
type flightEntry struct {
	mu   sync.Mutex
	refs int
}

// New builds a cache. readDirs are the directories consulted for reads (in
// order); writeDir is the single directory tarballs are written to ("" disables
// writing). The write directory and its .tmp staging area are created if
// missing; stale staging files from a previous crashed run are removed. Missing
// read directories are tolerated (skipped at lookup time).
func New(readDirs []string, writeDir string, debug bool) (*Cache, error) {
	c := &Cache{
		readDirs: readDirs,
		writeDir: writeDir,
		debug:    debug,
	}
	if writeDir != "" {
		if err := os.MkdirAll(writeDir, 0o755); err != nil {
			return nil, err
		}
		c.tmpDir = filepath.Join(writeDir, ".tmp")
		if err := os.MkdirAll(c.tmpDir, 0o755); err != nil {
			return nil, err
		}
		c.cleanupStaleTmp()
	}
	return c, nil
}

// cleanupStaleTmp removes any files left in the staging directory by a previous
// process that died mid-download. They are never served (reads use request paths,
// not the random staging names) but would otherwise accumulate.
func (c *Cache) cleanupStaleTmp() {
	entries, err := os.ReadDir(c.tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(c.tmpDir, e.Name()))
	}
}

// ServeHit serves the requested tarball from cache if it is present in any read
// directory. It returns true when a response was written (cache hit, fully
// bypassing upstream) and false when nothing was found (caller proceeds to
// upstream). Only GET requests for tarball-shaped paths are considered.
func (c *Cache) ServeHit(w http.ResponseWriter, req *http.Request) bool {
	if c == nil {
		return false
	}
	rel, ok := c.key(req)
	if !ok {
		return false
	}
	for _, dir := range c.readDirs {
		path := filepath.Join(dir, rel)
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue // race with a concurrent delete/rewrite; try the next dir
		}
		defer f.Close()
		c.debugf("cache HIT %s -> %s", req.URL.Path, path)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, f); err != nil {
			// Headers are already committed; the client gets a truncated body.
			// Nothing else to do but log.
			log.Printf("cache serve %s: write interrupted: %v", req.URL.Path, err)
		}
		return true
	}
	c.debugf("cache miss %s", req.URL.Path)
	return false
}

// Acquire returns a per-path mutex and a release callback that together
// serialize concurrent cacheable downloads of the same request path, so that at
// most one upstream fetch runs at a time per path: the first request downloads
// and caches the tarball; concurrent followers wait, then re-check the cache
// (serving the just-cached file) inside their own critical section. It returns
// (nil, nil) when caching is disabled or the request is not a cacheable GET, in
// which case the caller proceeds without locking.
//
// The caller MUST Lock the returned mutex before downloading and call release
// exactly once when finished (typically via defer). release is safe to call
// regardless of whether the download succeeded.
func (c *Cache) Acquire(req *http.Request) (*sync.Mutex, func()) {
	if c == nil {
		return nil, nil
	}
	key, ok := c.key(req)
	if !ok {
		return nil, nil
	}
	c.fmu.Lock()
	if c.flights == nil {
		c.flights = make(map[string]*flightEntry)
	}
	e, ok := c.flights[key]
	if !ok {
		e = &flightEntry{}
		c.flights[key] = e
	}
	e.refs++
	c.fmu.Unlock()
	return &e.mu, func() {
		c.fmu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(c.flights, key)
		}
		c.fmu.Unlock()
	}
}

// Capture wraps w so that bytes written for a tarball download are teed into a
// staging file and committed to the cache once the download fully succeeds. It
// returns the writer to forward to and a finalize closure; calling finalize(true)
// commits the captured bytes, finalize(false) discards them. When caching is
// disabled, the path is not a cachable tarball, or there is no write directory,
// it returns the original w and a nil closure (zero overhead).
//
// The wrapper preserves Upstream.Forward's "false => zero bytes written"
// contract: if the caller never writes, nothing is staged, and finalize(false)
// is a no-op.
func (c *Cache) Capture(w http.ResponseWriter, req *http.Request) (http.ResponseWriter, func(ok bool)) {
	if c == nil || c.writeDir == "" {
		return w, nil
	}
	rel, ok := c.key(req)
	if !ok {
		return w, nil
	}
	tw := &teeResponseWriter{ResponseWriter: w, c: c, rel: rel}
	return tw, tw.finalize
}

// key derives the cache-relative path for a request and reports whether the
// request is eligible for caching (GET + tarball-shaped path + no path
// traversal). The relative path mirrors the request path (with a query hash
// appended when a query string is present, so query-signed tarballs do not
// collide).
func (c *Cache) key(req *http.Request) (string, bool) {
	if req.Method != http.MethodGet {
		return "", false
	}
	if !isCachablePath(req.URL.Path) {
		return "", false
	}
	return cacheRelPath(req.URL.Path, req.URL.RawQuery)
}

// isCachablePath reports whether path ends with a known compressed-artifact
// suffix.
func isCachablePath(path string) bool {
	low := strings.ToLower(path)
	for _, suf := range tarballSuffixes {
		if strings.HasSuffix(low, suf) {
			return true
		}
	}
	return false
}

// cacheRelPath converts a request path (plus optional query) into a path safe to
// join under a cache directory root. It rejects any ".." segment (path
// traversal) and returns ok=false in that case. A non-empty query is folded into
// a short hash appended to the final segment.
func cacheRelPath(urlPath, rawQuery string) (string, bool) {
	p := urlPath
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", false
	}
	if rawQuery != "" {
		sum := sha256.Sum256([]byte(rawQuery))
		p = p + "_" + hex.EncodeToString(sum[:8])
	}
	return p, true
}

func (c *Cache) debugf(format string, args ...any) {
	if c.debug {
		log.Printf("[debug] "+format, args...)
	}
}
