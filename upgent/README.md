# Upgent

Upgent is a lightweight sidecar that scrapes metrics from a local Prometheus
`node_exporter` instance and forwards the raw payload to the upupup server
ingest API (`/api/ingest/{node_id}`).

## Quick start (Docker Compose)

The repository ships with a dedicated compose file that bundles Upgent and
`node_exporter`.

```bash
cd upgent

# Configure the required environment variables and launch
export UPGENT_NODE_ID=my-node-01
export UPGENT_SERVER_URL=http://upupup-server:8080

docker compose -f compose.yml up
```

By default Upgent scrapes the exporter at `http://node-exporter:9100/metrics`
every 15 seconds, gzips the response, and POSTs it to
`${UPGENT_SERVER_URL}/api/ingest/${UPGENT_NODE_ID}`. Adjust the environment
variables in `compose.yml` to change this behaviour.

> **Note:** The provided compose definition expects to run on a Linux host so
> that `node_exporter` can mount `/proc`, `/sys` and `/` read-only. When running
> on macOS or Windows, remove or adapt those mounts and flags according to your
> environment.

## Configuration

Upgent is configured through environment variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `UPGENT_NODE_ID` | ✅ | - | Identifier used when posting to the ingest API. |
| `UPGENT_SERVER_URL` | ✅ | - | Base URL of the upupup server (e.g. `http://server:8080`). |
| `UPGENT_SCRAPE_URL` | | `http://node-exporter:9100/metrics` | Metrics endpoint to scrape. |
| `UPGENT_INTERVAL` | | `15s` | Interval between scrapes (Go duration format). |
| `UPGENT_TIMEOUT` | | `10s` | Overall timeout for scrape and ingest HTTP requests. |
| `UPGENT_MAX_METRICS_BYTES` | | `2097152` | Maximum accepted scrape payload size in bytes. |
| `UPGENT_ENABLE_GZIP` | | `true` | Compress payloads with gzip before sending. |
| `UPGENT_SKIP_TLS_VERIFY` | | `false` | Skip TLS certificate verification (use with caution). |
| `UPGENT_LOG_LEVEL` | | `info` | Log level (`debug`, `info`, `warn`, `error`). |
| `UPGENT_USER_AGENT` | | `upgent/0.1` | Custom User-Agent header. |

When gzip is enabled the agent sets the `Content-Encoding` header and the server
automatically inflates the payload.

## Building locally

```bash
cd upgent
go build ./cmd/upgent
```

To build the container image:

```bash
docker build -t upgent:latest .
```

## Operational notes

- Ensure the upupup server allows the Upgent host IP in its `allowed_ips`
  configuration so that ingest requests are accepted.
- Metrics are forwarded verbatim; no additional relabeling or filtering is
  performed by Upgent.
- Non-200 scrape responses or non-202 ingest responses are logged as errors and
  retried on the next interval.

