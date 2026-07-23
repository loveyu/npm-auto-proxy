// Package config loads and validates the YAML configuration for the proxy.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration loaded from YAML.
type Config struct {
	HTTP      HTTPConfig     `yaml:"http"`
	Strategy  StrategyConfig `yaml:"strategy"`
	Rewrite   RewriteConfig  `yaml:"rewrite"`
	Cache     CacheConfig    `yaml:"cache"`
	Upstreams []Upstream     `yaml:"upstreams"`
	Routes    []Route        `yaml:"routes"`
}

// HTTPConfig configures the listening HTTP server.
type HTTPConfig struct {
	Addr              string `yaml:"addr"`
	ReadTimeout       string `yaml:"readTimeout"`
	ReadHeaderTimeout string `yaml:"readHeaderTimeout"`
	WriteTimeout      string `yaml:"writeTimeout"`
	IdleTimeout       string `yaml:"idleTimeout"`
}

// StrategyConfig controls concurrent HEAD racing and the download fallback.
type StrategyConfig struct {
	Head     HeadConfig     `yaml:"head"`
	Download DownloadConfig `yaml:"download"`
}

// HeadConfig controls the concurrent HEAD probe race used to discover which
// upstreams currently serve the requested resource.
type HeadConfig struct {
	FirstTimeout string `yaml:"firstTimeout"` // Max wait for the first HEAD success.
	Grace        string `yaml:"grace"`        // After the first success, extra time to wait for the rest.
	Retries      int    `yaml:"retries"`      // Re-run the whole race this many times if every HEAD times out.
}

// DownloadConfig controls the actual content download.
type DownloadConfig struct {
	Timeout string `yaml:"timeout"` // Per-upstream download timeout (0 = unlimited, recommended for streaming).
}

// Upstream defines a single backend registry that requests can be forwarded to.
type Upstream struct {
	Name               string      `yaml:"name"`
	URL                string      `yaml:"url"`
	Priority           int         `yaml:"priority"` // Lower number = tried first among HEAD-healthy upstreams.
	Resolve            string      `yaml:"resolve"`  // Force-dial this IP for the upstream host (keeps Host/SNI).
	Timeout            string      `yaml:"timeout"`  // Reserved (per-upstream tuning, later stages).
	MaxIdleConns       int         `yaml:"maxIdleConns"`
	IdleConnsPerHost   int         `yaml:"idleConnsPerHost"`
	InsecureSkipVerify bool        `yaml:"insecureSkipVerify"`
	Proxy              ProxyConfig `yaml:"proxy"`
}

// ProxyConfig optionally routes upstream traffic through an HTTP or SOCKS5 proxy.
// Credentials are embedded in the URL for both schemes, e.g.
// socks5://user:pass@host:1080 or http://user:pass@host:8080.
type ProxyConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
}

// Route maps an incoming path prefix to a set of candidate upstreams that race.
type Route struct {
	Prefix      string   `yaml:"prefix"`
	Upstream    string   `yaml:"upstream"`  // Single candidate (convenience; populates Upstreams if empty).
	Upstreams   []string `yaml:"upstreams"` // Candidate names. If empty along with Upstream, all upstreams are used.
	StripPrefix bool     `yaml:"stripPrefix"`
}

// RewriteConfig controls rewriting dist.tarball URLs inside package metadata
// responses so downstream consumers fetch tarballs through this
// proxy. Upstream registries publish absolute tarball URLs (e.g.
// https://registry.example.com/pkg/-/pkg-1.0.0.tgz) that may be unreachable or
// self-referential behind a reverse proxy; rewriting them to point back at this
// proxy makes the tarball downloadable via the same path that already routes
// here. The base URL is derived per request (Host / X-Forwarded-* headers)
// unless ExternalURL is set.
type RewriteConfig struct {
	Enabled     bool   `yaml:"enabled"`     // Master switch (off by default).
	ExternalURL string `yaml:"externalUrl"` // Optional explicit base, e.g. "http://127.0.0.1:48180". Empty = derive per request.
}

