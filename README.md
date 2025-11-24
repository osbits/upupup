# UpUpUp

This repository now houses multiple Go subprojects that collaboratively power the UpUpUp platform.

## Projects

- `worker/` – the original monitoring engine. See `worker/README.md` for full documentation, configuration examples, and Docker usage.
- `server/` – Go HTTP API that exposes health, hook and Prometheus proxy endpoints backed by the shared SQLite datastore.
- `upgent/` – placeholder for generation tooling and auxiliary utilities.

Each project is an independent Go module. Create a personal `go.work` file if you want to develop several modules at once.

## Getting Started

1. Install Go 1.24 (or later).
2. Choose the module you want to work on (for example `cd worker/` or `cd server/`) and run Go commands from there.
3. For multi-module workflows, create your own `go.work` alongside the modules you wish to include.

## Server Quickstart

```
cd server
go run ./cmd/upupup-server --config ../config.yml
```

The server reads the shared `config.yml`, opens the `storage.path` SQLite database and exposes:

- `GET /healthcheck` – verifies database connectivity, recent check execution activity and notification log health.
- `GET /readiness` – reports readiness once the server is healthy and the Prometheus scrape configuration has been generated.
- `POST /api/hook/{id}` – triggers pre-defined hooks (for example temporary pause of notifications) with optional runtime parameters.
- `GET /api/metrics/{id}` – renders Prometheus-compatible metrics for a specific check using stored check state.

All endpoints enforce configurable IP allowlists defined under the `server:` section in `config.yml`.

> Docker Compose uses the `/readiness` endpoint as a container healthcheck so the bundled Prometheus service only starts once the server reports ready.

## Repository Layout

```
LICENSE
README.md          # this file
config.yml         # shared configuration used by worker and server
server/            # HTTP API and control plane
upgent/            # tooling module (placeholder)
worker/            # monitoring engine (formerly repository root)
```

## Licensing

The repository is licensed under the Apache License 2.0. See `LICENSE` for details.



