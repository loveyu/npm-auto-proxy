package cache

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// teeResponseWriter forwards response bytes to the real client while teeing them
// into a staging file. The staging file is created lazily on the first Write and
// promoted to its final cache location only by finalize(true) once the full body
// has streamed successfully. This keeps Upstream.Forward's "false => zero bytes
// written" contract intact: if the caller returns false without writing, nothing
// is staged and finalize(false) is a cheap no-op.
type teeResponseWriter struct {
	http.ResponseWriter // real client writer; Header/WriteHeader delegate here
	c                   *Cache
	rel                 string // cache-relative target path
	f                   *os.File
	tmp                 string // staging path under c.tmpDir
	written             int64
	writeErr            error
	teeFailed           bool // stop staging after a disk write error
}

func (t *teeResponseWriter) Write(p []byte) (int, error) {
	n, err := t.ResponseWriter.Write(p)
	if n > 0 {
		t.written += int64(n)
		// Tee the bytes that actually went to the client into the staging file.
		if err == nil && !t.teeFailed {
			if t.f == nil {
				if err := t.createStaging(); err != nil {
					t.teeFailed = true // cannot stage; keep serving the client
				}
			}
			if t.f != nil {
				if _, werr := t.f.Write(p[:n]); werr != nil {
					t.teeFailed = true
					t.abort()
					// Serving continues; we just won't cache this response.
				}
			}
		}
	}
	if err != nil {
		t.writeErr = err // client disconnect / write error -> do not commit a partial file
	}
	return n, err
}

// Flush transparently supports http.Flusher if the underlying writer does.
func (t *teeResponseWriter) Flush() {
	if fl, ok := t.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// createStaging opens a fresh temp file in the write directory's .tmp area.
func (t *teeResponseWriter) createStaging() error {
	f, err := os.CreateTemp(t.c.tmpDir, "cap-*")
	if err != nil {
		return err
	}
	t.f = f
	t.tmp = f.Name()
	return nil
}

// finalize commits the staged bytes on success or discards them on failure. It
// is a no-op when nothing was staged.
func (t *teeResponseWriter) finalize(ok bool) {
	if t.f == nil {
		return // nothing was written/staged
	}
	t.f.Close()
	t.f = nil
	if !ok || t.writeErr != nil || t.teeFailed || t.written == 0 || t.truncated() {
		t.abort()
		return
	}
	final := filepath.Join(t.c.writeDir, t.rel)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		log.Printf("cache store %s: mkdir: %v", t.rel, err)
		os.Remove(t.tmp)
		return
	}
	if err := os.Rename(t.tmp, final); err != nil {
		log.Printf("cache store %s: rename: %v", t.rel, err)
		os.Remove(t.tmp)
		return
	}
	t.c.debugf("cache STORE %s (%d bytes)", t.rel, t.written)
}

// truncated reports whether fewer bytes than declared were received. The upstream
// Content-Length header (copied into the response before WriteHeader) is the
// declared length; if it is absent (chunked transfer) the length is unknown and
// truncation cannot be detected, so this returns false. This guards against
// caching a tarball whose upstream stream was cut mid-download — Forward still
// returns true in that case because the status was already committed.
func (t *teeResponseWriter) truncated() bool {
	cl := t.ResponseWriter.Header().Get("Content-Length")
	if cl == "" {
		return false
	}
	want, err := strconv.ParseInt(cl, 10, 64)
	if err != nil {
		return false
	}
	return t.written != want
}

// abort removes the staging file (ignore error; best-effort cleanup).
func (t *teeResponseWriter) abort() {
	if t.tmp != "" {
		os.Remove(t.tmp)
		t.tmp = ""
	}
}
