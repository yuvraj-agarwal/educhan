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
server with a health endpoint and graceful shutdown. Board, thread, and media
features are not implemented yet.

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
curl http://localhost:8080/health
# {"status":"ok","service":"educhan"}
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
| `GET` | `/health` | Health check; dependency-free by design |
| `GET` | `/` | Placeholder landing page |

`GET /` is a prefix pattern, so any path that is not otherwise routed is served
by the landing page rather than returning 404.

## Configuration

All configuration comes from the environment, so a single binary can be promoted
between environments without a rebuild. Unset variables fall back to the
defaults below.

| Variable | Default | Description |
| --- | --- | --- |
| `ADDR` | `:8080` | TCP address to listen on |
| `ENV` | `development` | Deployment environment name |

Request timeouts and the shutdown grace period are compile-time constants in
`internal/server`; promote them to `Config` if they need to vary per
environment.

## Project Layout

```
cmd/educhan/       Service entrypoint
internal/config/   Environment-based configuration
internal/server/   Routing and server lifecycle
internal/handler/  HTTP handlers
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

- Set `ADDR` to match the port the platform expects. On Cloud Run, which injects
  `PORT`, that means `ADDR=:$PORT`.
- `SIGTERM` starts a graceful drain bounded by `shutdownTimeout` (15s). Keep it
  below the platform's termination grace period.
- Point liveness and readiness checks at `/health`.

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
