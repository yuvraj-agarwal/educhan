#!/usr/bin/env bash
# First deployment of educhan to Cloud Run, in one command, from Cloud Shell.
#
#   bash deploy/first-deploy.sh PROJECT_ID [REGION]
#   DOMAIN=example.org bash deploy/first-deploy.sh PROJECT_ID [REGION]
#
# REGION defaults to asia-south1. The script follows deploy/README.md section 2
# step by step and is safe to re-run: everything it creates is checked for
# first, so a run that stopped halfway picks up where it left off, and a re-run
# from a newer checkout moves both the service and the archive sweeper Job onto
# the new image. At the end it prints the service URL and how to read the
# moderation key.
#
# With DOMAIN it also puts the site on that domain (deploy/README.md section
# 2.10): an HTTPS load balancer with a Google-managed certificate and a Cloud
# Armor flood limit, the www. name redirected to the bare one, and, once the
# domain's DNS record and certificate are in place, the run.app address closed
# to the public, so every visitor comes through the load balancer. It prints
# the DNS records to add. Once the load balancer exists, later runs find it and
# keep the service behind it without DOMAIN.
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
# DOMAIN adds the load balancer's forwarding rules and the Cloud Armor policy,
# which also bill around the clock (deploy/README.md section 2.10 has the sums).
set -euo pipefail

PROJECT_ID="${1:-${PROJECT_ID:-}}"
REGION="${2:-${REGION:-asia-south1}}"
MIN_INSTANCES="${MIN_INSTANCES:-1}"
DOMAIN="${DOMAIN:-}"
# Requests a minute from one address before Cloud Armor answers 429. Unset, a
# new load balancer gets 1200 and an existing one keeps the limit it has.
EDGE_RATE="${EDGE_RATE:-}"
if [[ -z "$PROJECT_ID" ]]; then
  echo "usage: [DOMAIN=example.org] bash deploy/first-deploy.sh PROJECT_ID [REGION]" >&2
  exit 2
fi
# In the C locale, where a-z and 0-9 are those ASCII characters and no others,
# whatever the shell's own locale would count as a letter or a digit.
matches() {
  local LC_ALL=C
  [[ "$1" =~ $2 ]]
}
# A bare name in lower case: no https://, no path, no port, no trailing dot.
if [[ -n "$DOMAIN" ]] && ! matches "$DOMAIN" '^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]([a-z0-9-]*[a-z0-9])?$'; then
  echo "DOMAIN must be a bare name in lower case, such as educhan.org (not ${DOMAIN})." >&2
  exit 2
fi
if [[ -n "$EDGE_RATE" ]] && ! matches "$EDGE_RATE" '^[1-9][0-9]*$'; then
  echo "EDGE_RATE must be a whole number of requests a minute, such as 1200 (not ${EDGE_RATE})." >&2
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
# The load balancer DOMAIN builds (deploy/README.md section 2.10). Names are
# unique per kind of resource, so a proxy and its forwarding rule share one.
EDGE_IP=educhan-ip
EDGE_NEG=educhan-neg
EDGE_BACKEND=educhan-backend
EDGE_POLICY=educhan-edge
EDGE_CERT=educhan-cert
EDGE_WWW_CERT=educhan-cert-www
EDGE_TLS=educhan-tls
EDGE_MAP=educhan
EDGE_HTTPS=educhan-https
EDGE_HTTP=educhan-http
CLOSED=internal-and-cloud-load-balancing

step() { printf '\n==> %s\n' "$*"; }
# Is it there? The answer is thrown away, so the command gets no terminal to
# ask a question on either: gcloud would otherwise wait, unseen, for a reply
# (it offers to enable an API that has only just been enabled).
exists() { "$@" </dev/null >/dev/null 2>&1; }
# A read that decides what the script does next, where "could not read" must
# never pass for "nothing there". It is tried for a minute, because a newly
# enabled API can go on refusing calls that long; a failure after that stops
# the script.
must_read() {
  local out
  for _ in 1 2 3 4 5 6; do
    if out="$("$@" </dev/null)"; then
      printf '%s\n' "$out"
      return 0
    fi
    sleep 10
  done
  echo "That read failed six times in a row (its error is above), so the script stops here." >&2
  return 1
}

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

# A load balancer this script built is found again from its certificate, so a
# run without DOMAIN keeps the service behind it. A run that put the service
# back to EDUCHAN_TRUST_PROXY=1 there would key every visitor's flood control
# on the load balancer's address, and one that opened run.app again would let
# a client forge its own (deploy/README.md section 2.10). These are lists, not
# describes, and awk picks the one name out: a list of nothing succeeds, so
# only a real failure is a failure.
COMPUTE_API="$(must_read gcloud services list --enabled --format='value(config.name)' |
  awk '$1 == "compute.googleapis.com"')"
EDGE_DOMAIN=""
LEFTOVER=""
if [[ -n "$COMPUTE_API" ]]; then
  EDGE_CERTS="$(must_read gcloud compute ssl-certificates list --global --format='value(name,managed.domains[0])')"
  EDGE_DOMAIN="$(awk -v name="$EDGE_CERT" '$1 == name { print $2 }' <<<"$EDGE_CERTS")"
  if [[ -z "$EDGE_DOMAIN" ]]; then
    # While the bare name's certificate is being replaced (section 2.10), the
    # www. one still says which domain this load balancer serves.
    EDGE_DOMAIN="$(awk -v name="$EDGE_WWW_CERT" '$1 == name { sub(/^www[.]/, "", $2); print $2 }' <<<"$EDGE_CERTS")"
  fi
  if [[ -z "$EDGE_DOMAIN" && -z "$DOMAIN" ]]; then
    # The address that a run with DOMAIN reserved and then never used.
    LEFTOVER="$(must_read gcloud compute addresses list --global --format='value(name,status)' |
      awk -v name="$EDGE_IP" '$1 == name && $2 == "RESERVED" { print $1 }')"
  fi
