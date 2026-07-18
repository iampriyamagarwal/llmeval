# llmeval

A production-oriented LLM inference proxy in Go. It exposes a chat-completions
endpoint that forwards requests to an upstream model and returns the response to
the caller, while **shadow-evaluating** that response against a second model off
the request path. The shadow programme is fully observable via OpenTelemetry
traces and Prometheus metrics, and it never affects the caller-facing latency or
result.

It uses only the standard library `net/http` for serving, with structured
logging, configuration via viper, tuned outbound HTTP clients with
retry-with-backoff, and a bounded background worker pool that runs comparisons
asynchronously and sheds load when saturated.

## Architecture

The service is built around a clear separation between the **request path**
(what the caller waits on) and the **shadow path** (best-effort background work).

- **`cmd/server`** wires everything together: it loads config, sets up
  telemetry, builds the two outbound HTTP clients, starts the worker pool and
  comparator, constructs the handler, and runs the HTTP server with graceful
  shutdown.
- **`internal/handlers`** owns routing, middleware, and the `/v1/chat` proxy. It
  forwards the request to the inference endpoint via the **primary** client,
  streams the upstream response back to the caller, and then fires a
  fire-and-forget shadow comparison.
- **`internal/clients` (httpx)** provides the shared building blocks for outbound
  calls: a tuned `*http.Client` (timeout + connection pooling), an
  OpenTelemetry-instrumented transport, retry-with-backoff for transient
  failures (429/5xx, honouring `Retry-After`), and typed error decoding.
- **`internal/worker`** is a bounded, in-process goroutine pool fed by a buffered
  queue. Submissions are non-blocking: a full queue sheds the job rather than
  stalling the request path. Jobs recover from panics and each gets its own span.
- **`internal/shadow`** builds and runs a comparison job: it re-sends the
  original request to a **shadow** model (with the `model` field rewritten), then
  answers two questions — (1) did both models return JSON-parsable payloads, and
  (2) do their extracted `action` keys match exactly — recording the outcome as
  metrics and span attributes.
- **`internal/telemetry`** installs a W3C propagator, a pull-based Prometheus
  metrics exporter (served at `/metrics`), and, when an OTLP endpoint is
  configured, an OTLP/HTTP trace exporter. Everything degrades to no-op rather
  than crashing.

### Request & shadow flow

```mermaid
flowchart TD
    client([Client])

    subgraph server["llmeval server"]
        mw["Logging + otelhttp middleware"]
        chat["POST /v1/chat handler"]
        primary["Primary HTTP client<br/>(timeout, pooling, retry)"]
        pool["Worker pool<br/>(bounded queue)"]
        comparator["Shadow comparator"]
        shadowClient["Shadow HTTP client<br/>(timeout, pooling, retry)"]
        metrics["/metrics (Prometheus)"]
    end

    upstream["Inference endpoint<br/>(primary model)"]
    shadowUpstream["Shadow endpoint<br/>(comparison model)"]
    otlp["OTLP/HTTP collector<br/>(traces, optional)"]
    scraper["Prometheus scraper"]

    client -->|"chat request"| mw --> chat
    chat -->|"forward request"| primary -->|"POST"| upstream
    upstream -->|"response"| primary --> chat
    chat -->|"stream response"| client

    chat -.->|"submit job<br/>(fire-and-forget)"| pool
    pool -->|"run"| comparator
    comparator -->|"rewrite model + POST"| shadowClient -->|"POST"| shadowUpstream
    shadowUpstream -->|"response"| shadowClient --> comparator
    comparator -.->|"compare actions<br/>emit metrics + spans"| metrics

    scraper -->|"scrape"| metrics
    server -.->|"export spans"| otlp
```

Solid arrows are the synchronous request path the caller waits on; dashed arrows
are the asynchronous shadow/telemetry path that runs after the response has been
returned. If the worker queue is full, the shadow job is dropped (and counted)
rather than delaying the caller.

## Project layout

