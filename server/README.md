# UpUpUp Server

The server module provides an HTTP API that sits alongside the monitoring worker. It reads the same `config.yml` file and SQLite database, enabling health checks, hook orchestration and Prometheus-compatible data export.

## Features

- **Health endpoint** – validates database connectivity, recent check execution activity and notification log health (`GET /healthcheck`).
- **Hook endpoint** – triggers pre-defined operational hooks (e.g. pause notifications for a check) with optional runtime metadata (`POST /api/hook/{id}`).
- **Prometheus proxy** – renders the most recent check state as metrics consumable by Prometheus scrapers (`GET /api/data/{checkID}`).
- **Metrics ingestion** – accepts node exporter style snapshots from agents and persists them for later consumption (`POST /api/ingest/{id}`).
- **IP allowlists** – global and per-hook CIDR/IP rules restrict who may access the API.

## Configuration

The server re-uses `config.yml`. The following sections are relevant:

```yaml
storage:
  path: /app/data/monitor.db

server:
  listen: ":8080"
  allowed_ips:
    - 127.0.0.1/32
  trusted_proxies: []
  health:
    max_interval_multiplier: 3
    required_recent_runs: 1
  prometheus:
    namespace: upupup

hooks:
  - id: pause-ms-portal
    description: Pause ms-portal notifications until the next success or for 15 minutes.
    action:
      kind: pause_notifications
      scope: check
      target_ids: [ms-portal]
      duration: 15m
      max_duration: 1h
      until_first_success: true
```

Hooks may optionally define `allowed_ips` (restricting the hook further) and `metadata` which becomes part of the recorded hook payload.

## Running

```bash
cd server
go run ./cmd/upupup-server --config ../config.yml
```

Environment overrides:

- `MONITOR_DB_PATH` – overrides `storage.path`.
- `--listen` flag – overrides `server.listen`.

## Tests

Run the server module tests with:

```bash
cd server
go test ./...
```

Integration tests are not provided yet; the module focuses on deterministic helpers and compilation checks.


