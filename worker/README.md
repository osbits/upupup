# UpUpUp Monitor

UpUpUp is a composable infrastructure health-check and notification service implemented in Go. It loads a declarative YAML configuration describing checks, thresholds, and notification policies, then executes those checks on a configurable schedule. The service ships with a Docker-first setup for straightforward deployment.

> This directory hosts the `worker` module inside the multi-project UpUpUp repository. For other components see the repository root README.

## Features

- **Multi-protocol checks**: HTTP/S (with templated headers/body and optional pre-auth token flows), TCP, ICMP, DNS, TLS certificate validation, and WHOIS expiry.
- **Metrics checks**: Validate node-exporter style metrics ingested via the server against configurable thresholds and freshness windows.
- **Flexible assertions**: Compare HTTP status codes, JSONPath expressions, body regexes, latency, SSL validity, DNS answers, and more.
- **Thresholds & retries**: Per-check retry/backoff, sliding window failure ratios, and maintenance windows to suppress alerts.
- **Notification routing**: Escalation policies with timed stages; out of the box support for email (SMTP), Twilio or Vonage SMS/voice, generic webhooks, Slack, Telegram, and Discord.
- **Templating support**: Render request bodies/headers and webhook payloads with secrets (`{{ secret "KEY" }}`) and captured variables.
- **Structured logging**: Optional per-run logging via the `log_runs` setting at global or per-check scope.

## Repository Layout

```
cmd/monitor/           # main entrypoint
internal/checks/       # protocol-specific execution logic
internal/config/       # YAML config types and loader
internal/notifier/     # notifier implementations and registry
internal/render/       # template engine helpers
internal/runner/       # scheduler, state tracking, routing
Dockerfile
compose.yml
config.yml             # sample configuration
```

## Configuration

All behaviour is driven by `config.yml`. Key sections:

- `service`: global defaults (interval, timeout, retries, backoff, timezone, maintenance windows, `log_runs`, etc.).
- `storage`: sqlite persistence for check history and notifications (`path`, retention knobs). The `MONITOR_DB_PATH` env var overrides `storage.path`.
- `secrets`: names mapped to environment variables (`env:VAR_NAME`) used later in templates.
- `notifiers`: delivery endpoints, each with a unique `id`.
- `notification_policies`: escalation routes keyed by labels (e.g. `env: prod` or `category: security`).
- `assertion_sets`: reusable bundles of assertions you can reference from multiple checks.
- `checks`: individual monitoring definitions.

### Example: Vonage SMS notifier

```yaml
- id: sms-oncall
  type: sms
  config:
    provider: vonage
    api_key_ref: VONAGE_API_KEY
    api_secret_ref: VONAGE_API_SECRET
    from: "UpUpUp"
    to:
      - "+41790001122"
```

Add corresponding secret entries such as:

```yaml
secrets:
  VONAGE_API_KEY: env:VONAGE_API_KEY
  VONAGE_API_SECRET: env:VONAGE_API_SECRET
```

For Vonage voice calls supply a pre-generated JWT (via `jwt_ref`) and optional message/voice name:

```yaml
- id: voice-escalation
  type: voice
  config:
    provider: vonage
    jwt_ref: VONAGE_VOICE_JWT
    from: "+14155550123"
    to:
      - "+41790001122"
    message: "{{ .check.name }} is {{ .status }} – please investigate."
    voice_name: "Joanna"
```

Expose the JWT via `secrets` (for example `VONAGE_VOICE_JWT: env:VONAGE_VOICE_JWT`).

### Example: Global Defaults

```yaml
service:
  name: upupup
  timezone: Europe/Zurich
  defaults:
    interval: 60s
    timeout: 10s
    retries: 2
    backoff: 2s
    log_runs: true         # enable per-run logging
    maintenance_windows:
      - "cron: 0 2 * * SUN"
      - "range: 2025-12-24T00:00-2025-12-26T23:59"
```

### Example: HTTP Check

The example below reuses the `http-status-200` assertion set and adds extra assertions specific to this check.

```yaml
- id: api-health
  name: Public API /health
  type: http
  target: "https://api.example.com/health"
  schedule:
    interval: 30s
  request:
    method: GET
    headers:
      Accept: "application/json"
    timeout: 5s
  assertion_sets: [http-status-200]
  assertions:
    - kind: latency_ms
      op: less_than
      value: 300
  thresholds:
    failure_ratio:
      window: 4
      fail_count: 3
  labels:
    env: prod
    team: core
    service: api
  notifications:
    route: route-prod
    overrides:
      initial_notifiers: [telegram-noc]
```

#### Per-check options

- `schedule.interval`, `schedule.timeout`, `schedule.retries`, `schedule.backoff` override defaults.
- `log_runs: true|false` toggles per-run logging for an individual check.
- `preauth` supports token capture before executing the main request.
- `assertion_sets` allows you to include one or more reusable assertion bundles defined at the root of the config.
- Assertions vary by check type (`latency_ms`, `tcp_connect`, `packet_loss_percent`, `ssl_valid_days`, `domain_expires_in_days`, etc.).

See the provided `config.yml` for additional examples, including a WHOIS domain expiry check and TLS validation.

### Example: Metrics Check

