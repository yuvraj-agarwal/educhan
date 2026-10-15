#!/usr/bin/env bash
# First deployment of educhan to Cloud Run, in one command, from Cloud Shell.
#
#   bash deploy/first-deploy.sh PROJECT_ID [REGION]
#
# REGION defaults to asia-south1. The script follows deploy/README.md section 2
# step by step and is safe to re-run: everything it creates is checked for
# first, so a run that stopped halfway picks up where it left off, and a re-run
# from a newer checkout moves both the service and the archive sweeper Job onto
# the new image. At the end it prints the service URL and how to read the
# moderation key.
#
# What it leaves switched off, on purpose:
#   - image uploads (EDUCHAN_UPLOADS_ENABLED=false): images are not yet stripped
#     of metadata or scanned;
#   - institutional verification (no EDUCHAN_VERIFY_SECRET): there is no real
#     mail sender yet, and the development one would print addresses into
#     Cloud Logging.
#
# It also excludes the platform request logs, which carry every visitor's IP,
# from Cloud Logging (deploy/README.md section 2.8).
#
# Cost: this starts a db-f1-micro Cloud SQL instance and keeps one Cloud Run
# instance warm (MIN_INSTANCES=1, for the demo window, so the first visitor
# sees no cold start). Both bill around the clock whether or not anyone visits.
# To drop the warm instance after the demo:
#   gcloud run services update educhan --region REGION --min-instances 0
# and to stop the database charge: gcloud sql instances delete educhan-db
set -euo pipefail

PROJECT_ID="${1:-${PROJECT_ID:-}}"
REGION="${2:-${REGION:-asia-south1}}"
MIN_INSTANCES="${MIN_INSTANCES:-1}"
if [[ -z "$PROJECT_ID" ]]; then
  echo "usage: bash deploy/first-deploy.sh PROJECT_ID [REGION]" >&2
  exit 2
fi

SERVICE=educhan
DB_INSTANCE=educhan-db
AR_REPO=educhan
SWEEP_JOB=educhan-sweep
SWEEP_SCHEDULE=educhan-sweep-daily
RUN_SA_NAME=educhan-run
BUILD_SA_NAME=educhan-build
SCHEDULER_SA_NAME=educhan-scheduler
RUN_SA="${RUN_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
BUILD_SA="${BUILD_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
SCHEDULER_SA="${SCHEDULER_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
MEDIA_BUCKET="${PROJECT_ID}-educhan-media"
# The service and the sweeper Job must agree on these exactly, or the sweep
# looks for files where the server never put them.
MEDIA_ENV="EDUCHAN_MEDIA_BACKEND=gcs,EDUCHAN_GCS_BUCKET=${MEDIA_BUCKET},EDUCHAN_GCS_PREFIX=media"
STAGING_BUCKET="${PROJECT_ID}_cloudbuild"

step() { printf '\n==> %s\n' "$*"; }
exists() { "$@" >/dev/null 2>&1; }

cd "$(dirname "$0")/.."

# This checkout must already default uploads to off, cap the database pool,
# and have the -sweep mode the archive sweeper Job runs. An older one would
# deploy with uploads accepted and nothing scanned, or a Job that cannot start.
if ! grep -q EDUCHAN_UPLOADS_ENABLED internal/app/config.go ||
   ! grep -q EDUCHAN_DB_MAX_CONNS internal/app/config.go ||
   ! grep -q '"sweep"' cmd/server/main.go; then
  echo "This checkout predates the uploads switch, the pool cap or the sweeper." >&2
  echo "Pull the latest main and run the script again." >&2
  exit 1
fi
IMAGE_TAG="$(git rev-parse --short HEAD)"
IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/educhan:${IMAGE_TAG}"

step "Project ${PROJECT_ID}, region ${REGION}"
gcloud config set project "$PROJECT_ID" >/dev/null
# Empty means the answer could not be read (the billing API may be off); the
# first API call that needs billing will then say so itself.
if [[ "$(gcloud billing projects describe "$PROJECT_ID" --format='value(billingEnabled)' 2>/dev/null)" == "False" ]]; then
  echo "Billing is not enabled on ${PROJECT_ID}. Link a billing account in the console" >&2
  echo "(Billing > Link a billing account) and run the script again." >&2
  exit 1
fi

step "Enabling APIs (the first time this takes a minute or two)"
gcloud services enable \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  run.googleapis.com \
  sqladmin.googleapis.com \
  secretmanager.googleapis.com \
  storage.googleapis.com \
  logging.googleapis.com \
  containerscanning.googleapis.com \
  cloudscheduler.googleapis.com

step "Artifact Registry repository"
exists gcloud artifacts repositories describe "$AR_REPO" --location="$REGION" ||
  gcloud artifacts repositories create "$AR_REPO" \
    --repository-format=docker --location="$REGION" \
    --description="educhan container images"

step "Service accounts"
exists gcloud iam service-accounts describe "$RUN_SA" ||
  gcloud iam service-accounts create "$RUN_SA_NAME" --display-name="educhan Cloud Run"
