.PHONY: run build test check tidy fmt vet db-up cloud-preflight cloud-build cloud-deploy cloud-image

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

# --- Google Cloud -----------------------------------------------------------
#
# The container image is built by Cloud Build from the `Dockerfile` and pushed
# to Artifact Registry, so neither target below needs a Docker daemon — only
# gcloud and a project. Local development does not use the image at all: that is
# `make db-up` plus `make run`.
#
# Override any of these on the command line:
#
#   make cloud-build REGION=europe-west1 PROJECT_ID=my-project
#
# One-time project setup (APIs, the Artifact Registry repository, and the IAM
# the build service account needs) is in deploy/README.md section 2.1.

PROJECT_ID ?= $(shell gcloud config get-value project 2>/dev/null)
REGION     ?= asia-south1
AR_REPO    ?= educhan
IMAGE_NAME ?= educhan
SERVICE    ?= educhan
# The image carries this alongside `latest`, so a revision names the commit it
# was built from and a rollback has something immutable to point at.
IMAGE_TAG  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
IMAGE       = $(REGION)-docker.pkg.dev/$(PROJECT_ID)/$(AR_REPO)/$(IMAGE_NAME):$(IMAGE_TAG)

# Fail here, legibly, rather than submitting a build whose image path has an
# empty project or an empty tag in it.
cloud-preflight:
	@command -v gcloud >/dev/null || \
	  { echo "gcloud not found: https://cloud.google.com/sdk/docs/install" >&2; exit 1; }
	@test -n "$(PROJECT_ID)" && test "$(PROJECT_ID)" != "(unset)" || \
	  { echo "PROJECT_ID is not set. Pass PROJECT_ID=… or run: gcloud config set project <id>" >&2; exit 1; }
	@test -n "$(IMAGE_TAG)" || \
	  { echo "IMAGE_TAG is empty (no git checkout?). Pass IMAGE_TAG=…" >&2; exit 1; }

# The image reference the other two targets work with. Useful on its own for
# `envsubst < deploy/cloudrun/service.yaml`.
cloud-image: cloud-preflight
	@echo "$(IMAGE)"

# Build on Cloud Build and push to Artifact Registry, tagged with the short
# commit SHA and `latest`.
cloud-build: cloud-preflight
	gcloud builds submit \
	  --project "$(PROJECT_ID)" \
	  --config cloudbuild.yaml \
	  --substitutions "_REGION=$(REGION),_REPOSITORY=$(AR_REPO),_IMAGE=$(IMAGE_NAME),_TAG=$(IMAGE_TAG)"

# Roll that image onto the existing Cloud Run service. Only the image changes:
# `gcloud run deploy` carries every other setting — service account, Cloud SQL
# instance, secrets, environment — over from the current revision, so the
# service's configuration stays in deploy/cloudrun/service.yaml rather than
# being duplicated here. Creating the service the first time is a longer
# command; see deploy/README.md section 2.6.
cloud-deploy: cloud-preflight
	@gcloud run services describe "$(SERVICE)" --project "$(PROJECT_ID)" --region "$(REGION)" >/dev/null 2>&1 || \
	  { echo "Cloud Run service '$(SERVICE)' does not exist in $(REGION); create it first — deploy/README.md section 2.6." >&2; exit 1; }
	gcloud run deploy "$(SERVICE)" \
	  --project "$(PROJECT_ID)" \
	  --region "$(REGION)" \
	  --image "$(IMAGE)"