```
.
├── cmd/
│   └── server/
│       └── main.go            # Entrypoint: wiring, server, graceful shutdown
├── config/
│   ├── config.go              # Typed Config loaded via viper (env + .env)
│   └── config_test.go
├── internal/
│   ├── clients/
│   │   ├── httpx.go           # Outbound HTTP client: pooling, retry, APIError
│   │   └── httpx_test.go
│   ├── handlers/
│   │   ├── handlers.go        # Handler struct, Routes(), /v1/chat proxy, middleware
│   │   └── handlers_test.go
│   ├── logger/
│   │   ├── logger.go          # slog-based structured logger
│   │   └── logger_test.go
│   ├── shadow/
│   │   ├── shadow.go          # Off-path shadow comparison + telemetry
│   │   └── shadow_test.go
│   ├── telemetry/
│   │   ├── telemetry.go       # OpenTelemetry setup (Prometheus + OTLP/HTTP)
│   │   └── telemetry_test.go
│   └── worker/
│       ├── worker.go          # Bounded background goroutine pool
│       └── worker_test.go
├── test/
│   └── integration/           # End-to-end handler tests
├── .do/
│   └── app.yaml               # DigitalOcean App Platform spec (templated)
├── .github/workflows/         # CI/CD pipelines
├── Dockerfile                 # Multi-stage, distroless nonroot
├── .dockerignore
├── .env.example
└── go.mod
```

## Configuration

Configuration is read from real environment variables, with an optional `.env`
file as a fallback (a missing file is not an error). **Environment variables
take precedence over the `.env` file.**

### Core

| Variable          | Default       | Description                                  |
| ----------------- | ------------- | -------------------------------------------- |
| `APP_ENV`         | `development` | Environment; `production` enables JSON logs. |
| `HOST`            | `0.0.0.0`     | Bind host.                                    |
| `PORT`            | `9090`        | Bind port.                                   |
| `LOG_LEVEL`       | `info`        | `debug` \| `info` \| `warn` \| `error`.      |
| `SERVICE_NAME`    | `llmeval`     | Service name (OTel resource attribute).      |
| `SERVICE_VERSION` | `dev`         | Service version (OTel resource attribute).   |
| `MODEL_ACCESS_KEY`| _(none)_      | Secret credential for the model provider. Never commit it. |
| `INFERENCE_ENDPOINT` | `https://inference.do-ai.run/v1/chat/completions` | Upstream chat-completions URL `/v1/chat` proxies to. |

### Shadow evaluation & worker pool

| Variable                  | Default          | Description                                          |
| ------------------------- | ---------------- | --------------------------------------------------- |
| `SHADOW_MODEL`            | `alibaba-qwen3-32b` | Model substituted into the request body for the shadow call. Empty reuses the caller's model. |
| `SHADOW_ENDPOINT`         | `https://inference.do-ai.run/v1/chat/completions` | Chat-completions URL for the shadow call. Empty falls back to `INFERENCE_ENDPOINT`. |
| `WORKER_COUNT`            | `4`              | Number of background worker goroutines.             |
| `WORKER_QUEUE_SIZE`       | `64`             | Buffered queue depth; submissions are dropped (load shed) when full. |
| `WORKER_SHUTDOWN_TIMEOUT` | `5s`             | How long graceful shutdown waits for in-flight/queued jobs to drain. |

### Outbound HTTP clients

The primary and shadow clients are tuned independently. Durations accept Go
duration strings (e.g. `"30s"`, `"90s"`).

| Variable                            | Default | Description                                |
| ----------------------------------- | ------- | ------------------------------------------ |
| `PRIMARY_TIMEOUT`                   | `30s`   | Whole-request timeout for the primary client. |
| `PRIMARY_MAX_IDLE_CONNS`            | `100`   | Idle (keep-alive) connections across all hosts. |
| `PRIMARY_MAX_IDLE_CONNS_PER_HOST`   | `10`    | Idle connections per destination host.     |
| `PRIMARY_IDLE_CONN_TIMEOUT`         | `90s`   | How long an idle connection is kept.       |
| `SHADOW_TIMEOUT`                    | `30s`   | Whole-request timeout for the shadow client. |
| `SHADOW_MAX_IDLE_CONNS`             | `100`   | Idle connections across all hosts.         |
| `SHADOW_MAX_IDLE_CONNS_PER_HOST`    | `10`    | Idle connections per destination host.     |
| `SHADOW_IDLE_CONN_TIMEOUT`          | `90s`   | How long an idle connection is kept.       |

### HTTP server timeouts

| Variable               | Default | Description             |
| ---------------------- | ------- | ----------------------- |
| `SERVER_READ_TIMEOUT`  | `30s`   | Server read timeout.    |
| `SERVER_WRITE_TIMEOUT` | `30s`   | Server write timeout.   |
| `SERVER_IDLE_TIMEOUT`  | `60s`   | Server idle timeout.    |

Copy `.env.example` to `.env` to get started:

```bash
cp .env.example .env
```

## Endpoints

