# educhan

**Anonymous imageboard for tertiary education**

educhan is a fast, high-signal imageboard designed for students, researchers, and academic communities. It combines classic anonymous discussion with optional institutional verification, creating a space for authentic conversations about placements, research, courses, and campus life.

## Core Features

- **Verified Anonymity** — Optionally verify institutional email domains (`.ac.in`, `.edu`, etc.) to receive a trusted badge while remaining fully anonymous
- **Imageboard-native experience** — Fast, ephemeral, image-first discussion
- **Technical media support** — Accepts raw camera files and high-bitrate video with automatic web-optimised proxies
- **Selective permanence** — High-value threads can be automatically archived
- **Google Cloud native** — Built for scalability and low operational overhead

## Current Status

- Domain: [educhan.org](https://educhan.org)
- Stage: Technology Concept (TRL 2)
- Architecture and product design completed
- Prototype under development

This repository currently contains the service skeleton: a configurable HTTP
server with structured logging, health endpoints, and graceful shutdown. Board,
thread, and media features are not implemented yet.

## Requirements

- Go 1.22 or newer
- Docker (optional, for container builds)

## Getting Started

```bash
git clone https://github.com/yuvraj-agarwal/educhan.git
cd educhan
make run
```

The server listens on `http://localhost:8080` by default.

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"dev"}
```

## Make Targets

Run `make help` for the full list.

| Target | Description |
| --- | --- |
| `make run` | Run the server locally |
| `make build` | Compile a static binary into `bin/` |
| `make test` | Run tests with the race detector |
| `make cover` | Report test coverage |
| `make lint` | Format and vet the codebase |
| `make docker` | Build the container image |

## Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/` | Placeholder landing page |
| `GET` | `/healthz` | Liveness probe; dependency-free by design |
| `GET` | `/readyz` | Readiness probe; gains dependency checks as backing services are added |

Every response carries an `X-Request-Id` header. An inbound `X-Request-Id` is
reused when it is well-formed, so requests stay traceable across services.

## Configuration

All configuration comes from the environment. Unset variables fall back to the
defaults below; a variable that is set but unparseable fails startup rather than
degrading silently.

| Variable | Default | Description |
| --- | --- | --- |
| `HOST` | *(all interfaces)* | Interface to bind |
| `PORT` | `8080` | Port to listen on |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `READ_HEADER_TIMEOUT` | `5s` | Deadline for reading request headers |
| `READ_TIMEOUT` | `15s` | Deadline for reading the full request |
| `WRITE_TIMEOUT` | `30s` | Deadline for writing the response |
| `IDLE_TIMEOUT` | `60s` | Keep-alive idle timeout |
| `SHUTDOWN_TIMEOUT` | `15s` | Grace period for in-flight requests on shutdown |

Logs are JSON on stdout. Health probes are logged at `debug` level so they do
not drown out real traffic.

## Project Layout

```
cmd/educhan/       Service entrypoint: config, logger, signal handling
internal/config/   Environment-based configuration
internal/server/   Routing, middleware, handlers, server lifecycle
Dockerfile         Multi-stage build onto a distroless base
Makefile           Build, test, and lint entrypoints
```

`internal/` is intentional: nothing here is a supported public API yet, so the
compiler prevents outside modules from importing it.

## Docker

```bash
make docker
docker run --rm -p 8080:8080 educhan:$(git describe --tags --always --dirty)
```

The image is built statically with `CGO_ENABLED=0` onto
`gcr.io/distroless/static-debian12:nonroot`, so it ships no shell or package
manager and runs as a non-root user.

## Deployment Notes

- The server binds `PORT`, which Cloud Run and most PaaS providers inject.
- `SIGTERM` starts a graceful drain bounded by `SHUTDOWN_TIMEOUT`. Keep that
  value below the platform's termination grace period.
- Point liveness checks at `/healthz` and readiness checks at `/readyz`.
- Stamp builds with `make build VERSION=$(git rev-parse --short HEAD)`; the
  value is reported by `/healthz` and attached to every log line.

## Tech Stack (Planned)

- **Backend**: Go
- **Database**: Cloud SQL (PostgreSQL)
- **Storage**: Google Cloud Storage
- **Compute**: Cloud Run
- **Moderation**: Gemini API + Cloud Vision
- **CDN & Security**: Cloud CDN + Cloud Armor

## Contributing

This project is in early stages. Contribution guidelines will be added once the initial prototype is ready.

Before opening a pull request, please ensure `make lint test` passes.

## License

MIT — see [LICENSE](LICENSE).
