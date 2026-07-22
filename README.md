# npm-auto-proxy

High-concurrency HTTP path-forwarding proxy for npm registries, designed to sit in front of [Verdaccio](https://verdaccio.org) as an upstream forwarder. For each request it **races HEAD probes** across multiple upstreams, then **downloads from the highest-priority healthy one**, falling back to the next on failure. Each upstream may use a fixed IP and/or an HTTP/SOCKS5(S) proxy with optional authentication.

## Features

- Concurrent HEAD racing to discover which upstreams currently serve a resource
- Priority-ordered download with automatic fallback on failure
- Grace window: once the first HEAD succeeds, others get a little more time before being ignored
- Retries: if every HEAD times out, the whole race is re-run up to a configured limit
- Per-upstream connection pools (high concurrency, streaming downloads)
- Fixed-IP dialing per upstream (keeps Host header / TLS SNI) — useful for intranet hosts
- Per-upstream optional proxy: `http://`, `https://`, `socks5://`, `socks5h://` (credentials supported)
- Path-prefix routing (longest-prefix match) with optional prefix stripping
- Graceful shutdown, configurable server timeouts

## How it works

For a `GET` request matching a route:

1. **Race HEADs** against all candidate upstreams concurrently.
2. Wait up to `strategy.head.firstTimeout` for the **first** success; once one succeeds, wait up to `strategy.head.grace` longer for the rest, then ignore any still pending.
3. If **no** HEAD succeeds within the window, re-run the race up to `strategy.head.retries` times.
4. Among the HEAD-healthy upstreams, try downloading in **priority order (lowest number first)**. If a download fails (network error or HTTP ≥ 400), fall back to the next healthy upstream.
5. If no upstream is healthy after racing, the request degrades to trying all candidates by priority.

Non-`GET` requests skip the race (the body can be consumed only once) and are forwarded to the single highest-priority candidate.

> `priority` is **lower number = preferred**. If you want a specific upstream first, give it the smallest number.

## Quick Start

```bash
./build.sh v1.0.0
cp config-example.yaml config.yaml   # edit upstreams / routes / proxies
./npm-auto-proxy check               # verify each upstream is reachable
./npm-auto-proxy start
DEBUG=1 ./npm-auto-proxy start       # debug logging
```

## Configuration

See [config-example.yaml](config-example.yaml) for a fully commented example.

```yaml
http:
  addr: ":8080"
  readHeaderTimeout: 10s

strategy:
  head:
    firstTimeout: 30s   # wait for the first HEAD success
    grace: 5s           # extra wait for the rest after the first success
    retries: 2          # re-run the race if every probe times out (total attempts = retries+1)
  download:
    timeout: 0s         # per-upstream download timeout (0 = unlimited)

upstreams:
  - name: intra
    url: http://npm-registry.zuzuche.net
    resolve: 10.2.251.99        # force-dial this IP (keeps Host/SNI)
    priority: 1                 # most preferred (fastest)

  - name: npmmirror
    url: https://registry.npmmirror.com
    priority: 2                 # fast and complete

  - name: npmjs
    url: https://registry.npmjs.org
    priority: 3                 # last resort (proxied)
    proxy:
      enabled: true
      url: socks5://127.0.0.1:7891          # or http://127.0.0.1:7891
      # credentials: socks5://user:pass@127.0.0.1:7891

routes:
  - prefix: /
    upstreams: [intra, npmmirror, npmjs]     # candidates that race; omit to use all
    stripPrefix: false
```

### Upstream fields

| Field | Description |
|-------|-------------|
| `name` | Unique upstream identifier |
| `url` | Base registry URL |
| `priority` | Lower number = tried first among HEAD-healthy upstreams |
| `resolve` | Force-dial this IP for the upstream host (keeps Host/SNI) |
| `maxIdleConns` / `idleConnsPerHost` | Connection-pool sizing (defaults 100 / 32) |
| `insecureSkipVerify` | Skip upstream TLS verification (use with caution) |
| `proxy.enabled` | Route this upstream's traffic through `proxy.url` |
| `proxy.url` | `http://`, `https://`, `socks5://` or `socks5h://` (credentials allowed) |

### Routes

| Field | Description |
|-------|-------------|
| `prefix` | Path prefix to match (must start with `/`) |
| `upstreams` | Candidate upstream names that race; omit to use all upstreams |
| `upstream` | Single candidate (shorthand; populates `upstreams`) |
| `stripPrefix` | Remove `prefix` before forwarding |

Matching is auto-sorted longest-prefix-first.

## CLI Commands

| Command | Description |
|---------|-------------|
| `start` | Start the proxy (default) |
| `check` | Check connectivity to each configured upstream |
| `help` | Show help |
| `download-config` | Download config from `REMOTE_CONFIG_URL` |
| `version` | Show version |

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `config.yaml` |
| `DEBUG` | Enable debug logging | (unset) |
| `REMOTE_CONFIG_URL` | URL for `download-config` | (unset) |

## Building

```bash
./build.sh [version]   # local build
go vet ./...           # lint
go test ./...          # tests (covers routing, racing, fallback, retries)
```

## License

Apache 2.0