| Method | Path       | Description                                                       |
| ------ | ---------- | ---------------------------------------------------------------- |
| `GET`  | `/`        | Welcome message (JSON).                                           |
| `GET`  | `/health`  | Health status, current env, and UTC RFC3339 time.                |
| `POST` | `/v1/chat` | Proxies the body to the inference endpoint and returns the upstream response, then shadow-evaluates it off-path. |
| `GET`  | `/metrics` | Prometheus exposition endpoint (served outside the traced router). |

Application routes are registered using Go 1.22+ method-based routing. Every
request passes through a request-logging middleware (method, path, status,
latency, remote_addr) and an `otelhttp` handler that emits a server span and the
standard HTTP server metrics. The `/metrics` endpoint is mounted outside that
router so collector scrapes don't generate spans or request-log noise.

## Telemetry

OpenTelemetry is split into a **pull-based** metrics path and a **push-based**
traces path.

- **Metrics** use a Prometheus exporter and are always enabled: they make no
  outbound connections and are exposed at `/metrics` for a collector to scrape.
  A composite W3C `tracecontext` + `baggage` propagator and Go runtime metrics
  are also installed.
- **Traces** are driven by the standard `OTEL_*` environment variables. When an
  OTLP endpoint is set (`OTEL_EXPORTER_OTLP_ENDPOINT` or
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`), a global tracer provider with a batched
  OTLP/HTTP exporter is installed; otherwise a no-op tracer is used and no
  network connections are made.

The service is reported via the `service.name`, `service.version`, and
`deployment.environment.name` resource attributes. Any exporter that fails to
build downgrades to no-op with a warning; telemetry never crashes the app, and
it is flushed during graceful shutdown.

### Shadow metrics

The shadow programme emits dedicated counters so it can be monitored without
touching the caller-facing path:

| Metric                             | Description                                                        |
| ---------------------------------- | ----------------------------------------------------------------- |
| `shadow.requests.total`            | Chat requests shadowed against the comparison model.              |
| `shadow.success.total`             | Shadow calls that returned a JSON-parsable payload and were compared. |
| `shadow.failure.total`             | Failed comparisons, labelled by `reason` (`timeout`, `request_error`, `shadow_status`, `primary_unparsable`, `shadow_unparsable`). |
| `shadow.timeout.total`             | Shadow requests that failed due to a deadline/timeout.           |
| `shadow.actions.comparisons.total` | Action-key comparisons, labelled by `match=true` \| `false`.      |
| `shadow.dropped.total`             | Comparisons shed without running because the worker queue was full. |

## Running

```bash
# Run directly
go run ./cmd/server

# Build a binary
go build -o bin/server ./cmd/server
./bin/server
```

The server starts in a goroutine and shuts down gracefully on `SIGINT` /
`SIGTERM` with a 10s timeout, after which telemetry is flushed within the same
window. Read/write/idle timeouts are configured on the HTTP server.

## Testing

```bash
go test ./...
go test -race ./...
```

## Docker

```bash
docker build -t llmeval:local .
docker run --rm -p 9090:9090 llmeval:local
```

The image is a statically-linked binary (`CGO_ENABLED=0`, `-trimpath`,
`-ldflags="-s -w"`) on a distroless nonroot base.

## Deployment flow

CI/CD is implemented with GitHub Actions and DigitalOcean.

- **`ci.yml`** — On pull requests to `main` touching Go files: checks `gofmt`,
  verifies `go mod tidy` is clean, runs `go vet`, `golangci-lint`, and
  `go test -race`.
- **`cd-build.yml`** — On push/merge to `main`: builds the image and pushes it
  to DigitalOcean Container Registry (DOCR) tagged with the commit SHA. No
  deploy.
- **`cd-retag.yml`** — On pushing a `v*` git tag: waits for the commit-SHA image
  to exist, then uses `crane tag` to alias the release tag onto that exact image
  digest in DOCR (no rebuild, no deploy).
- **`cd-deploy.yml`** — Manual `workflow_dispatch` taking a release tag (or a raw
  `sha` escape hatch that takes precedence): verifies the image exists in DOCR,
  then deploys to DigitalOcean App Platform using the templated `.do/app.yaml`
  (with `${IMAGE_TAG}` substituted). It targets the `production` environment so
  a Required Reviewer gates the deploy, and uses a `deploy-production`
  concurrency group that never cancels an in-flight deploy.

### Required repository configuration

- **Secret** `DIGITALOCEAN_ACCESS_TOKEN` — DigitalOcean API token.
- **Variable** `DOCR_REGISTRY` — DOCR registry name.
- **Variable** `DO_APP_ID` — (optional) existing App Platform app ID; if unset,
  a new app is created on first deploy.
- **Environment** `production` — configure a Required Reviewer to gate deploys.
