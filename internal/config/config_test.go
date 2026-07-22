package config

import (
	"os"
	"testing"
	"time"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoadDefaultsAndSorting(t *testing.T) {
	path := writeTempConfig(t, `
http:
  addr: ":9090"
upstreams:
  - name: npmjs
    url: https://registry.npmjs.org
  - name: mirror
    url: https://registry.npmmirror.com
routes:
  - prefix: /
    upstream: mirror
  - prefix: /npmjs/
    upstream: npmjs
    stripPrefix: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("addr = %q, want :9090", cfg.HTTP.Addr)
	}
	if cfg.Routes[0].Prefix != "/npmjs/" {
		t.Errorf("first route = %q, want /npmjs/ (longest first)", cfg.Routes[0].Prefix)
	}
	if cfg.UpstreamByName("npmjs") == nil {
		t.Error("UpstreamByName(npmjs) returned nil")
	}
	if cfg.ReadHeaderTimeoutDur() != 10*time.Second {
		t.Errorf("readHeaderTimeout = %v, want 10s", cfg.ReadHeaderTimeoutDur())
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"no upstreams", `routes: [{prefix: /, upstream: x}]`},
		{"duplicate name", `upstreams: [{name: a, url: https://x}, {name: a, url: https://y}]`},
		{"missing url", `upstreams: [{name: a}]`},
		{"unknown upstream", "upstreams: [{name: a, url: https://x}]\nroutes: [{prefix: /, upstream: b}]"},
		{"proxy without url", `upstreams: [{name: a, url: https://x, proxy: {enabled: true}}]`},
		{"bad prefix", "upstreams: [{name: a, url: https://x}]\nroutes: [{prefix: foo, upstream: a}]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempConfig(t, c.yaml)
			if _, err := Load(path); err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
}