fi
# The service as it stands, as "ingress wanted|ingress in force|traffic": the
# ingress setting; the one Cloud Run is applying right now, which trails the
# first while a change goes through; and where requests go, which holds
# "True=100" when all of them go to the latest revision. Nothing at all for a
# service that is not deployed yet.
service_state() {
  gcloud run services list --region "$REGION" \
    --format='value[separator="|"](metadata.name,metadata.annotations."run.googleapis.com/ingress",metadata.annotations."run.googleapis.com/ingress-status",spec.traffic.map().format("{0}={1}", latestRevision, percent).list())' |
    awk -F'|' -v name="$SERVICE" '$1 == name { print $2 "|" $3 "|" $4 }'
}
read_service() {
  local state
  state="$(must_read service_state)" || return 1
  IFS='|' read -r INGRESS IN_FORCE TRAFFIC <<<"$state"
}
# A rollback made in the console pins traffic to one revision, and a pinned
# service goes on serving that revision whatever is deployed after it.
pinned() { [[ -n "$TRAFFIC" && ",${TRAFFIC}," != *",True=100,"* ]]; }
read_service
if pinned; then
  cat <<EOF

NOTE: traffic for ${SERVICE} is pinned to an older revision, as a rollback made
in the console leaves it. This run deploys this checkout (${IMAGE_TAG}) and
then sends all traffic to it. Press Ctrl-C now if the pin should stay.
EOF
fi
if [[ -n "$INGRESS" && "$INGRESS" != all && "$INGRESS" != "$CLOSED" ]]; then
  cat >&2 <<EOF
The ${SERVICE} service's ingress is "${INGRESS}": neither the internet nor a load
balancer can reach it, so the site is offline. That was set by hand, and this
script leaves it alone. To bring the site back, run the first block of commands
under "Going back to run.app" in deploy/README.md section 2.10, which opens
run.app again with the right count, and then this script.
EOF
  exit 1
fi
if [[ -n "$EDGE_DOMAIN" ]]; then
  if [[ -n "$DOMAIN" && "$DOMAIN" != "$EDGE_DOMAIN" ]]; then
    echo "This project's load balancer already serves ${EDGE_DOMAIN}, not ${DOMAIN}." >&2
    echo "Moving to another domain is not a re-run: see deploy/README.md section 2.10." >&2
    exit 1
  fi
  DOMAIN="$EDGE_DOMAIN"
elif [[ -z "$DOMAIN" && -n "$INGRESS" && "$INGRESS" != all ]]; then
  echo "The ${SERVICE} service takes no traffic from the internet (ingress: ${INGRESS}), and this" >&2
  echo "script found no load balancer of its own in front of it. Run it with DOMAIN=your.domain" >&2
  echo "to build one, or see deploy/README.md section 2.10 to go back to the run.app address." >&2
  exit 1
fi

if [[ -n "$DOMAIN" ]]; then
  # www.DOMAIN is redirected to DOMAIN, unless DOMAIN is a www. name itself.
  WWW="www.${DOMAIN}"
  if [[ "$DOMAIN" == www.* ]]; then WWW=""; fi

  step "Address for ${DOMAIN}"
  gcloud services enable compute.googleapis.com
  if [[ -z "$COMPUTE_API" ]]; then
    # Only just enabled, the API can go on refusing calls for a minute.
    for _ in 1 2 3 4 5 6; do
      exists gcloud compute addresses list --global && break
      sleep 10
    done
  fi
  # An IPv4 address and no IPv6 one, on purpose. Flood control, the app's and
  # Cloud Armor's, counts by address, and an IPv6 subscriber holds a whole /64
  # of them to move between. IPv6 waits for the app to count by /64
  # (deploy/README.md section 2.10).
  exists gcloud compute addresses describe "$EDGE_IP" --global ||
    gcloud compute addresses create "$EDGE_IP" --global --ip-version=IPV4
  EDGE_ADDRESS="$(gcloud compute addresses describe "$EDGE_IP" --global --format='value(address)')"
  dns_records() {
    printf '    %-5s %-28s %s\n' Type Name Content A "$DOMAIN" "$EDGE_ADDRESS"
    if [[ -n "$WWW" ]]; then
      printf '    %-5s %-28s %s\n' A "$WWW" "$EDGE_ADDRESS"
    fi
  }
  if [[ "$INGRESS" == "$CLOSED" ]]; then
    cat <<EOF

The DNS records for ${DOMAIN}, as they should already stand. In Cloudflare
each has Proxy status "DNS only" (the grey cloud).

$(dns_records)
EOF
  else
    cat <<EOF

Add these DNS records for ${DOMAIN} now, while the rest of this runs. In
Cloudflare that is DNS > Records, with Proxy status "DNS only" (the grey
cloud) on each: proxied, Cloudflare would see every visitor's address, and
Google could not issue the certificate. Delete any other A, AAAA or CNAME
record on these names first; the load balancer answers on IPv4 only, so there
is no AAAA record to add.

