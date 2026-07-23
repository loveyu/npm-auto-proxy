package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"npm-auto-proxy/internal/config"
)

func TestJoinPath(t *testing.T) {
	cases := []struct {
		base, rest, want string
	}{
		{"", "/lodash", "/lodash"},
		{"/", "/lodash", "/lodash"},
		{"/v1", "/lodash", "/v1/lodash"},
		{"/v1/", "/lodash", "/v1/lodash"},
		{"/v1", "lodash", "/v1/lodash"},
		{"/v1", "/", "/v1"},
		{"", "/", "/"},
	}
	for _, c := range cases {
		if got := joinPath(c.base, c.rest); got != c.want {
			t.Errorf("joinPath(%q,%q) = %q, want %q", c.base, c.rest, got, c.want)
		}
	}
}

func TestSingleCandidateForwardAndStripPrefix(t *testing.T) {
	var seen []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{{Name: "b", URL: backend.URL}},
		// Routes already longest-prefix-first.
		Routes: []config.Route{
			{Prefix: "/npmjs/", Upstreams: []string{"b"}, StripPrefix: true},
			{Prefix: "/", Upstreams: []string{"b"}, StripPrefix: false},
		},
	}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/npmjs/lodash", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(seen) != 1 || seen[0] != "/lodash" {
		t.Errorf("backend path = %v, want [/lodash]", seen)
	}

	seen = nil
	req = httptest.NewRequest(http.MethodGet, "/express", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if len(seen) != 1 || seen[0] != "/express" {
		t.Errorf("backend path = %v, want [/express]", seen)
	}
}

func TestNewRouterBadProxyScheme(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name:  "b",
			URL:   "https://example.com",
			Proxy: config.ProxyConfig{Enabled: true, URL: "ftp://host:21"},
		}},
		Routes: []config.Route{{Prefix: "/", Upstreams: []string{"b"}}},
	}
	if _, err := NewRouter(cfg); err == nil {
		t.Fatal("expected error for unsupported proxy scheme, got nil")
	}
}

// TestGetRelaysDefinitive4xxWhenAllUpstreamsFail covers the npm attestations
// case: a package version with no provenance gets a unanimous 404 from every
// upstream. That is a definitive answer the client must see as 404, not a
// gateway 502 (which npm/pnpm treat as a hard install error).
func TestGetRelaysDefinitive4xxWhenAllUpstreamsFail(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"File not found"}`))
	}))
	defer backend.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "a", URL: backend.URL, Priority: 1},
			{Name: "b", URL: backend.URL, Priority: 2},
		},
		Routes: []config.Route{{Prefix: "/", Upstreams: []string{"a", "b"}}},
	}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/-/npm/v1/attestations/@scope/pkg@1.0.0", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want relayed 404; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "File not found") {
		t.Errorf("body = %q, want the relayed upstream body", rec.Body.String())
	}
}

// TestGetReturns502OnlyForTransportErrors ensures a synthetic 502 is still used
// when every candidate is genuinely unreachable (no definitive status at all),
// so the 404-relay path doesn't mask real outages.
func TestGetReturns502OnlyForTransportErrors(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // port now refuses connections -> transport error, status 0

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "a", URL: dead.URL, Priority: 1},
			{Name: "b", URL: dead.URL, Priority: 2},
		},
		Routes: []config.Route{{Prefix: "/", Upstreams: []string{"a", "b"}}},
	}
	r, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/-/npm/v1/attestations/x@1.0.0", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for all-unreachable upstreams", rec.Code)
	}
}