// CacheConfig controls the optional on-disk tarball cache. Only compressed
// package artifacts (tarballs: .tgz/.tar.gz/.zip/...) are cached — never package
// metadata. The cache is permanent: there is no TTL, eviction, or size limit.
// Directories are searched for reads in listed order; at most one directory may
// be Type "write" (a write directory also serves reads).
type CacheConfig struct {
	Directories []CacheDirectory `yaml:"directories"`
}

// CacheDirectory is one cache location.
type CacheDirectory struct {
	Path string `yaml:"path"` // required
	Type string `yaml:"type"` // "read" (default) or "write". Write also serves reads.
}

// Load reads, parses, defaults and validates the configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		HTTP: HTTPConfig{
			Addr:              ":8080",
			ReadHeaderTimeout: "10s",
			IdleTimeout:       "120s",
			// Read/Write timeouts default to 0 (unlimited) so large package
			// transfers are never cut mid-stream. Tighten in config if needed.
		},
		Strategy: StrategyConfig{
			Head: HeadConfig{
				FirstTimeout: "30s",
				Grace:        "5s",
				Retries:      2,
			},
			Download: DownloadConfig{Timeout: "0s"},
		},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.HTTP.Addr == "" {
		cfg.HTTP.Addr = ":8080"
	}
	if cfg.Strategy.Head.Retries < 0 {
		cfg.Strategy.Head.Retries = 0
	}

	cfg.normalizeRoutes()
	cfg.normalizeCache()

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Longest-prefix-first so a "/" catch-all never shadows a specific route,
	// regardless of the order they were written in the YAML.
	sortRoutesLongestFirst(cfg.Routes)
	return cfg, nil
}

// normalizeCache trims/defaults cache directory entries: an empty Type defaults
// to "read", and relative paths are resolved to absolute (against the process
// working directory) so cache keys map consistently regardless of CWD.
func (c *Config) normalizeCache() {
	for i := range c.Cache.Directories {
		d := &c.Cache.Directories[i]
		d.Type = strings.ToLower(strings.TrimSpace(d.Type))
		if d.Type == "" {
			d.Type = "read"
		}
		d.Path = strings.TrimSpace(d.Path)
		if d.Path != "" {
			if abs, err := filepath.Abs(d.Path); err == nil {
				d.Path = abs
			}
		}
	}
}

// normalizeRoutes expands each route's candidate list: Upstreams wins, then the
// single Upstream field, finally all configured upstreams.
func (c *Config) normalizeRoutes() {
	all := make([]string, 0, len(c.Upstreams))
	for i := range c.Upstreams {
		all = append(all, c.Upstreams[i].Name)
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if len(r.Upstreams) == 0 {
			switch {
			case r.Upstream != "":
				r.Upstreams = []string{r.Upstream}
			case len(all) > 0:
				r.Upstreams = append([]string(nil), all...)
			}
		}
	}
}

func (c *Config) validate() error {
	if c.Rewrite.Enabled && c.Rewrite.ExternalURL != "" {
		u, err := url.Parse(c.Rewrite.ExternalURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("rewrite.externalUrl %q: must be an absolute URL like http://host:port", c.Rewrite.ExternalURL)
		}
	}
	// Cache directories: each needs a path and a valid type; at most one writer.
	writers := 0
	for i := range c.Cache.Directories {
		d := &c.Cache.Directories[i]
		if d.Path == "" {
			return fmt.Errorf("cache.directories[%d]: path is required", i)
		}
		switch d.Type {
		case "read", "write":
		default:
			return fmt.Errorf("cache.directories[%d]: type %q must be \"read\" or \"write\"", i, d.Type)
		}
		if d.Type == "write" {
			writers++
		}
	}
	if writers > 1 {
		return fmt.Errorf("cache.directories: at most one entry may be type \"write\" (got %d)", writers)
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("no upstreams configured")
	}
	seen := make(map[string]bool, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Name == "" {
			return fmt.Errorf("upstream[%d]: name is required", i)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstream %q: duplicate name", u.Name)
		}
		seen[u.Name] = true
		if u.URL == "" {
			return fmt.Errorf("upstream %q: url is required", u.Name)
		}
		if u.Proxy.Enabled && u.Proxy.URL == "" {
			return fmt.Errorf("upstream %q: proxy is enabled but proxy.url is empty", u.Name)
		}
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("no routes configured")
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Prefix == "" {
			return fmt.Errorf("route[%d]: prefix is required", i)
		}
		if r.Prefix[0] != '/' {
			return fmt.Errorf("route[%d] (%s): prefix must start with '/'", i, r.Prefix)
		}
		if len(r.Upstreams) == 0 {
			return fmt.Errorf("route[%d] (%s): no candidate upstreams", i, r.Prefix)
		}
		for _, name := range r.Upstreams {
			if c.UpstreamByName(name) == nil {
				return fmt.Errorf("route[%d] (%s): upstream %q is not defined", i, r.Prefix, name)
			}
		}
	}
	return nil
}

