package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"npm-auto-proxy/internal/config"
	"npm-auto-proxy/internal/httpserver"
	"npm-auto-proxy/internal/proxy"
)

var version = "dev"

func main() {
	loadEnv(".env")

	if len(os.Args) < 2 {
		os.Args = append(os.Args, "start")
	}

	switch os.Args[1] {
	case "start":
		runStart()
	case "check":
		runCheck()
	case "help", "--help":
		runHelp()
	case "download-config":
		runDownloadConfig()
	case "version", "--version", "-v":
		fmt.Println("npm-auto-proxy", version)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUse 'help' for usage.\n", os.Args[1])
		os.Exit(1)
	}
}

func runStart() {
	cfgPath := config.ConfigPath()
	debug := config.IsDebug()

	log.Printf("npm-auto-proxy %s starting...", version)
	log.Printf("config: %s", cfgPath)
	if debug {
		log.Printf("debug logging enabled (per-request logs on)")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	router, err := proxy.NewRouter(cfg)
	if err != nil {
		log.Fatalf("build router: %v", err)
	}

	srv := httpserver.New(cfg, router)
	go func() {
		if err := srv.Start(); err != nil {
			log.Fatalf("http server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func runCheck() {
	cfgPath := config.ConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	router, err := proxy.NewRouter(cfg)
	if err != nil {
		log.Fatalf("build router: %v", err)
	}

	var failed int
	for _, u := range router.Upstreams() {
		if err := u.Check(10 * time.Second); err != nil {
			log.Printf("[FAIL] upstream %q (%s): %v", u.Name(), u.BaseURL().String(), err)
			failed++
			continue
		}
		log.Printf("[ OK ] upstream %q (%s)", u.Name(), u.BaseURL().String())
	}

	if failed > 0 {
		os.Exit(1)
	}
}

func runHelp() {
	fmt.Print(`npm-auto-proxy - High-concurrency HTTP path-forwarding proxy for npm registries

Usage:
  npm-auto-proxy [command]

Commands:
  start            Start the proxy service (default)
  check            Check connectivity to each configured upstream (exit 0 on success, 1 on failure)
  help             Show this help message
  download-config  Download remote config from REMOTE_CONFIG_URL
  version          Show version

Environment Variables:
  DEBUG             Enable debug logging
  REMOTE_CONFIG_URL URL for downloading config (used by download-config)
  CONFIG_PATH       Config file path (default: config.yaml)

Routing:
  Incoming paths are matched against route prefixes (longest-prefix first) and
  forwarded to the matching upstream's registry URL. Each upstream may optionally
  route through an HTTP or SOCKS5 proxy.

Cache:
  Optionally cache downloaded tarballs (.tgz/.tar.gz/.zip/...) to disk via the
  cache.directories config (metadata is never cached). Hits are served directly,
  bypassing upstream; concurrent identical requests are de-duplicated per path.

`)
}

func runDownloadConfig() {
	url := config.RemoteConfigURL()
	if url == "" {
		log.Fatal("REMOTE_CONFIG_URL is not set")
	}

	cfgPath := config.ConfigPath()
	log.Printf("downloading config from %s to %s", url, cfgPath)

	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("download config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("download config: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}

	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		log.Fatalf("write config: %v", err)
	}

	log.Printf("config saved to %s (%d bytes)", cfgPath, len(data))
}

// loadEnv reads KEY=VALUE pairs from a .env file. Existing env vars are not overwritten.
// Lines starting with # and empty lines are skipped.
func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // .env file is optional
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, value)
	}
}
