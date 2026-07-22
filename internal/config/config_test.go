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

func TestRewriteConfigValidation(t *testing.T) {
	base := "upstreams: [{name: a, url: https://x}]\nroutes: [{prefix: /, upstream: a}]\n"
	cases := []struct {
		name string
		yaml string
		ok   bool
	}{
		{"rewrite off", base, true},
		{"rewrite on no external", "rewrite: {enabled: true}\n" + base, true},
		{"rewrite on valid external", "rewrite: {enabled: true, externalUrl: http://127.0.0.1:48180}\n" + base, true},
		{"rewrite on non-absolute external", "rewrite: {enabled: true, externalUrl: not-a-url}\n" + base, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempConfig(t, c.yaml)
			cfg, err := Load(path)
			if c.ok {
				if err != nil {
					t.Fatalf("expected ok, got err: %v", err)
				}
				if cfg.RewriteEnabled() != (c.name != "rewrite off") {
					t.Errorf("RewriteEnabled mismatch")
				}
			} else if err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestCacheConfigValidation(t *testing.T) {
	base := "upstreams: [{name: a, url: https://x}]\nroutes: [{prefix: /, upstream: a}]\n"
	cases := []struct {
		name   string
		yaml   string
		ok     bool
		writeN int // expected: 0 = no write dir, 1 = one write dir
		readN  int // expected total read dirs
	}{
		{"no cache", base, true, 0, 0},
		{"one read", "cache:\n  directories:\n    - path: /tmp/c1\n" + base, true, 0, 1},
		{"explicit read type", "cache:\n  directories:\n    - path: /tmp/c1\n      type: read\n" + base, true, 0, 1},
		{"one write", "cache:\n  directories:\n    - path: /tmp/c1\n      type: write\n" + base, true, 1, 1},
		{"read plus write", "cache:\n  directories:\n    - {path: /tmp/c1}\n    - {path: /tmp/c2, type: write}\n" + base, true, 1, 2},
		{"two writes", "cache:\n  directories:\n    - {path: /tmp/c1, type: write}\n    - {path: /tmp/c2, type: write}\n" + base, false, 0, 0},
		{"bad type", "cache:\n  directories:\n    - {path: /tmp/c1, type: sync}\n" + base, false, 0, 0},
		{"empty path", "cache:\n  directories:\n    - {type: read}\n" + base, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempConfig(t, c.yaml)
			cfg, err := Load(path)
			if c.ok {
				if err != nil {
					t.Fatalf("expected ok, got err: %v", err)
				}
				if got := len(cfg.CacheReadDirs()); got != c.readN {
					t.Errorf("CacheReadDirs len = %d, want %d", got, c.readN)
				}
				w := cfg.CacheWriteDir()
				switch {
				case c.writeN == 0 && w != "":
					t.Errorf("CacheWriteDir = %q, want empty", w)
				case c.writeN == 1 && w == "":
					t.Error("CacheWriteDir empty, want a write dir")
				}
			} else if err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}
