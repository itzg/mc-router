# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

mc-router is a Minecraft Java Edition connection router/proxy that multiplexes multiple backend Minecraft servers onto a single public IP and port. It routes client connections based on the requested server hostname extracted from the Minecraft handshake packet. Supports auto-discovery via Kubernetes and Docker, auto-scaling (scale to zero/up on demand), rate limiting, PROXY protocol, metrics, webhooks, and ngrok tunnels.

## Build & Test Commands

```bash
# Run all tests
go test ./...
# or
make test

# Run a single test
go test ./server -run TestRoutesSetMapping

# Build the binary
go build ./cmd/mc-router

# Build Docker image (multi-stage, scratch-based)
docker build -t mc-router .
```

There is no separate lint command configured. The CI runs `go test` with coverage via a reusable workflow at `itzg/github-workflows`.

## Architecture

### Entry Point

`cmd/mc-router/main.go` — Parses CLI flags (using `itzg/go-flagsfiller` which auto-generates flags from struct tags with env var support), creates a `server.Server`, handles graceful shutdown via context cancellation, and SIGHUP for config reloads.

### Core Packages

**`server/`** — All server logic lives here as a single package:

- **`configs.go`** — `Config` struct with 30+ fields. Each field has `usage` tags consumed by go-flagsfiller. Environment variables are derived automatically from flag names.
- **`server.go`** — `Server` struct orchestrates startup: initializes metrics, down-scaler, routes, connector, API server, and route watchers (K8s/Docker/Swarm).
- **`connector.go`** (~700 lines) — Core connection handler. Accepts TCP connections, reads the Minecraft handshake, resolves the route, dials the backend, and relays bidirectional traffic. Also handles rate limiting, PROXY protocol send/receive, ngrok, client IP filtering, and webhook notifications.
- **`routes.go`** — Global `Routes` variable implementing `IRoutes` interface. Thread-safe (sync.RWMutex) mapping from external hostnames to backend `host:port`. Supports waker/sleeper functions for auto-scaling and SRV record simplification.
- **`k8s.go`** — Watches Kubernetes Services for `mc-router.itzg.me/*` annotations. Auto-scales StatefulSets (0↔1 replicas).
- **`docker.go`** / **`docker_swarm.go`** — Polls Docker API for containers/services with `mc-router.*` labels. Docker watcher supports auto-start/stop of containers.
- **`routes_config_loader.go`** — Loads route mappings from a JSON config file. Supports file watching and SIGHUP-triggered reloads.
- **`api_server.go`** — REST API (gorilla/mux) for dynamic route management: `GET/POST /routes`, `POST /defaultRoute`, `DELETE /routes/{serverAddress}`.
- **`metrics.go`** — `MetricsBuilder` interface with 4 backends: discard, expvar, prometheus, influxdb.
- **`down_scaler.go`** — Manages idle timeouts for scale-to-zero.
- **`client_filter.go`** / **`allow_deny_list.go`** — CIDR-based client filtering and per-server player allow/deny lists.
- **`webhook_notifier.go`** — POSTs connection events to a configured URL.

**`mcproto/`** — Minecraft protocol implementation:

- **`types.go`** — Protocol constants, packet types (Handshake, LoginStart, StatusResponse), version map (1.18.2–1.21.5), state machine (Handshaking → Status or Login).
- **`read.go`** — Reads and decodes Minecraft packets from the wire. `ReadPacket`, `DecodeHandshake`, `DecodeLoginStart` handle version-specific differences.
- **`write.go`** — Writes protocol responses (status/disconnect packets).

### Connection Flow

1. Client connects → `connector.go` accepts
2. Reads Minecraft handshake packet → extracts requested hostname
3. `routes.go` resolves hostname to backend `host:port`
4. If route has a waker (auto-scale), triggers scale-up and waits
5. Dials backend server, optionally sends PROXY protocol header
6. Relays bidirectional traffic, tracking metrics
7. On disconnect, updates down-scaler and connection counts

## Testing Patterns

- Uses `stretchr/testify` (assert/require) for assertions
- Table-driven tests with `[]struct` test case slices
- Minecraft protocol tests use hex-encoded packet fixtures for multiple protocol versions
- Test files are colocated in their respective packages (`server/`, `mcproto/`)

## Configuration

All config is in `server/configs.go`. The `Config` struct fields map directly to CLI flags and environment variables via go-flagsfiller. For example, `--connection-rate-limit` maps to env var `CONNECTION_RATE_LIMIT`. Nested structs create flag prefixes (e.g., `--auto-scale-up`).

## Go Version

Go 1.25 (specified in go.mod).
