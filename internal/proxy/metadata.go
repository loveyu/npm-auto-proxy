package proxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// maxMetadataBytes bounds how much of a metadata response is buffered for
// rewriting. Package manifests are small (typically < 1 MB); this only guards
// against a pathological upstream. Tarball downloads stream through untouched.
const maxMetadataBytes = 64 << 20 // 64 MB

// rewriteBaseURL returns the base URL (scheme://host[:port]) that rewritten
// tarball URLs should point at. Priority:
//  1. configured (explicit rewrite.externalUrl), if non-empty
//  2. X-Forwarded-Proto + X-Forwarded-Host (when behind a reverse proxy)
//  3. the request's own scheme and Host header
//
// This makes generated tarball URLs reachable whether a downstream cache hits
// the proxy directly (Host = 127.0.0.1:48180) or through an nginx layer that
// forwards the original client Host/Proto. X-Forwarded-* chains are honored by
// taking the first (left-most, original-client) value.
func rewriteBaseURL(r *http.Request, configured string) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	scheme := "http"
	if proto := firstForwarded(r, "X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if fh := firstForwarded(r, "X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host
}

// firstForwarded returns the first (left-most) value of a possibly comma-separated
// forwarded header, trimmed of surrounding whitespace.
func firstForwarded(r *http.Request, key string) string {
	v := r.Header.Get(key)
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// isPackageMetadataPath reports whether path targets a package metadata
// document (the version manifest JSON), as opposed to a tarball download or a
// registry special endpoint. Heuristic: tarballs and /-/ endpoints contain
// "/-/" (e.g. /pkg/-/pkg-1.0.0.tgz, /-/v1/search, /-/ping); metadata paths
// (e.g. /pkg, /@scope/pkg, /pkg/1.0.0) do not.
func isPackageMetadataPath(path string) bool {
	if path == "" || path == "/" {
		return false
	}
	return !strings.Contains(path, "/-/")
}

// rewriteTarballs rewrites every dist.tarball URL in a package metadata JSON
// document so it points at baseURL (keeping each tarball's own path). It returns
// the rewritten bytes, or nil if body is not a metadata document containing any
// tarball URL — in which case the caller must pass the original body through
// unchanged.
func rewriteTarballs(body []byte, baseURL string) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil // not JSON; leave untouched
	}
	changed := false

	// Full manifest: versions.<ver>.dist.tarball
	if versions, ok := doc["versions"].(map[string]any); ok {
		for _, v := range versions {
			if ver, ok := v.(map[string]any); ok {
				if rewriteDist(ver["dist"], baseURL) {
					changed = true
				}
			}
		}
	}

	// Single-version shorthand: top-level dist.tarball
	if rewriteDist(doc["dist"], baseURL) {
		changed = true
	}

	if !changed {
		return nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	return out
}

// rewriteDist rewrites the tarball URL inside a dist object and reports whether
// a change was made.
func rewriteDist(dist any, baseURL string) bool {
	d, ok := dist.(map[string]any)
	if !ok {
		return false
	}
	tb, ok := d["tarball"].(string)
	if !ok || tb == "" {
		return false
	}
	if rewritten := rewriteTarballURL(tb, baseURL); rewritten != tb {
		d["tarball"] = rewritten
		return true
	}
	return false
}

// rewriteTarballURL returns orig with its scheme://host[:port] replaced by
// baseURL, preserving the original path and query. If either side fails to
// parse, orig is returned unchanged.
func rewriteTarballURL(orig, baseURL string) string {
	u, err := url.Parse(orig)
	if err != nil || u.Path == "" {
		return orig
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return orig
	}
	u.Scheme = base.Scheme
	u.Host = base.Host
	return u.String()
}