$(dns_records)
EOF
  fi
fi

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
# Daily backups, seven kept, in a window opening at 05:00 UTC: after the 03:17
# sweep (below), so a backup holds as little as possible of what the sweep
# deletes or rewrites. 05:00 UTC is also when a verification credential's day
# ends (credentialDayStart in internal/app/verify.go), so none of the three
# moves alone (deploy/README.md sections 2.2 and 2.9). Point-in-time recovery
# stays off, the gcloud default for this edition, and is never changed here.
# deploy/README.md section 2.2 says what each of these keeps. An existing
# instance with backups on gets only the start time moved, so a re-run never
# shortens a retention someone has raised and never turns backups back on where
# someone turned them off. BACKUP_START is the window's start for the check,
# the move and the create alike, so changing it here changes all three.
BACKUP_START=05:00
if exists gcloud sql instances describe "$DB_INSTANCE"; then
  BACKUP_STATE="$(gcloud sql instances describe "$DB_INSTANCE" \
    --format='value(settings.backupConfiguration.enabled,settings.backupConfiguration.startTime)')"
  if [[ "$BACKUP_STATE" == True* && "$BACKUP_STATE" != *"$BACKUP_START" ]]; then
    gcloud sql instances patch "$DB_INSTANCE" --quiet --backup-start-time="$BACKUP_START" >/dev/null ||
      echo "Could not move the backup window to ${BACKUP_START} UTC; see deploy/README.md section 2.2." >&2
  fi
else
  gcloud sql instances create "$DB_INSTANCE" \
    --database-version=POSTGRES_16 \
    --edition=ENTERPRISE \
    --tier=db-f1-micro \
    --region="$REGION" \
    --storage-size=10GB \
    --backup --backup-start-time="$BACKUP_START" --retained-backups-count=7
fi
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
for secret in educhan-poster-id-salt educhan-mod-key; do
  exists gcloud secrets describe "$secret" ||
    openssl rand -hex 32 | gcloud secrets create "$secret" --replication-policy=automatic --data-file=-
done
for secret in educhan-database-url educhan-poster-id-salt educhan-mod-key; do
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
# Put back on every run, not only added on the first: an exclusion that someone
# disabled or edited while debugging keeps its name, and a check of the name
# alone would pass it while every visitor's address went into the log. An empty
# "disabled=" is how gcloud spells "enabled".
EXCLUSION='description=Request logs carry client IPs,filter=LOG_ID("run.googleapis.com/requests") OR resource.type="http_load_balancer"'
exclusions="$(gcloud logging sinks describe _Default --format='value(exclusions[].name)')"
if grep -qx no-request-logs <<<"${exclusions//;/$'\n'}"; then
  gcloud logging sinks update _Default --update-exclusion="name=no-request-logs,disabled=,${EXCLUSION}"
else
  gcloud logging sinks update _Default --add-exclusion="name=no-request-logs,${EXCLUSION}"
fi

# Organisations created since May 2024 enforce a policy that refuses allUsers
# as an invoker. There, skip the invoker check instead: the service is still
# public, and ingress is what limits who can reach it. The whole ancestry, not
# just the parent: a project in a folder has the organisation further up. If
# the ancestry can't be read, allUsers is tried, and the check at the end says
# whether the service answers.
ancestors="$(gcloud projects get-ancestors "$PROJECT_ID" --format='value(type)' || true)"
if grep -qx organization <<<"$ancestors"; then
  PUBLIC_ACCESS=(--no-invoker-iam-check)
else
  PUBLIC_ACCESS=(--allow-unauthenticated)
fi

# EDUCHAN_TRUST_PROXY is how many entries at the end of X-Forwarded-For were
# written by proxies the app can trust: one while visitors use run.app (Cloud
# Run's front end adds the caller's address), two behind the load balancer,
# which adds the visitor's address and then its own. Two is only right with
# run.app closed: open, a client could send an X-Forwarded-For of its own
# straight to run.app and choose its flood-control bucket. One with run.app
# closed is the lesser fault: every visitor shares the load balancer's bucket.
# So the count is two only where the closing is in force, read again here
# because the build above took minutes, and it is raised for the first time by
# the switch further down, after run.app has closed (deploy/README.md section
# 2.10).
read_service
TRUST_PROXY=1
EDGE_INGRESS=()
if [[ -n "$DOMAIN" && "$INGRESS" == "$CLOSED" && "$IN_FORCE" == "$CLOSED" ]]; then
  TRUST_PROXY=2
  EDGE_INGRESS=(--ingress "$CLOSED")
fi

step "Deploying the Cloud Run service"
# --update-*, not --set-*: a re-run sets the values below and keeps any other
# variable or secret added by hand since, where --set-* would drop it and switch
# that feature off without a word. Two things are enforced on every run all the
# same: uploads stay off, and verification stays off (its secret is removed,
# whether it was set as a secret or a plain variable) until a mail sender exists.
gcloud run deploy "$SERVICE" \
  --image "$IMAGE" \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --update-secrets "DATABASE_URL=educhan-database-url:latest,EDUCHAN_POSTER_ID_SALT=educhan-poster-id-salt:latest,EDUCHAN_MOD_KEY=educhan-mod-key:latest" \
  --remove-secrets EDUCHAN_VERIFY_SECRET \
  --update-env-vars "EDUCHAN_TRUST_PROXY=${TRUST_PROXY},${MEDIA_ENV},EDUCHAN_LOG_FORMAT=json,EDUCHAN_UPLOADS_ENABLED=false,EDUCHAN_DB_MAX_CONNS=3" \
  --remove-env-vars EDUCHAN_VERIFY_SECRET \
  --cpu 1 --memory 512Mi --concurrency 80 \
  --min-instances "$MIN_INSTANCES" --max-instances 4 \
  "${PUBLIC_ACCESS[@]}" ${EDGE_INGRESS[@]+"${EDGE_INGRESS[@]}"}
