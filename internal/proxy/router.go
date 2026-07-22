package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"npm-auto-proxy/internal/config"
)

// Router matches incoming request paths against configured routes (longest
// prefix first) and, for each match, races HEAD probes across the route's
// candidate upstreams and downloads from the highest-priority healthy one.
type Router struct {
	routes      []compiledRoute
	headFirst   time.Duration // max wait for the first HEAD success
	headGrace   time.Duration // extra wait after the first success
	headRetries int           // re-run the whole race if every probe times out
	debug       bool          // emit per-request debug logs
}

// requestInfo carries per-request tracing data (the upstream that served the
// request, or a failure hint) so ServeHTTP can emit a single completion line.
type requestInfo struct {
	upstream string
}

type compiledRoute struct {
	prefix      string
	stripPrefix bool
	upstreams   []*Upstream
}

// NewRouter compiles config routes and builds the per-upstream clients.
func NewRouter(cfg *config.Config) (*Router, error) {
	dlTimeout := cfg.DownloadTimeoutDur()

	built := make(map[string]*Upstream, len(cfg.Upstreams))
	for i := range cfg.Upstreams {
		u, err := NewUpstream(&cfg.Upstreams[i], dlTimeout)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: %w", cfg.Upstreams[i].Name, err)
		}
		built[u.Name()] = u
		log.Printf("upstream %q -> %s (priority=%d)", u.Name(), u.BaseURL().String(), u.Priority())
	}

	routes := make([]compiledRoute, 0, len(cfg.Routes))
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		ups := make([]*Upstream, 0, len(r.Upstreams))
		for _, name := range r.Upstreams {
			ups = append(ups, built[name])
		}
		routes = append(routes, compiledRoute{
			prefix:      r.Prefix,
			stripPrefix: r.StripPrefix,
			upstreams:   ups,
		})
		log.Printf("route %s -> %v (stripPrefix=%v)", r.Prefix, r.Upstreams, r.StripPrefix)
	}

	return &Router{
		routes:      routes,
		headFirst:   cfg.HeadFirstTimeoutDur(),
		headGrace:   cfg.HeadGraceDur(),
		headRetries: cfg.HeadRetries(),
		debug:       config.IsDebug(),
	}, nil
}

