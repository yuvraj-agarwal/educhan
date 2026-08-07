# educhan ships as a single static binary: the web UI and the SQL migrations are
# embedded in it (internal/app/web.go, internal/app/migrate.go), so the runtime
# image needs nothing but the executable and a writable upload directory.

ARG GO_VERSION=1.24

# ---- build ---------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off produces a statically linked binary, which is what the distroless
# runtime below can execute. -trimpath keeps build paths out of the binary.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/educhan ./cmd/server

# Created here rather than at runtime: the runtime image has no shell, and the
# media store needs a directory it may write to as a non-root user. A named
# volume mounted over this path inherits its ownership.
RUN mkdir -p /out/data/uploads

# ---- runtime -------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/educhan /usr/local/bin/educhan
COPY --from=build --chown=65532:65532 /out/data /data

ENV EDUCHAN_UPLOAD_DIR=/data/uploads \
    EDUCHAN_LOG_FORMAT=json

# Matches the server's default. Cloud Run overrides it by setting PORT, which
# ConfigFromEnv honours.
EXPOSE 8080

# 65532 is distroless's "nonroot" user.
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/educhan"]