# With traffic pinned (above), the revision just made, with this run's image
# and count, serves nobody. A run of this script ships its checkout, so it ends
# the pin, and only now, so that no revision in between is served on the way.
if pinned; then
  step "Sending traffic to the new revision (it was pinned to an older one)"
  gcloud run services update-traffic "$SERVICE" --region "$REGION" --to-latest
fi

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
  --update-secrets "DATABASE_URL=educhan-database-url:latest" \
  --update-env-vars "${MEDIA_ENV},EDUCHAN_LOG_FORMAT=json,EDUCHAN_DB_MAX_CONNS=3" \
  --tasks 1 --max-retries 1 --task-timeout 15m \
  --cpu 1 --memory 512Mi
gcloud run jobs add-iam-policy-binding "$SWEEP_JOB" --region "$REGION" \
  --member="serviceAccount:${SCHEDULER_SA}" --role=roles/run.invoker >/dev/null
SCHEDULE_ARGS=(
  --location "$REGION"
  # Done, retry included, half an hour or more before 05:00 UTC, when the backup
  # window opens and a verification credential's day ends; with this retry and
  # timeout that allows 03:00 to 04:00 (TestCredentialDaysEndAfterTheSweep).
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
# What run.app answers a request from here, which is outside the project's
# network: 404 once ingress refuses it, 000 if nothing answered at all.
run_app_code() { curl -s -o /dev/null --max-time 20 -w '%{http_code}' "${URL}/api/boards" || true; }
# Does the site answer? Not /healthz: Cloud Run reserves some paths ending in
# "z" on run.app URLs. The board list also reads the database, so it proves the
# same thing.
SITE="$URL"
check=(curl -fsS -o /dev/null "${URL}/api/boards")
tries=6
answered=""
checked=""
check_site() {
  step "Checking ${SITE}/api/boards"
  answered=""
  checked=1
  for ((try = 0; try < tries; try++)); do
    if "${check[@]}"; then
      answered=1
      echo "ok"
      break
    fi
    sleep 5
  done
}
DOMAIN_LIVE=""
RUN_APP_CLOSED=""
CERT_STATE=""
EDGE_ERROR=""
APEX_AT=""
DNS_NOTES=""
CERT_NOTES=""
SPARE_CERTS=""
STOPPED=""

if [[ -n "$DOMAIN" ]]; then
  step "Load balancer for ${DOMAIN}"
  # How the load balancer reaches the service: a serverless network endpoint
  # group in the service's region, behind a global load balancer.
  exists gcloud compute network-endpoint-groups describe "$EDGE_NEG" --region "$REGION" ||
    gcloud compute network-endpoint-groups create "$EDGE_NEG" --region "$REGION" \
      --network-endpoint-type=serverless --cloud-run-service="$SERVICE"

  # Cloud Armor's one rule: a flood limit per address, on every request. It
  # uses the visitor's address for the count and writes no log entry of its
  # own: what it decides goes in the load balancer's request log, which is off
  # (below). The limit is generous on purpose: a campus, or a mobile network's
  # shared IPv4 address, puts many readers behind one address, and the app's
  # own cooldowns limit posting.
  armor_quota_hint() {
    cat >&2 <<EOF

If the error above says a SECURITY_POLICIES or SECURITY_POLICY_RULES quota is
exceeded with a limit of 0, this project has no Cloud Armor allowance at all,
which is reported for billing accounts still on the free trial. Activating the
full account (console: Billing) is the fix to try; then run this script again.
Meanwhile the site stays at the address it had.
EOF
  }
  exists gcloud compute security-policies describe "$EDGE_POLICY" --global ||
    gcloud compute security-policies create "$EDGE_POLICY" --global \
      --description="educhan: a flood limit per address" ||
    { armor_quota_hint; exit 1; }
  # Without EDGE_RATE, a rule that is already there keeps its limit, so one
  # tuned by hand, in a flood say, outlives the next deploy.
  rate="$EDGE_RATE"
  rule_verb=create
  if exists gcloud compute security-policies rules describe 1000 --security-policy "$EDGE_POLICY"; then
    rule_verb=update
    if [[ -z "$rate" ]]; then
      rate="$(gcloud compute security-policies rules describe 1000 --security-policy "$EDGE_POLICY" \
        --format='value(rateLimitOptions.rateLimitThreshold.count)')"
    fi
  fi
  rate="${rate:-1200}"
  gcloud compute security-policies rules "$rule_verb" 1000 \
    --security-policy "$EDGE_POLICY" \
    --description "At most ${rate} requests a minute from one address" \
    --src-ip-ranges '*' \
    --action throttle --conform-action allow --exceed-action deny-429 \
    --rate-limit-threshold-count "$rate" --rate-limit-threshold-interval-sec 60 \
    --enforce-on-key IP >/dev/null ||
    { armor_quota_hint; exit 1; }
  echo "Cloud Armor answers 429 past ${rate} requests a minute from one address."

  exists gcloud compute backend-services describe "$EDGE_BACKEND" --global ||
    gcloud compute backend-services create "$EDGE_BACKEND" --global \
      --load-balancing-scheme=EXTERNAL_MANAGED --no-enable-logging
  neg_groups="$(gcloud compute backend-services describe "$EDGE_BACKEND" --global \
    --format='value(backends[].group.basename())')"
  if ! grep -qx "$EDGE_NEG" <<<"${neg_groups//;/$'\n'}"; then
    gcloud compute backend-services add-backend "$EDGE_BACKEND" --global \
      --network-endpoint-group="$EDGE_NEG" --network-endpoint-group-region="$REGION" >/dev/null
  fi
  # On every run, not only the first: request logging off, since each entry
  # holds the visitor's address and the time (deploy/README.md section 2.8);
  # the Armor policy attached; and HSTS on the service's answers, so a browser
  # that has been to the site once asks for https:// from then on.
  gcloud compute backend-services update "$EDGE_BACKEND" --global \
    --no-enable-logging --security-policy "$EDGE_POLICY" \
    --custom-response-header='Strict-Transport-Security: max-age=31536000' >/dev/null

  # One certificate per name, so either goes live as soon as its own DNS
  # record does, whatever the other's is doing.
  exists gcloud compute ssl-certificates describe "$EDGE_CERT" --global ||
    gcloud compute ssl-certificates create "$EDGE_CERT" --global --domains "$DOMAIN"
  CERTS="$EDGE_CERT"
  if [[ -n "$WWW" ]]; then
    exists gcloud compute ssl-certificates describe "$EDGE_WWW_CERT" --global ||
      gcloud compute ssl-certificates create "$EDGE_WWW_CERT" --global --domains "$WWW"
    CERTS="${EDGE_CERT},${EDGE_WWW_CERT}"
  fi
  # TLS 1.2 and up only.
  exists gcloud compute ssl-policies describe "$EDGE_TLS" --global ||
    gcloud compute ssl-policies create "$EDGE_TLS" --global --profile MODERN --min-tls-version 1.2

  # Everything goes to the service, except www.DOMAIN, which is sent on to
  # DOMAIN with its path and query.
  {
    echo "name: ${EDGE_MAP}"
    echo "defaultService: https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/backendServices/${EDGE_BACKEND}"
    if [[ -n "$WWW" ]]; then
      cat <<EOF
hostRules:
- hosts:
  - ${WWW}
  pathMatcher: www
pathMatchers:
- name: www
  defaultUrlRedirect:
    hostRedirect: ${DOMAIN}
    redirectResponseCode: MOVED_PERMANENTLY_DEFAULT
    stripQuery: false
EOF
    fi
  } | gcloud compute url-maps import "$EDGE_MAP" --global --quiet >/dev/null
  # Plain http:// on port 80 is answered with a redirect to https://, and
  # reaches nothing else.
  gcloud compute url-maps import "$EDGE_HTTP" --global --quiet >/dev/null <<EOF
name: ${EDGE_HTTP}
defaultUrlRedirect:
  httpsRedirect: true
  redirectResponseCode: MOVED_PERMANENTLY_DEFAULT
  stripQuery: false
EOF

  if exists gcloud compute target-https-proxies describe "$EDGE_HTTPS" --global; then
    # A certificate that someone put on the proxy stays on it. That is how a
    # failed certificate is replaced: a spare serves beside it meanwhile
    # (deploy/README.md section 2.10), and taking the spare off here would
    # leave the name with no certificate until the new one is issued.
    on_proxy="$(gcloud compute target-https-proxies describe "$EDGE_HTTPS" --global \
      --format='value(sslCertificates[].basename())')"
    while read -r cert; do
      if [[ -n "$cert" && ",${CERTS}," != *",${cert},"* ]]; then
        CERTS="${CERTS},${cert}"
        SPARE_CERTS="${SPARE_CERTS:+${SPARE_CERTS}, }${cert}"
      fi
    done <<<"${on_proxy//;/$'\n'}"
    gcloud compute target-https-proxies update "$EDGE_HTTPS" --global \
      --url-map "$EDGE_MAP" --ssl-certificates "$CERTS" --ssl-policy "$EDGE_TLS" >/dev/null
  else
    gcloud compute target-https-proxies create "$EDGE_HTTPS" --global \
      --url-map "$EDGE_MAP" --ssl-certificates "$CERTS" --ssl-policy "$EDGE_TLS"
  fi
  exists gcloud compute target-http-proxies describe "$EDGE_HTTP" --global ||
    gcloud compute target-http-proxies create "$EDGE_HTTP" --global --url-map "$EDGE_HTTP"

  for rule in "${EDGE_HTTPS}:443:--target-https-proxy=${EDGE_HTTPS}" \
              "${EDGE_HTTP}:80:--target-http-proxy=${EDGE_HTTP}"; do
    IFS=: read -r name port target <<<"$rule"
    exists gcloud compute forwarding-rules describe "$name" --global ||
      gcloud compute forwarding-rules create "$name" --global \
        --load-balancing-scheme=EXTERNAL_MANAGED --address="$EDGE_IP" --ports="$port" "$target"
  done

  # Through the load balancer, by its address, so that a DNS answer cached in
  # Cloud Shell can't hold this up. curl checks the certificate against the
  # name, so an answer means Google has issued it.
  through_edge() {
    curl -fsS -o /dev/null --max-time 20 --resolve "${DOMAIN}:443:${EDGE_ADDRESS}" "https://${DOMAIN}/api/boards"
  }
  cert_state() {
    gcloud compute ssl-certificates describe "$EDGE_CERT" --global --format='value(managed.status)' </dev/null
  }
  # The load balancer's certificates: name, state, and each name's own state.
  cert_report() {
    local rows
    if rows="$(gcloud compute ssl-certificates list --global \
      --format='value(name,managed.status,managed.domainStatus)' </dev/null 2>/dev/null)"; then
      awk -v a="$EDGE_CERT" -v b="$EDGE_WWW_CERT" \
        '$1 == a || $1 == b { printf "  %-17s %-32s %s\n", $1, $2, $3 }' <<<"$rows"
    else
      echo "  (the certificates could not be read)"
    fi
  }
  # Where a name points, as the resolver here sees it: "ok" for the load
  # balancer's address and no other, "none" for no address at all, "unknown"
  # on a machine with no getent to ask, and otherwise the addresses. A
  # certificate shows where its name pointed on the day it was issued, not
  # where it points now, and a record that Cloudflare proxies (the orange
  # cloud) answers with Cloudflare's addresses.
  points_at() {
    local found
    command -v getent >/dev/null 2>&1 || { echo unknown; return 0; }
    found="$(getent ahostsv4 "$1" 2>/dev/null | awk '!seen[$1]++ { printf "%s%s", sep, $1; sep = " " }' || true)"
    case "$found" in
      "$EDGE_ADDRESS") echo ok ;;
      "") echo none ;;
      *) echo "$found" ;;
    esac
  }
  # A line for a name whose record is not right, and nothing for one that is.
  dns_line() {
    case "$2" in
      ok | unknown) ;;
      none) printf '  %s has no A record, as seen from here\n' "$1" ;;
      *) printf '  %s points at %s, not at %s\n' "$1" "$2" "$EDGE_ADDRESS" ;;
    esac
  }
  # A line for a name that has an AAAA record, where dig is there to ask, and
  # nothing for one that has none. The load balancer answers on IPv4 only, and
  # a visitor whose network has IPv6 follows an AAAA record in preference to
  # the A record, to wherever it points. An address under 64:ff9b::/96 is not
  # a record: a network with no IPv4 of its own makes those up for a name that
  # has only an A record.
  aaaa_line() {
    local found
    command -v dig >/dev/null 2>&1 || return 0
    found="$(dig +short +time=3 +tries=2 AAAA "$1" 2>/dev/null </dev/null |
      awk '/^[0-9a-fA-F:.]+$/ && /:/ && !/^64:[fF][fF]9[bB]:/ { printf "%s%s", sep, $1; sep = " " }' || true)"
    if [[ -n "$found" ]]; then
      printf '  %s has an AAAA record (%s), and the load balancer answers on IPv4 only\n' "$1" "$found"
    fi
  }

  if [[ "$TRUST_PROXY" == 1 ]]; then
    # That the service is up comes first, on the address it keeps until the
    # domain answers: the wait below can be a long one.
    check_site
  fi
  if [[ "$TRUST_PROXY" == 1 && -n "$answered" ]]; then
    step "Waiting for https://${DOMAIN} to answer (Ctrl-C ends the wait, and nothing else)"
    # Google issues the certificate once DNS points here, usually well within
    # an hour, and the load balancer can take half an hour more to start
    # serving it. Meanwhile the site stays on run.app, unchanged. Visitors are
    # moved only when the name also resolves, from here, to the load balancer
    # and nowhere else: an issued certificate says where the name pointed on
    # the day it was issued, and with run.app closed, a name that has since
    # moved away would leave the site with no address at all.
    trap 'STOPPED=1' INT
    for ((minute = 0; ; minute++)); do
      APEX_AT="$(points_at "$DOMAIN")"
      if [[ "$APEX_AT" == ok || "$APEX_AT" == unknown ]]; then
        if edge_error="$(through_edge 2>&1)"; then
          DOMAIN_LIVE=1
          break
        fi
        EDGE_ERROR="${edge_error%%$'\n'*}"
      fi
      state="$(cert_state 2>/dev/null || true)"
      # Ctrl-C stops that read too, so the last answer is the one to keep.
      [[ -z "$STOPPED" ]] || break
      CERT_STATE="$state"
      case "$APEX_AT" in
        ok | unknown)
          if [[ "$CERT_STATE" == ACTIVE ]]; then
            printf '  %2d min: certificate issued, the load balancer is not serving it yet\n' "$minute"
          else
            printf '  %2d min: certificate %s\n' "$minute" "${CERT_STATE:-unreadable}"
          fi
          ;;
        none)
          printf '  %2d min: certificate %s; %s has no A record yet, as seen from here\n' \
            "$minute" "${CERT_STATE:-unreadable}" "$DOMAIN"
          ;;
        *)
          printf '  %2d min: certificate %s; %s points at %s, not at %s\n' \
            "$minute" "${CERT_STATE:-unreadable}" "$DOMAIN" "$APEX_AT" "$EDGE_ADDRESS"
          ;;
      esac
      # Waiting does not mend a certificate in either of these states.
      [[ "$CERT_STATE" != PROVISIONING_FAILED_PERMANENTLY && "$CERT_STATE" != RENEWAL_FAILED ]] || break
      ((minute < 20)) || break
      sleep 60 || true
      [[ -z "$STOPPED" ]] || break
    done
    trap - INT
    if [[ -n "$STOPPED" ]]; then
      printf '\nStopped waiting.\n'
    fi
    if [[ -n "$DOMAIN_LIVE" ]]; then
      # Two updates, in this order. With run.app closed first there is a moment
      # when every visitor shares one flood-control bucket; the other way
      # round there would be one when a client could choose its own.
      step "Moving visitors to ${DOMAIN}: closing ${URL} to the public"
      gcloud run services update "$SERVICE" --region "$REGION" --ingress "$CLOSED"
      # Closed for certain: Cloud Run reports the setting in force, and a
      # request from here, outside the project's network, no longer gets in.
      for ((i = 0; i < 30; i++)); do
        if [[ "$(service_state 2>/dev/null </dev/null || true)" == "${CLOSED}|${CLOSED}|"* &&
              "$(run_app_code)" != 2* ]]; then
          RUN_APP_CLOSED=1
          break
        fi
        sleep 10
      done
      if [[ -n "$RUN_APP_CLOSED" ]]; then
        step "Counting the load balancer's hop (EDUCHAN_TRUST_PROXY=2)"
        gcloud run services update "$SERVICE" --region "$REGION" --update-env-vars EDUCHAN_TRUST_PROXY=2 || {
          echo "That update failed with run.app already closed, so every visitor shares one" >&2
          echo "flood-control bucket until it goes through. Run this script again." >&2
          exit 1
        }
        TRUST_PROXY=2
      fi
    fi
  fi
