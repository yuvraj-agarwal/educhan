FROM golang:1.22-alpine AS build

WORKDIR /src

# Copy manifests first so dependency download stays cached across code changes.
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X github.com/yuvraj-agarwal/educhan/internal/server.Version=${VERSION}" \
    -o /out/educhan ./cmd/educhan

# distroless/static carries no shell or package manager, which keeps the
# runtime attack surface minimal.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/educhan /usr/local/bin/educhan

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/educhan"]
