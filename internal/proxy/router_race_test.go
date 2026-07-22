package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"npm-auto-proxy/internal/config"
)

func strat(first, grace string, retries int) config.StrategyConfig {
	return config.StrategyConfig{
		Head:     config.HeadConfig{FirstTimeout: first, Grace: grace, Retries: retries},
		Download: config.DownloadConfig{Timeout: "0s"},
	}
}

// backend returns headStatus for HEAD and getStatus+body for other methods.
func backend(headStatus, getStatus int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(headStatus)
			return
		}
		if getStatus != 0 {
			w.WriteHeader(getStatus)
		}
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
}

func doGet(t *testing.T, r *Router, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func twoUpstreamRouter(srvA, srvB *httptest.Server, prioA, prioB int, s config.StrategyConfig) (*Router, error) {
	cfg := &config.Config{
		Strategy: s,
		Upstreams: []config.Upstream{
			{Name: "a", URL: srvA.URL, Priority: prioA},
			{Name: "b", URL: srvB.URL, Priority: prioB},
		},
		Routes: []config.Route{{Prefix: "/", Upstreams: []string{"a", "b"}, StripPrefix: false}},
	}
	return NewRouter(cfg)
}

// Unhealthy high-priority upstream must be skipped in favor of a healthy one.
func TestRaceSkipsUnhealthy(t *testing.T) {
	srvA := backend(http.StatusNotFound, http.StatusOK, "A") // HEAD 404 (unhealthy)
	defer srvA.Close()
	srvB := backend(http.StatusOK, http.StatusOK, "B") // HEAD 200 (healthy)
	defer srvB.Close()

	r, err := twoUpstreamRouter(srvA, srvB, 90, 10, strat("200ms", "50ms", 0))
	if err != nil {
		t.Fatal(err)
	}
	code, body := doGet(t, r, "/pkg")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body != "B" {
		t.Errorf("body = %q, want %q (unhealthy A must be skipped)", body, "B")
	}
}

// A healthy download failure (>=400) falls back to the next upstream.
func TestDownloadFallback(t *testing.T) {
	srvA := backend(http.StatusOK, http.StatusInternalServerError, "") // healthy HEAD, 500 GET
	defer srvA.Close()
	srvB := backend(http.StatusOK, http.StatusOK, "B")
	defer srvB.Close()

	r, err := twoUpstreamRouter(srvA, srvB, 90, 10, strat("200ms", "50ms", 0))
	if err != nil {
		t.Fatal(err)
	}
	code, body := doGet(t, r, "/pkg")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body != "B" {
		t.Errorf("body = %q, want %q (should fall back from failing A)", body, "B")
	}
}

// When all HEAD probes are unhealthy, the race exhausts retries and the request
// degrades to downloading from candidates by priority (highest first).
func TestHeadAllUnhealthyDegradesByPriority(t *testing.T) {
	srvA := backend(http.StatusNotFound, http.StatusOK, "A") // HEAD 404
	defer srvA.Close()
	srvB := backend(http.StatusNotFound, http.StatusOK, "B") // HEAD 404
	defer srvB.Close()

	r, err := twoUpstreamRouter(srvA, srvB, 90, 10, strat("40ms", "20ms", 1))
	if err != nil {
		t.Fatal(err)
	}
	code, body := doGet(t, r, "/pkg")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body != "A" {
		t.Errorf("body = %q, want %q (degraded download should pick highest priority)", body, "A")
	}
}

// With multiple healthy upstreams, the highest priority one is downloaded.
func TestRacePicksHighestPriorityHealthy(t *testing.T) {
	srvA := backend(http.StatusOK, http.StatusOK, "A")
	defer srvA.Close()
	srvB := backend(http.StatusOK, http.StatusOK, "B")
	defer srvB.Close()

	r, err := twoUpstreamRouter(srvA, srvB, 90, 10, strat("200ms", "50ms", 0))
	if err != nil {
		t.Fatal(err)
	}
	code, body := doGet(t, r, "/pkg")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body != "A" {
		t.Errorf("body = %q, want %q (highest-priority healthy upstream)", body, "A")
	}
}