fi

if [[ "$TRUST_PROXY" == 2 ]]; then
  SITE="https://${DOMAIN}"
  check=(through_edge)
  # Cloud Run answers 404, from its front end, to an outside request that
  # ingress refuses.
  step "Checking that ${URL} is closed to the public"
  direct="$(run_app_code)"
  case "$direct" in
    404) echo "closed (HTTP 404)" ;;
    2*)
      cat >&2 <<EOF
WARNING: ${URL}/api/boards still answers ${direct} from the internet, so a client
can reach the app past the load balancer and choose its own flood-control
bucket. Close it, then run this script again:
  gcloud run services update ${SERVICE} --region ${REGION} --ingress ${CLOSED}
EOF
      ;;
    *) echo "could not tell: it answered HTTP ${direct}, where a closed address answers 404" ;;
  esac
  check_site
elif [[ -z "$checked" ]]; then
  check_site
fi

if [[ -n "$DOMAIN" ]]; then
  # On every run with a domain, before the move and long after it: a record
  # that has since been switched to proxied, or a certificate that failed to
  # renew, shows up here and nowhere else.
  APEX_AT="$(points_at "$DOMAIN")"
  DNS_NOTES="$(
    dns_line "$DOMAIN" "$APEX_AT"
    aaaa_line "$DOMAIN"
    if [[ -n "$WWW" ]]; then
      dns_line "$WWW" "$(points_at "$WWW")"
      aaaa_line "$WWW"
    fi
  )"
  if [[ "$TRUST_PROXY" == 2 ]]; then
    CERT_NOTES="$(cert_report | awk '$2 != "ACTIVE"')"
  fi