# Builds run as an account of their own rather than whichever default account
# this project's age implies, so the grants below are always the right ones.
exists gcloud iam service-accounts describe "$BUILD_SA" ||
  gcloud iam service-accounts create "$BUILD_SA_NAME" --display-name="educhan Cloud Build"
# Cloud Scheduler starts the sweeper as this account, which may run that one
# Job and nothing else.
exists gcloud iam service-accounts describe "$SCHEDULER_SA" ||
  gcloud iam service-accounts create "$SCHEDULER_SA_NAME" --display-name="educhan sweeper schedule"
# A freshly created account can take a few seconds to be usable in IAM policies.
for sa in "$BUILD_SA" "$SCHEDULER_SA"; do
  for _ in 1 2 3 4 5 6; do
    exists gcloud iam service-accounts describe "$sa" && break
    sleep 5
  done
done

step "IAM grants"
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${RUN_SA}" --role=roles/cloudsql.client --condition=None >/dev/null
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${BUILD_SA}" --role=roles/logging.logWriter --condition=None >/dev/null
gcloud artifacts repositories add-iam-policy-binding "$AR_REPO" --location="$REGION" \
  --member="serviceAccount:${BUILD_SA}" --role=roles/artifactregistry.writer >/dev/null
# gcloud builds submit uploads the source here and the build reads it back.
exists gcloud storage buckets describe "gs://${STAGING_BUCKET}" ||
  gcloud storage buckets create "gs://${STAGING_BUCKET}" \
    --location="$REGION" --uniform-bucket-level-access
gcloud storage buckets add-iam-policy-binding "gs://${STAGING_BUCKET}" \
  --member="serviceAccount:${BUILD_SA}" --role=roles/storage.objectViewer >/dev/null

step "Cloud SQL instance ${DB_INSTANCE} (creating one takes 5 to 10 minutes)"
exists gcloud sql instances describe "$DB_INSTANCE" ||
  gcloud sql instances create "$DB_INSTANCE" \
    --database-version=POSTGRES_16 \
    --edition=ENTERPRISE \
    --tier=db-f1-micro \
    --region="$REGION" \
    --storage-size=10GB
gcloud sql databases describe educhan --instance="$DB_INSTANCE" >/dev/null 2>&1 ||
  gcloud sql databases create educhan --instance="$DB_INSTANCE"
CLOUDSQL_INSTANCE="$(gcloud sql instances describe "$DB_INSTANCE" --format='value(connectionName)')"

step "Media bucket gs://${MEDIA_BUCKET}"
exists gcloud storage buckets describe "gs://${MEDIA_BUCKET}" ||
  gcloud storage buckets create "gs://${MEDIA_BUCKET}" \
    --location="$REGION" --uniform-bucket-level-access --public-access-prevention
gcloud storage buckets add-iam-policy-binding "gs://${MEDIA_BUCKET}" \
  --member="serviceAccount:${RUN_SA}" --role=roles/storage.objectAdmin >/dev/null

step "Secrets"
# The database password exists only inside the database-url secret. If that
# secret is missing, set a new password on the user (creating it if needed), so
# a re-run after a failure never leaves the two out of step.
if ! exists gcloud secrets describe educhan-database-url; then
  DB_PASSWORD="$(openssl rand -hex 24)"
  # Read into a variable first: with pipefail, grep -q closing the pipe early
  # could fail the condition on SIGPIPE.
  db_users="$(gcloud sql users list --instance="$DB_INSTANCE" --format='value(name)')"
  if grep -qx educhan <<<"$db_users"; then
    gcloud sql users set-password educhan --instance="$DB_INSTANCE" --password="$DB_PASSWORD"
  else
    gcloud sql users create educhan --instance="$DB_INSTANCE" --password="$DB_PASSWORD"
  fi
  printf 'postgres://educhan:%s@/educhan?host=/cloudsql/%s&sslmode=disable' \
    "$DB_PASSWORD" "$CLOUDSQL_INSTANCE" |
    gcloud secrets create educhan-database-url --replication-policy=automatic --data-file=-
  unset DB_PASSWORD
fi
# Created once and never rotated by this script: the salt keys every poster ID.
for secret in educhan-poster-id-salt educhan-mod-key educhan-captcha-secret; do
  exists gcloud secrets describe "$secret" ||
    openssl rand -hex 32 | gcloud secrets create "$secret" --replication-policy=automatic --data-file=-
done
for secret in educhan-database-url educhan-poster-id-salt educhan-mod-key educhan-captcha-secret; do
  gcloud secrets add-iam-policy-binding "$secret" \
    --member="serviceAccount:${RUN_SA}" --role=roles/secretmanager.secretAccessor >/dev/null
done

step "Building ${IMAGE} on Cloud Build (a few minutes)"
gcloud builds submit \
  --config cloudbuild.yaml \
  --service-account="projects/${PROJECT_ID}/serviceAccounts/${BUILD_SA}" \
  --substitutions "_REGION=${REGION},_REPOSITORY=${AR_REPO},_IMAGE=educhan,_TAG=${IMAGE_TAG}"

