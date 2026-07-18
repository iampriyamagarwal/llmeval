# llmeval

A minimal, production-oriented REST API scaffold in Go. It uses only the
standard library `net/http` for serving, with structured logging, configuration
via viper, and OpenTelemetry tracing/metrics wired in.

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
│   ├── handlers/
│   │   ├── handlers.go        # Handler struct, Routes(), logging middleware
│   │   └── handlers_test.go
│   ├── logger/
│   │   ├── logger.go          # slog-based structured logger
│   │   └── logger_test.go
│   └── telemetry/
│       ├── telemetry.go       # OpenTelemetry setup (OTLP/HTTP, no-op fallback)
│       └── telemetry_test.go
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

| Variable          | Default       | Description                                  |
| ----------------- | ------------- | -------------------------------------------- |
| `APP_ENV`         | `development` | Environment; `production` enables JSON logs. |
| `HOST`            | `0.0.0.0`     | Bind host.                                    |
| `PORT`            | `9090`        | Bind port.                                   |
| `LOG_LEVEL`       | `info`        | `debug` \| `info` \| `warn` \| `error`.      |
| `SERVICE_NAME`    | `llmeval`     | Service name (OTel resource attribute).      |
| `SERVICE_VERSION` | `dev`         | Service version (OTel resource attribute).   |

Copy `.env.example` to `.env` to get started:

```bash
cp .env.example .env
```

## Endpoints

| Method | Path      | Description                                        |
| ------ | --------- | -------------------------------------------------- |
| `GET`  | `/`       | Welcome message (JSON).                            |
| `GET`  | `/health` | Health status, current env, and UTC RFC3339 time.  |

Routes are registered using Go 1.22+ method-based routing. Every request passes
through a request-logging middleware (method, path, status, latency,
remote_addr) and an `otelhttp` handler that emits a server span and the standard
HTTP server metrics.

## Telemetry

OpenTelemetry is driven entirely by the standard `OTEL_*` environment variables.

- **Unconfigured (default):** if no OTLP endpoint is set
  (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, or
  `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`), no-op providers are installed, no
  network connections are made, and a single log line is emitted.
- **Configured:** global tracer and meter providers are set using OTLP/HTTP
  exporters (batched span processor + periodic metric reader), a composite W3C
  `tracecontext` + `baggage` propagator is installed, and Go runtime metrics are
  started. The service is reported via the `service.name`, `service.version`,
  and `deployment.environment.name` resource attributes.

An exporter that fails to build downgrades to no-op with a warning; telemetry
never crashes the app. Telemetry is flushed during graceful shutdown.

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