Metrics checks read the latest snapshot stored by the server-side ingestion API (`POST /api/ingest/{nodeID}`) and evaluate one or more metric thresholds. Each threshold targets a Prometheus metric name and optional label selector.

```yaml
- id: node-load
  name: Node Load Average
  type: metrics
  metrics:
    node_id: node-a
    max_age: 5m            # optional freshness guard
    computed:
      disk_usage_root:
        expression: "((size - avail) / size) * 100"
        variables:
          size:
            name: node_filesystem_size_bytes
            labels:
              mountpoint: "/"
              fstype: "ext4"
          avail:
            name: node_filesystem_avail_bytes
            labels:
              mountpoint: "/"
              fstype: "ext4"
    thresholds:
    - name: disk_usage_root
      op: less_than
      value: 80
      - name: node_load1
        op: less_than
        value: 1.5
        labels:
          instance: node-a:9100
      - name: node_cpu_seconds_total
        op: greater_than
        value: 10
        labels:
          instance: node-a:9100
          mode: system
  notifications:
    route: route-prod
```

If `metrics.node_id` is omitted the worker falls back to the check `target`. Thresholds use the same comparison operators as assertions (`less_than`, `<`, `greater_than`, `>`, `equals`, etc.), and any missing metric or label match is treated as a failed assertion that can trigger notifications.

The optional `metrics.computed` map lets you derive new series from existing ones before evaluating thresholds. Each computed entry defines an arithmetic expression and the metric variables it depends on; thresholds can then reference the computed metric by name (e.g. `disk_usage_root` above).

## Running Locally

### Prerequisites

- Docker or nerdctl (for the provided Compose workflow).
- Optional: Go 1.24+ if you plan to build and run without containers.

### Using Docker Compose

1. Copy `config.yml` and adjust URLs, assertions, and notification routing.
2. Ensure the necessary secrets are available as environment variables (see below).
3. From this directory (`worker/`), start the service (swap `docker` for `nerdctl` if you use containerd tooling):

   ```sh
   docker compose up --build
   # or nerdctl compose up --build
   ```

   The service runs under the `monitor` container. Logs are emitted via structured JSON to stdout.

4. To stop:

   ```sh
   docker compose down
   # or nerdctl compose down
   ```

Compose grants `NET_RAW` capability so ICMP checks can send ping packets. If you do not need ping checks, you may remove that capability.

### Environment Variables / Secrets

Secrets referenced in `config.yml` must be exposed to the container:

```
SMTP_PASSWORD
TWILIO_AUTH_TOKEN
TELEGRAM_BOT_TOKEN
DISCORD_WEBHOOK_URL
SLACK_WEBHOOK_URL
API_USER
API_PASS
```

Set them in a `.env` file or export before running `docker compose up`. Example `.env`:

```
SMTP_PASSWORD=supersecret
TWILIO_AUTH_TOKEN=xxx
TELEGRAM_BOT_TOKEN=yyy
DISCORD_WEBHOOK_URL=https://discord...
SLACK_WEBHOOK_URL=https://hooks.slack.com/...
API_USER=monitor
API_PASS=monitor_password
```

### Rollbar

The worker automatically loads a local `.env` file on startup. Set `ROLLBAR_ACCESS_TOKEN` there (or export it) to enable Rollbar reporting; leave it unset to keep Rollbar disabled. Optional helpers include `ROLLBAR_ENVIRONMENT` and `ROLLBAR_CODE_VERSION` for tagging payloads.

### Running Without Docker

```sh
go build ./cmd/monitor
MONITOR_CONFIG=./config.yml ./monitor
```

Ensure you export all required secrets in your shell beforehand.

## Logging

- Structured JSON logs via `log/slog`. Each check loop emits a startup log such as:

  ```
  {"time":"...","level":"INFO","msg":"starting check loop","check_id":"api-health","interval":30000000000}
  ```

  The interval value is in nanoseconds (30 seconds in the example).

- If `log_runs` is enabled (globally or per check), every execution logs a summary with success flag, latency, and failed assertion count:

  ```
  {"time":"...","level":"INFO","msg":"check run","check_id":"api-health","success":true,"latency":"85.3ms"}
  ```

- When a check transitions into failure or recovery, the runner emits `ERROR` or `INFO` logs and triggers the configured notifications.

## Extending

- **New check types**: Add an implementation under `internal/checks` and extend the `Execute` switch.
- **New notifiers**: Implement the `Notifier` interface under `internal/notifier` and register it in `registry.go`.
- **Custom templating**: `internal/render` exposes helper functions; extend as needed for additional template features.

## Troubleshooting

- **No logs for successful runs**: Ensure `log_runs: true` is set either globally or on the check.
- **ICMP failures in containers**: Verify the container has `NET_RAW` capability and the host allows ping.
- **Missing secrets**: The service will exit with an error like `missing env var "SMTP_PASSWORD"` if a referenced secret is not provided.
- **WHOIS server unsupported**: Currently supports popular TLDs; extend `whoisServerCache` in `internal/checks/run.go` for others.

## License

This project is licensed under the Apache License, Version 2.0. You may obtain a copy of the License at https://www.apache.org/licenses/LICENSE-2.0