fi

HEADING="educhan is live at:"
if [[ -z "$answered" ]]; then
  HEADING="educhan is deployed, but did not answer (see the warning above), at:"
elif [[ "$TRUST_PROXY" == 2 && "$APEX_AT" != ok && "$APEX_AT" != unknown ]]; then
  HEADING="educhan answers behind the load balancer, but DNS does not lead visitors to it (see the warning above), at:"
fi
if [[ -n "$DOMAIN" && "$TRUST_PROXY" == 1 && -n "$answered" ]]; then
  if [[ -n "$DOMAIN_LIVE" ]]; then
    cat >&2 <<EOF

WARNING: ${DOMAIN} answers, but Cloud Run has not closed ${URL}
yet, so the app still counts one hop, and everyone who comes through ${DOMAIN}
shares one flood-control bucket. Run this script again now: it finishes the move.
EOF
  elif [[ "$APEX_AT" != ok && "$APEX_AT" != unknown ]]; then
    cat <<EOF

${DOMAIN} is not serving yet, and cannot until its DNS record leads to the load
balancer (the warning below). The site stays on run.app meanwhile. Run this
script again once the record is right, to move visitors to ${DOMAIN}.
EOF
  elif [[ "$CERT_STATE" == ACTIVE ]]; then
    cat <<EOF