# Before the service exists, so not even its first request is logged with an IP.
step "Keeping request logs (which carry client IPs) out of Cloud Logging"
exclusions="$(gcloud logging sinks describe _Default --format='value(exclusions[].name)')"
if grep -qw no-request-logs <<<"$exclusions"; then
  echo "already excluded"
else
  gcloud logging sinks update _Default \
    --add-exclusion='name=no-request-logs,description=Request logs carry client IPs,filter=LOG_ID("run.googleapis.com/requests") OR resource.type="http_load_balancer"'
fi

# Organisations created since May 2024 enforce a policy that refuses allUsers
# as an invoker. There, skip the invoker check instead: the service is still
# public, and ingress is what limits who can reach it.
if [[ "$(gcloud projects describe "$PROJECT_ID" --format='value(parent.type)')" == "organization" ]]; then
  PUBLIC_ACCESS=(--no-invoker-iam-check)
else
  PUBLIC_ACCESS=(--allow-unauthenticated)
fi

step "Deploying the Cloud Run service"
gcloud run deploy "$SERVICE" \
  --image "$IMAGE" \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest,EDUCHAN_POSTER_ID_SALT=educhan-poster-id-salt:latest,EDUCHAN_MOD_KEY=educhan-mod-key:latest,EDUCHAN_CAPTCHA_SECRET=educhan-captcha-secret:latest" \
  --set-env-vars "EDUCHAN_TRUST_PROXY=1,${MEDIA_ENV},EDUCHAN_LOG_FORMAT=json,EDUCHAN_UPLOADS_ENABLED=false,EDUCHAN_DB_MAX_CONNS=3" \
  --cpu 1 --memory 512Mi --concurrency 80 \
  --min-instances "$MIN_INSTANCES" --max-instances 4 \
  "${PUBLIC_ACCESS[@]}"

# After the service, whose first instance applies any new migrations: the
# sweeper refuses to run on a schema newer than its image, so it moves onto
# every new image with the service (deploy/README.md section 2.9).
step "Archive sweeper: Cloud Run Job ${SWEEP_JOB}, run daily by Cloud Scheduler"
gcloud run jobs deploy "$SWEEP_JOB" \
  --image "$IMAGE" \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --args=-sweep \
  --set-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest" \
  --set-env-vars "${MEDIA_ENV},EDUCHAN_LOG_FORMAT=json,EDUCHAN_DB_MAX_CONNS=3" \
  --tasks 1 --max-retries 1 --task-timeout 15m \
  --cpu 1 --memory 512Mi
gcloud run jobs add-iam-policy-binding "$SWEEP_JOB" --region "$REGION" \
  --member="serviceAccount:${SCHEDULER_SA}" --role=roles/run.invoker >/dev/null
SCHEDULE_ARGS=(
  --location "$REGION"
  --schedule "17 3 * * *" --time-zone "Etc/UTC"
  --http-method POST
  --uri "https://run.googleapis.com/v2/projects/${PROJECT_ID}/locations/${REGION}/jobs/${SWEEP_JOB}:run"
  --oauth-service-account-email "$SCHEDULER_SA"
)
if exists gcloud scheduler jobs describe "$SWEEP_SCHEDULE" --location "$REGION"; then
  gcloud scheduler jobs update http "$SWEEP_SCHEDULE" "${SCHEDULE_ARGS[@]}" >/dev/null
else
  gcloud scheduler jobs create http "$SWEEP_SCHEDULE" "${SCHEDULE_ARGS[@]}" >/dev/null
fi

URL="$(gcloud run services describe "$SERVICE" --region "$REGION" --format='value(status.url)')"
# Not /healthz: Cloud Run reserves some paths ending in "z" on run.app URLs.
# The board list also reads the database, so it proves the same thing.
step "Checking ${URL}/api/boards"
for _ in 1 2 3 4 5 6; do
  if curl -fsS -o /dev/null "${URL}/api/boards"; then echo "ok"; break; fi
  sleep 5
done

cat <<EOF

educhan is live at:

  ${URL}

Moderation key (keep it private; it goes in the X-Mod-Key header):

  gcloud secrets versions access latest --secret=educhan-mod-key

Image uploads and institutional verification are switched off.

Expired archived threads are deleted daily at 03:17 UTC by the ${SWEEP_JOB} Job. To run
it now and see what it did:

  gcloud run jobs execute ${SWEEP_JOB} --region ${REGION} --wait

Check that no log holds a visitor IP: open the URL from your phone or laptop,
search "what is my ip" to learn that device's address, wait a minute, then
run this here. It must print nothing.

  gcloud logging read '"YOUR.IP.ADDRESS"' --freshness=1d --limit=5

After the demo, stop paying for the warm instance:

  gcloud run services update ${SERVICE} --region ${REGION} --min-instances 0

To ship a new version later, git pull in this folder and run the same command
again: it skips what already exists, builds the new commit, and moves both the
service and the sweeper Job onto it.

  bash deploy/first-deploy.sh ${PROJECT_ID} ${REGION}
EOF
