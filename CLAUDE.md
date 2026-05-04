# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

mc-router is a Minecraft Java Edition reverse proxy that routes client connections to backend servers based on the requested server address (hostname). It multiplexes multiple Minecraft servers onto a single public IP/port and supports auto-discovery via Kubernetes and Docker, auto-scaling (scale to zero/one), webhooks, rate limiting, IP filtering, PROXY protocol, and ngrok tunneling.

## Build & Test Commands

```bash
go build ./cmd/mc-router/       # Build the binary
make test                        # Run all tests (go test ./...)
go test ./server/...             # Run only server package tests
go test ./mcproto/...            # Run only protocol package tests
go test -run TestRouteLookup ./server/  # Run a single test
docker build -t mc-router .      # Build Docker image
```

Go version: 1.26.2. Testing uses `testify` (assert/require). Tests are table-driven with subtests. Mock pattern: embed `mock.Mock` and call `m.MethodCalled()` (see `k8s_test.go`). Protocol packet tests use hex fixture files in `testdata/` (e.g., `handshake-status.hex`). Test setup for route tests calls `NewRoutes()` and restores the global `Routes` singleton with defer.

## Architecture

### Request Flow

1. **Connector** (`server/connector.go`) accepts TCP connections on port 25565
2. **mcproto** package (`mcproto/`) reads the Minecraft handshake packet to extract the target server address
3. **Routes** (`server/routes.go`) looks up the backend address for that hostname
4. If auto-scale is enabled and the backend is sleeping, a **waker** function starts it (Kubernetes StatefulSet replica 0→1 or Docker container start/unpause)
5. Traffic is proxied bidirectionally between client and backend
6. On disconnect, metrics are updated, webhooks fired, and the **DownScaler** (`server/down_scaler.go`) may schedule scale-down after idle timeout

### Key Packages

- **`cmd/mc-router/`** — Entry point. Parses CLI flags via `go-flagsfiller`, sets up signal handling (SIGINT for shutdown, SIGHUP for config reload).
- **`server/`** — Core router logic:
  - `server.go` — Initializes all subsystems (metrics, routes, connector, API, service discovery)
  - `connector.go` — Connection handler: accepts clients, reads handshake, proxies traffic, manages rate limiting and client filtering
  - `routes.go` — In-memory route table mapping server addresses to backends; supports default route fallback
  - `routes_config_loader.go` — Loads/watches JSON routes config file (with fsnotify)
  - `k8s.go` — Kubernetes service discovery via annotation `mc-router.itzg.me/externalServerName`
  - `docker.go` — Docker container discovery via label `mc-router.host`; event-driven via Docker Events API (`client.Events`). Each event handled incrementally by `applyEvent` → `containersForID` (single `ContainerInspect`) → `applyContainerRoutesLocked` (touches only that container's routes). Full `monitorContainers` re-list runs at startup and on event-stream reconnect (exponential backoff)
  - `docker_swarm.go` — Docker Swarm service discovery via label `mc-router.host`; event-driven, but each service event triggers a full `reconcileServices` re-list (services churn rarely, swarm has no autoscaling)
  - `down_scaler.go` — Auto-scale down after idle period
  - `api_server.go` — REST API (`GET/POST /routes`, `POST /defaultRoute`, `DELETE /routes/{serverAddress}`)
  - `metrics.go` — Pluggable metrics backends (Prometheus, InfluxDB, expvar, discard)
  - `webhook_notifier.go` — POST notifications on connect/disconnect events
  - `client_filter.go` / `allow_deny_list.go` — IP and player allow/deny lists
  - `configs.go` — All configuration structs with CLI flag annotations
- **`mcproto/`** — Minecraft Java protocol implementation:
  - `types.go` — Frame, Packet, Handshake, LoginStart types
  - `read.go` / `write.go` — Network I/O for Minecraft protocol frames
  - `decode.go` — Packet decoding (handshake, login, status)

### Configuration

CLI flags are the primary config mechanism, with environment variable support via `go-flagsfiller` (flag `--some-flag` maps to env `SOME_FLAG`). Routes can also be loaded from a JSON config file (`--routes-config`). The `Config` struct in `server/configs.go` defines all options.

### Service Discovery

Routes are populated from three sources that can be combined:
1. Static `--mapping` flags or JSON config file
2. Kubernetes: watches Services with `mc-router.itzg.me/externalServerName` annotation
3. Docker/Swarm: watches containers/services via the Docker Events API, filtered to lifecycle events (label `mc-router.host`)

### Key Dependencies

- `sirupsen/logrus` — Logging
- `k8s.io/client-go` — Kubernetes client
- `github.com/docker/docker` — Docker client
- `github.com/gorilla/mux` — HTTP routing (API server)
- `github.com/prometheus/client_golang` — Prometheus metrics
- `golang.ngrok.com/ngrok` — ngrok tunnel integration
- `github.com/stretchr/testify` — Test assertions

### Concurrency Model

- **`routes.go`**: global singleton `var Routes = NewRoutes()`. `sync.RWMutex` protects `mappings` and `defaultRoute` — `RLock` for all reads, `Lock` for mutations.
- **`connector.go`**: `ActiveConnections` map guarded by `sync.RWMutex`; `totalActiveConnections` counter uses `atomic.AddInt32`. Shutdown drain uses `sync.Cond` in `WaitForConnections()`.
- **Bidirectional proxy**: two goroutines per connection (client→backend, backend→client) communicate via a buffered `chan error` (size 2) — first error triggers mutual close.
- All goroutines respect context cancellation via `select { case <-ctx.Done() }`.

### Error Handling

- Wrap with context: `fmt.Errorf("message: %w", err)`
- Check specific sentinels: `errors.Is(err, io.EOF)`
- Log with fields: `logrus.WithError(err).WithField("key", val).Error("msg")`

### Protocol Notes

The `mcproto` package handles Minecraft Java protocol quirks: Forge mod identifiers appended to server addresses (separated by `\x00`), DNS root zone trailing dots, legacy server list ping format, and VarInt encoding. Server address matching in routes also strips TCP Shield patterns (`///...`) and lowercases before lookup.

Auto-scale MOTD fallback: waking servers display `LoadingMOTD`; sleeping servers display `AsleepMOTD`. Per-route MOTDs take precedence over global ones.