// Upstreams returns the upstreams referenced by any route (sorted by name) for the check command.
func (r *Router) Upstreams() []*Upstream {
	seen := make(map[string]bool)
	out := make([]*Upstream, 0)
	for _, cr := range r.routes {
		for _, u := range cr.upstreams {
			if !seen[u.Name()] {
				seen[u.Name()] = true
				out = append(out, u)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ServeHTTP dispatches the request to the first matching route's strategy.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	for _, cr := range r.routes {
		if strings.HasPrefix(req.URL.Path, cr.prefix) {
			r.debugf(">> %s %s: route %q (candidates=%v, stripPrefix=%v)",
				req.Method, req.URL.Path, cr.prefix, upstreamNames(cr.upstreams), cr.stripPrefix)
			info := &requestInfo{}
			cr.serve(w, req, r, info)
			r.debugf("<< %s %s: %s in %s", req.Method, req.URL.Path, info.upstream, time.Since(start))
			return
		}
	}
	r.debugf(">> %s %s: no matching route -> 404", req.Method, req.URL.Path)
	http.NotFound(w, req)
}

// serve applies the racing/fallback strategy for this route.
func (cr *compiledRoute) serve(w http.ResponseWriter, req *http.Request, r *Router, info *requestInfo) {
	path := cr.strip(req.URL.Path)
	candidates := cr.upstreams

	// Single candidate: no race needed.
	if len(candidates) == 1 {
		if candidates[0].Forward(req.Context(), w, req, path) {
			info.upstream = "served by " + candidates[0].Name()
			return
		}
		info.upstream = "502 upstream " + candidates[0].Name() + " unreachable"
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}

	// GET benefits from a HEAD race before downloading. Healthy upstreams are
	// tried in priority order; a download failure falls back to the next.
	if req.Method == http.MethodGet {
		alive := r.raceHead(req, cr)
		r.debugf("   %s %s: head race healthy=%v", req.Method, req.URL.Path, upstreamNames(alive))
		if len(alive) == 0 {
			alive = sortByPriority(candidates) // race fully failed: best-effort by priority
		}
		for _, u := range alive {
			if u.Forward(req.Context(), w, req, path) {
				info.upstream = "served by " + u.Name()
				return
			}
			r.debugf("   %s %s: upstream %q failed, falling back", req.Method, req.URL.Path, u.Name())
		}
		info.upstream = "502 all upstreams failed"
		http.Error(w, "all upstreams failed", http.StatusBadGateway)
		return
	}

	// Non-GET: the request body can be consumed only once, so try just the
	// highest-priority candidate without racing or fallback.
	ordered := sortByPriority(candidates)
	if ordered[0].Forward(req.Context(), w, req, path) {
		info.upstream = "served by " + ordered[0].Name()
		return
	}
	info.upstream = "502 upstream " + ordered[0].Name() + " failed"
	http.Error(w, "upstream failed", http.StatusBadGateway)
}

func (cr *compiledRoute) strip(path string) string {
	if !cr.stripPrefix {
		return path
	}
	return strings.TrimPrefix(path, cr.prefix)
}

// raceHead concurrently probes candidates with HEAD and returns the ones that
// reported the resource available, ordered by priority (highest first). If every
// probe times out, the race is retried up to r.headRetries times.
func (r *Router) raceHead(req *http.Request, cr *compiledRoute) []*Upstream {
	path := cr.strip(req.URL.Path)
	for attempt := 0; attempt <= r.headRetries; attempt++ {
		healthy := r.raceHeadOnce(req.Context(), path, cr.upstreams)
		if len(healthy) > 0 {
			return sortByPriority(healthy)
		}
		if attempt < r.headRetries {
			log.Printf("head race %s: no healthy upstream after %d probes, retrying (%d/%d)",
				req.URL.Path, len(cr.upstreams), attempt+1, r.headRetries)
		}
	}
	return nil
}

type headResult struct {
	u      *Upstream
	status int
	err    error
}

// raceHeadOnce runs a single HEAD race: wait up to headFirst for the first
// success, then up to headGrace longer for the rest. Already-arrived healthy
// results at the deadline are included.
func (r *Router) raceHeadOnce(parent context.Context, path string, candidates []*Upstream) []*Upstream {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	ch := make(chan headResult, len(candidates))
	for _, u := range candidates {
		go func(u *Upstream) {
			status, err := u.Head(ctx, path)
			if err != nil {
				r.debugf("   head %s %s: %v", path, u.Name(), err)
			} else {
				r.debugf("   head %s %s: HTTP %d", path, u.Name(), status)
			}
			ch <- headResult{u, status, err}
		}(u)
	}

	timer := time.NewTimer(r.headFirst)
	defer timer.Stop()

	var healthy []*Upstream
	pending := len(candidates)
	firstDone := false

	for pending > 0 {
		select {
		case res := <-ch:
			pending--
			if res.err == nil && res.status >= 200 && res.status < 400 {
				healthy = append(healthy, res.u)
				if !firstDone {
					firstDone = true
					resetTimer(timer, r.headGrace)
				}
			}
		case <-timer.C:
			collectHealthy(ch, &healthy)
			return healthy
		}
	}
	return healthy
}

// sortByPriority returns a copy ordered by priority, lowest first (lower number
// = preferred; stable for ties).
func sortByPriority(upstreams []*Upstream) []*Upstream {
	out := append([]*Upstream(nil), upstreams...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority() < out[j].Priority()
	})
	return out
}

// resetTimer stops t (draining a pending fire) and resets it to d.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// collectHealthy non-blockingly drains any already-arrived healthy results.
func collectHealthy(ch <-chan headResult, healthy *[]*Upstream) {
	for {
		select {
		case res := <-ch:
			if res.err == nil && res.status >= 200 && res.status < 400 {
				*healthy = append(*healthy, res.u)
			}
		default:
			return
		}
	}
}

// debugf emits a [debug]-prefixed log line only when debug mode is enabled.
func (r *Router) debugf(format string, args ...any) {
	if r.debug {
		log.Printf("[debug] "+format, args...)
	}
}

// upstreamNames returns the names of the given upstreams, in order.
func upstreamNames(us []*Upstream) []string {
	names := make([]string, len(us))
	for i, u := range us {
		names[i] = u.Name()
	}
	return names
}
