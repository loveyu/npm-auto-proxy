// Package proxy builds per-upstream transports (with optional HTTP/SOCKS5 proxy
// and fixed-IP resolution) and races HEAD probes across upstreams before
// downloading from the highest-priority healthy one.
package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"npm-auto-proxy/internal/config"
)

// hopByHopHeaders are headers that must not be forwarded between client and upstream.
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Upstream is the runtime form of a configured upstream: its parsed base URL,
// priority, and dedicated connection-pooled clients (optionally proxied and/or
// pinned to a fixed IP).
type Upstream struct {
	cfg           *config.Upstream
	base          *url.URL
	priority      int
	headClient    *http.Client // does not follow redirects
	forwardClient *http.Client // follows redirects; Timeout = download timeout
}

// NewUpstream builds the upstream runtime, including its transport and clients.
// dlTimeout applies to the forward (download) client; 0 means unlimited.
func NewUpstream(cfg *config.Upstream, dlTimeout time.Duration) (*Upstream, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid upstream url %q", cfg.URL)
	}

	tr, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}

	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Upstream{
		cfg:           cfg,
		base:          base,
		priority:      cfg.Priority,
		headClient:    &http.Client{Transport: tr, CheckRedirect: noRedirect},
		forwardClient: &http.Client{Transport: tr, Timeout: dlTimeout},
	}, nil
}

// Name returns the upstream name.
func (u *Upstream) Name() string { return u.cfg.Name }

// Priority returns the upstream priority (higher = preferred).
func (u *Upstream) Priority() int { return u.priority }

// BaseURL returns the parsed upstream base URL.
func (u *Upstream) BaseURL() *url.URL { return u.base }

// ProxyEnabled reports whether this upstream routes through a proxy.
func (u *Upstream) ProxyEnabled() bool { return u.cfg.Proxy.Enabled && u.cfg.Proxy.URL != "" }

// Check performs a GET against the upstream base URL (through the same
// transport, including any proxy) and returns nil if it is reachable.
func (u *Upstream) Check(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base.String(), nil)
	if err != nil {
		return err
	}
	resp, err := u.forwardClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Head issues a non-following HEAD against the upstream for the given relative
// path and returns the HTTP status (and/or an error).
func (u *Upstream) Head(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.targetURL(path), nil)
	if err != nil {
		return 0, err
	}
	req.Host = u.base.Host
	resp, err := u.headClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// Forward streams the request body to the upstream and the upstream response to
// the client. It returns true once the response has been committed (status <
// 400). A connection/protocol error or a >= 400 status returns false without
// writing anything, letting the caller fall back to another upstream.
func (u *Upstream) Forward(ctx context.Context, w http.ResponseWriter, inReq *http.Request, path string) bool {
	outReq := inReq.Clone(ctx)
	outReq.URL.Scheme = u.base.Scheme
	outReq.URL.Host = u.base.Host
	outReq.Host = u.base.Host
	outReq.URL.Path = joinPath(u.base.Path, path)
	outReq.URL.RawPath = ""
	outReq.RequestURI = ""
	dropHopByHop(outReq.Header)

	resp, err := u.forwardClient.Do(outReq)
	if err != nil {
		log.Printf("download [%s %s -> %s]: %v", inReq.Method, inReq.URL.Path, u.Name(), err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("download [%s %s -> %s]: upstream returned HTTP %d", inReq.Method, inReq.URL.Path, u.Name(), resp.StatusCode)
		return false
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("download [%s %s -> %s]: stream interrupted: %v", inReq.Method, inReq.URL.Path, u.Name(), err)
	}
	return true
}

// targetURL builds the absolute upstream URL for a relative request path.
func (u *Upstream) targetURL(path string) string {
	return u.base.Scheme + "://" + u.base.Host + joinPath(u.base.Path, path)
}

func buildTransport(cfg *config.Upstream) (*http.Transport, error) {
	maxIdle := cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 100
	}
	perHost := cfg.IdleConnsPerHost
	if perHost <= 0 {
		perHost = 32
	}

	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdle,
		MaxIdleConnsPerHost:   perHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if cfg.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	// Pin the dial to a fixed IP (keeping the Host header / TLS SNI). Applied
	// before proxy wiring so a proxy (if enabled) takes precedence.
	if cfg.Resolve != "" {
		applyResolve(tr, cfg.Resolve)
	}

	if cfg.Proxy.Enabled && cfg.Proxy.URL != "" {
		if err := applyProxy(tr, cfg.Proxy.URL); err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		log.Printf("upstream %q: proxy enabled (%s)", cfg.Name, schemeOf(cfg.Proxy.URL))
	}
	return tr, nil
}

// applyResolve wraps the transport's DialContext so the upstream host is always
// dialed at the given IP, while the original Host header and TLS SNI are kept.
func applyResolve(tr *http.Transport, ip string) {
	base := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, port, err := net.SplitHostPort(addr); err == nil {
			addr = net.JoinHostPort(ip, port)
		}
		return base(ctx, network, addr)
	}
}

func applyProxy(tr *http.Transport, proxyURL string) error {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy url: %w", err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return fmt.Errorf("socks5: %w", err)
		}
		// proxy.SOCKS5 returns a dialer that also implements proxy.ContextDialer,
		// so cancellations propagate to the SOCKS5 handshake.
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			tr.DialContext = cd.DialContext
		} else {
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	default:
		return fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5 or socks5h)", u.Scheme)
	}
	return nil
}

func schemeOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "?"
	}
	return u.Scheme
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func dropHopByHop(h http.Header) {
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}

// joinPath merges the upstream base path (e.g. "/v1") with the request path,
// guaranteeing the result starts with "/" and has no duplicate slashes.
func joinPath(base, rest string) string {
	if rest == "" || rest == "/" {
		if base == "" {
			return "/"
		}
		return base
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	if base == "" || base == "/" {
		return rest
	}
	return strings.TrimSuffix(base, "/") + rest
}
