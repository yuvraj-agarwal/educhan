.PHONY: run build test check tidy fmt vet db-up

# Start (or reuse) the local PostgreSQL cluster used for development.
db-up:
	bash .cursor/scripts/start.sh

# Run the development server (expects Postgres to be up; see `make db-up`).
run:
	go run ./cmd/server

build:
	go build ./...

# Integration tests need PostgreSQL; they skip themselves when it is unreachable.
test:
	go test ./...

# Everything CI should agree on before a change lands.
check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -w .

vet:
	go vet ./...
