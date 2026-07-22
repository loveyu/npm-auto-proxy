// Package config loads and validates the YAML configuration for the proxy.
package config

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration loaded from YAML.
type Config struct {
	HTTP      HTTPConfig      `yaml:"http"`
	Strategy  StrategyConfig  `yaml:"strategy"`
	Upstreams []Upstream      `yaml:"upstreams"`
	Routes    []Route         `yaml:"routes"`
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
	Priority           int         `yaml:"priority"`           // Higher = tried first among HEAD-healthy upstreams.
	Resolve            string      `yaml:"resolve"`            // Force-dial this IP for the upstream host (keeps Host/SNI).
	Timeout            string      `yaml:"timeout"`            // Reserved (per-upstream tuning, later stages).
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
	Upstream    string   `yaml:"upstream"`    // Single candidate (convenience; populates Upstreams if empty).
	Upstreams   []string `yaml:"upstreams"`   // Candidate names. If empty along with Upstream, all upstreams are used.
	StripPrefix bool     `yaml:"stripPrefix"`
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

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Longest-prefix-first so a "/" catch-all never shadows a specific route,
	// regardless of the order they were written in the YAML.
	sortRoutesLongestFirst(cfg.Routes)
	return cfg, nil
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