${DOMAIN} is not serving yet. Its certificate is issued, and the load balancer
can take half an hour more to start using it. The last try said:

  ${EDGE_ERROR:-nothing}

Run this script again in a few minutes to move visitors from run.app to
${DOMAIN}. Once ${DOMAIN} answers and until that run, anyone who uses it shares
one flood-control bucket with everyone else who does.
EOF
  elif [[ "$CERT_STATE" == PROVISIONING_FAILED_PERMANENTLY || "$CERT_STATE" == RENEWAL_FAILED ]]; then
    cat <<EOF

${DOMAIN} is not serving: its certificate is ${CERT_STATE}, which
waiting does not mend. deploy/README.md section 2.10 says how to replace it;
then run this script again.
EOF
  else
    cat <<EOF

${DOMAIN} is not serving yet: its certificate is ${CERT_STATE:-unreadable}. Google issues it
once ${DOMAIN} points at the load balancer, usually within an hour of the DNS
record going in. Check with this (ACTIVE means issued):

  gcloud compute ssl-certificates describe ${EDGE_CERT} --global --format='value(managed.status,managed.domainStatus)'

and run this script again as soon as it is, to move visitors from run.app to
${DOMAIN}. Once ${DOMAIN} answers and until that run, anyone who uses it shares
one flood-control bucket with everyone else who does.
EOF
  fi