// UpstreamByName returns the upstream with the given name, or nil.
func (c *Config) UpstreamByName(name string) *Upstream {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return &c.Upstreams[i]
		}
	}
	return nil
}

// HTTPAddr returns the configured listen address.
func (c *Config) HTTPAddr() string { return c.HTTP.Addr }

// RewriteEnabled reports whether package metadata tarball URL rewriting is on.
func (c *Config) RewriteEnabled() bool { return c.Rewrite.Enabled }

// RewriteExternalURL returns the explicit rewrite base URL, or "" to derive per request.
func (c *Config) RewriteExternalURL() string { return c.Rewrite.ExternalURL }

// CacheReadDirs returns every cache directory that can serve reads (all of them,
// in configured order — write directories read as well), or nil if caching is off.
func (c *Config) CacheReadDirs() []string {
	if len(c.Cache.Directories) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.Cache.Directories))
	for _, d := range c.Cache.Directories {
		out = append(out, d.Path)
	}
	return out
}

// CacheWriteDir returns the single write cache directory, or "" if there is none.
func (c *Config) CacheWriteDir() string {
	for _, d := range c.Cache.Directories {
		if d.Type == "write" {
			return d.Path
		}
	}
	return ""
}

func (c *Config) ReadTimeoutDur() time.Duration {
	return parseDurationDefault(c.HTTP.ReadTimeout, 0)
}

func (c *Config) ReadHeaderTimeoutDur() time.Duration {
	return parseDurationDefault(c.HTTP.ReadHeaderTimeout, 10*time.Second)
}

func (c *Config) WriteTimeoutDur() time.Duration {
	return parseDurationDefault(c.HTTP.WriteTimeout, 0)
}

func (c *Config) IdleTimeoutDur() time.Duration {
	return parseDurationDefault(c.HTTP.IdleTimeout, 120*time.Second)
}

// HeadFirstTimeoutDur is the max wait for the first successful HEAD.
func (c *Config) HeadFirstTimeoutDur() time.Duration {
	return parseDurationDefault(c.Strategy.Head.FirstTimeout, 30*time.Second)
}

// HeadGraceDur is the extra wait after the first success for the rest.
func (c *Config) HeadGraceDur() time.Duration {
	return parseDurationDefault(c.Strategy.Head.Grace, 5*time.Second)
}

// HeadRetries is the number of times to re-run the HEAD race if every probe times out.
func (c *Config) HeadRetries() int { return c.Strategy.Head.Retries }

// DownloadTimeoutDur is the per-upstream download timeout (0 = unlimited).
func (c *Config) DownloadTimeoutDur() time.Duration {
	return parseDurationDefault(c.Strategy.Download.Timeout, 0)
}

func sortRoutesLongestFirst(routes []Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		return len(routes[i].Prefix) > len(routes[j].Prefix)
	})
}

func parseDurationDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// ConfigPath returns the config file path from CONFIG_PATH, defaulting to config.yaml.
func ConfigPath() string {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	return "config.yaml"
}

// IsDebug reports whether DEBUG logging is enabled.
func IsDebug() bool { return os.Getenv("DEBUG") != "" }

// RemoteConfigURL returns REMOTE_CONFIG_URL for the download-config command.
func RemoteConfigURL() string { return os.Getenv("REMOTE_CONFIG_URL") }
