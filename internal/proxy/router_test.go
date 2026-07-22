package proxy

import (
	"net/http"
	"net/http/httptest"
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