fi
if [[ -n "$DNS_NOTES" ]]; then
  cat >&2 <<EOF

WARNING: DNS does not lead to the load balancer for every name:

${DNS_NOTES}

The records should be these, each "DNS only" (the grey cloud) in Cloudflare,
with no other A, AAAA or CNAME record on the name:

$(dns_records)

A name that points somewhere else is most often a proxied record (the orange
cloud), which answers with Cloudflare's addresses: its visitors then pass
through Cloudflare, which sees their addresses, and Google can neither issue
nor renew its certificate. A record added in the last half hour can also be
missing here for a while yet. A visitor whose network has IPv6, as many mobile
networks do, goes to a name's AAAA record first, and the load balancer does
not answer there.
EOF
fi
if [[ -n "$CERT_NOTES" ]]; then
  cat >&2 <<EOF

NOTE: a certificate is not ACTIVE:

${CERT_NOTES}

PROVISIONING is Google still issuing it, which needs that name's DNS record in
place. For any other state, deploy/README.md section 2.10 says what it means
and what to do.
EOF
fi
if [[ -n "$SPARE_CERTS" ]]; then
  cat <<EOF

NOTE: the load balancer also holds a certificate this script did not make
(${SPARE_CERTS}), and the script leaves it there. If it is the spare
from "Replacing a certificate" in deploy/README.md section 2.10, the last step
there takes it off, once the script's own certificates are ACTIVE.
EOF
fi
if [[ -z "$answered" ]]; then
  if [[ "$SITE" == "$URL" ]]; then
    cat >&2 <<EOF

WARNING: ${SITE}/api/boards did not answer in ${tries} tries. A 403 above means the
service is not public: run
  gcloud run services update ${SERVICE} --region ${REGION} --no-invoker-iam-check
Anything else, read what the service logged, newest first:
  gcloud logging read 'resource.type="cloud_run_revision" AND resource.labels.service_name="${SERVICE}"' \\
    --freshness=1h --limit=20 --format='value(timestamp,severity,jsonPayload,textPayload)'
EOF
  else
    cat >&2 <<EOF

WARNING: ${SITE}/api/boards did not answer through the load balancer in ${tries}
tries. A certificate that is not ACTIVE is the usual reason (there is a note
above when one is not), and the site answers by itself once it is. Otherwise
read what the service logged, newest first:
  gcloud logging read 'resource.type="cloud_run_revision" AND resource.labels.service_name="${SERVICE}"' \\
    --freshness=1h --limit=20 --format='value(timestamp,severity,jsonPayload,textPayload)'
EOF
  fi
fi
if [[ -n "$LEFTOVER" ]]; then
  cat <<EOF

NOTE: this project holds a reserved address that nothing uses (${LEFTOVER}),
left by a run with DOMAIN that did not finish. An unused address is billed by
the hour. Run the script with DOMAIN again to finish the load balancer, or
delete it:

  gcloud compute addresses delete ${LEFTOVER} --global --quiet
EOF
fi

cat <<EOF

${HEADING}

  ${SITE}

Moderation key (keep it private; it goes in the X-Mod-Key header):

  gcloud secrets versions access latest --secret=educhan-mod-key

Image uploads and institutional verification are switched off.

Expired archived threads are deleted, and the verification tables rewritten,
daily at 03:17 UTC by the ${SWEEP_JOB} Job. Run it once now: its first run
also checks that the database role may vacuum pg_statistic, and if it fails,
deploy/README.md section 2.9 says how to read why.

  gcloud run jobs execute ${SWEEP_JOB} --region ${REGION} --wait

Check that no log holds a visitor IP: open the URL from your phone or laptop,
search "what is my ip" to learn that device's address, wait a minute, then
run these here. Both must print nothing: the first looks for that address
anywhere, the second for any request log entry at all.

  gcloud logging read '"YOUR.IP.ADDRESS"' --freshness=1d --limit=5
  gcloud logging read 'httpRequest.remoteIp:*' --freshness=1d --limit=5
EOF
if [[ "$TRUST_PROXY" == 2 ]]; then
  cat <<EOF

Open http://${DOMAIN}${WWW:+ and https://${WWW}} as well before running them: the
load balancer answers there itself, with a redirect, before the service.
EOF
fi
cat <<EOF

After the demo, stop paying for the warm instance:

  gcloud run services update ${SERVICE} --region ${REGION} --min-instances 0

To ship a new version later, git pull in this folder and run the same command
again: it skips what already exists, builds the new commit, and moves both the
service and the sweeper Job onto it. It finds the load balancer, if there is
one, by itself, so DOMAIN can be left off.

  bash deploy/first-deploy.sh ${PROJECT_ID} ${REGION}
EOF
