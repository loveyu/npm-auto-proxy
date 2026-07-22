// Package httpserver wraps net/http with configurable timeouts and graceful shutdown.
package httpserver

import (
	"context"
	"log"
	"net/http"

	"npm-auto-proxy/internal/config"
	"npm-auto-proxy/internal/proxy"
)

// Server is the HTTP front-end that dispatches requests to the proxy router.
type Server struct {
	cfg    *config.Config
	router *proxy.Router
	server *http.Server
}

// New creates a server bound to the given config and router.
func New(cfg *config.Config, router *proxy.Router) *Server {
	return &Server{cfg: cfg, router: router}
}

// Start listens and serves until Shutdown is called. It blocks the caller.
func (s *Server) Start() error {
	addr := s.cfg.HTTPAddr()
	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeoutDur(),
		ReadTimeout:       s.cfg.ReadTimeoutDur(),
		WriteTimeout:      s.cfg.WriteTimeoutDur(),
		IdleTimeout:       s.cfg.IdleTimeoutDur(),
	}
	log.Printf("http: listening on %s", addr)
	err := s.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server, waiting for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}
