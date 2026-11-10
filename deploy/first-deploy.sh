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
# Armor flood limit, the www. name redirected to the bare one, and the run.app
# address closed to the public, so every visitor comes through the load
# balancer. It prints the DNS records to add. Once the load balancer
# exists, later runs find it and keep the service behind it without DOMAIN.
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
if [[ -z "$PROJECT_ID" ]]; then
  echo "usage: [DOMAIN=example.org] bash deploy/first-deploy.sh PROJECT_ID [REGION]" >&2
  exit 2
fi
# A bare name in lower case: no https://, no path, no port, no trailing dot.
if [[ -n "$DOMAIN" && ! "$DOMAIN" =~ ^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]([a-z0-9-]*[a-z0-9])?$ ]]; then
  echo "DOMAIN must be a bare name in lower case, such as educhan.org (not ${DOMAIN})." >&2
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
EDGE_IPV6=educhan-ipv6
EDGE_NEG=educhan-neg
EDGE_BACKEND=educhan-backend
EDGE_POLICY=educhan-edge
EDGE_CERT=educhan-cert
EDGE_WWW_CERT=educhan-cert-www
EDGE_TLS=educhan-tls
EDGE_MAP=educhan
EDGE_HTTPS=educhan-https
EDGE_HTTP=educhan-http
# Requests a minute from one address before Cloud Armor answers 429. Generous
# on purpose: a campus, or a mobile network's shared IPv4 address, puts many
# readers behind one address. The app's own cooldowns limit posting.
EDGE_RATE="${EDGE_RATE:-1200}"

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

# A load balancer this script built is found again from its certificate, so a
# run without DOMAIN keeps the service behind it. A run that put the service
# back to EDUCHAN_TRUST_PROXY=1 there would key every visitor's flood control
# on the load balancer's address, and one that opened run.app again would let
# a client forge its own (deploy/README.md section 2.10).
EDGE_DOMAIN=""
if [[ -n "$(gcloud services list --enabled --filter=config.name=compute.googleapis.com --format='value(config.name)' 2>/dev/null || true)" ]]; then
  EDGE_DOMAIN="$(gcloud compute ssl-certificates describe "$EDGE_CERT" --global --format='value(managed.domains[0])' 2>/dev/null || true)"
fi
INGRESS="$(gcloud run services describe "$SERVICE" --region "$REGION" \
  --format='value(metadata.annotations."run.googleapis.com/ingress")' 2>/dev/null || true)"
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

  step "Addresses for ${DOMAIN}"
  gcloud services enable compute.googleapis.com
  exists gcloud compute addresses describe "$EDGE_IP" --global ||
    gcloud compute addresses create "$EDGE_IP" --global --ip-version=IPV4
  exists gcloud compute addresses describe "$EDGE_IPV6" --global ||
    gcloud compute addresses create "$EDGE_IPV6" --global --ip-version=IPV6
  EDGE_ADDRESS="$(gcloud compute addresses describe "$EDGE_IP" --global --format='value(address)')"
  EDGE_ADDRESS6="$(gcloud compute addresses describe "$EDGE_IPV6" --global --format='value(address)')"
  dns_records() {
    printf '    %-5s %-28s %s\n' Type Name Content A "$DOMAIN" "$EDGE_ADDRESS" AAAA "$DOMAIN" "$EDGE_ADDRESS6"
    if [[ -n "$WWW" ]]; then
      printf '    %-5s %-28s %s\n' A "$WWW" "$EDGE_ADDRESS" AAAA "$WWW" "$EDGE_ADDRESS6"
    fi
  }
  cat <<EOF

Add these DNS records for ${DOMAIN} now, while the rest of this runs. In
Cloudflare that is DNS > Records, with Proxy status "DNS only" (the grey
cloud) on each: proxied, Cloudflare would see every visitor's address, and
Google could not issue the certificate. Delete any other A, AAAA or CNAME
record on these names first.

$(dns_records)
EOF
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
exclusions="$(gcloud logging sinks describe _Default --format='value(exclusions[].name)')"
if grep -qw no-request-logs <<<"$exclusions"; then
  echo "already excluded"
else
  gcloud logging sinks update _Default \
    --add-exclusion='name=no-request-logs,description=Request logs carry client IPs,filter=LOG_ID("run.googleapis.com/requests") OR resource.type="http_load_balancer"'
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

# EDUCHAN_TRUST_PROXY is how many proxies append to X-Forwarded-For before the
# app reads it: one while visitors use run.app (Cloud Run's front end), two
# behind the load balancer, which adds the visitor's address and then its own.
# The count and ingress only ever change together. With run.app open and the
# count at two, a client could send its own X-Forwarded-For straight to run.app
# and choose its flood-control bucket; with run.app closed and the count at
# one, every visitor would share the load balancer's. So the service stays on
# run.app until the domain answers, and the switch further down makes both
# changes in one update (deploy/README.md section 2.10).
TRUST_PROXY=1
EDGE_INGRESS=()
if [[ -n "$DOMAIN" && "$INGRESS" == internal-and-cloud-load-balancing ]]; then
  TRUST_PROXY=2
  EDGE_INGRESS=(--ingress internal-and-cloud-load-balancing)
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

if [[ -n "$DOMAIN" ]]; then
  step "Load balancer for ${DOMAIN}"
  # How the load balancer reaches the service: a serverless network endpoint
  # group in the service's region, behind a global load balancer.
  exists gcloud compute network-endpoint-groups describe "$EDGE_NEG" --region "$REGION" ||
    gcloud compute network-endpoint-groups create "$EDGE_NEG" --region "$REGION" \
      --network-endpoint-type=serverless --cloud-run-service="$SERVICE"

  # Cloud Armor's one rule: a flood limit per address. It keeps the visitor's
  # address in memory only for the count, and writes it nowhere while the
  # backend's logging is off (below). Stripe's webhook, once the Pass is on
  # sale, must never be refused, so it is left out of the count.
  exists gcloud compute security-policies describe "$EDGE_POLICY" --global ||
    gcloud compute security-policies create "$EDGE_POLICY" --global \
      --description="educhan: a flood limit per address"
  RATE_RULE=(
    --security-policy "$EDGE_POLICY"
    --description "At most ${EDGE_RATE} requests a minute from one address"
    --expression "!request.path.startsWith('/api/pass/stripe-webhook')"
    --action throttle --conform-action allow --exceed-action deny-429
    --rate-limit-threshold-count "$EDGE_RATE" --rate-limit-threshold-interval-sec 60
    --enforce-on-key IP
  )
  if exists gcloud compute security-policies rules describe 1000 --security-policy "$EDGE_POLICY"; then
    gcloud compute security-policies rules update 1000 "${RATE_RULE[@]}" >/dev/null
  else
    gcloud compute security-policies rules create 1000 "${RATE_RULE[@]}" >/dev/null
  fi

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
  # the Armor policy attached; and HSTS, so a browser that has been here once
  # never asks for plain http:// again.
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
    gcloud compute target-https-proxies update "$EDGE_HTTPS" --global \
      --url-map "$EDGE_MAP" --ssl-certificates "$CERTS" --ssl-policy "$EDGE_TLS" >/dev/null
  else
    gcloud compute target-https-proxies create "$EDGE_HTTPS" --global \
      --url-map "$EDGE_MAP" --ssl-certificates "$CERTS" --ssl-policy "$EDGE_TLS"
  fi
  exists gcloud compute target-http-proxies describe "$EDGE_HTTP" --global ||
    gcloud compute target-http-proxies create "$EDGE_HTTP" --global --url-map "$EDGE_HTTP"

  for rule in "${EDGE_HTTPS}:${EDGE_IP}:443:--target-https-proxy=${EDGE_HTTPS}" \
              "${EDGE_HTTPS}-v6:${EDGE_IPV6}:443:--target-https-proxy=${EDGE_HTTPS}" \
              "${EDGE_HTTP}:${EDGE_IP}:80:--target-http-proxy=${EDGE_HTTP}" \
              "${EDGE_HTTP}-v6:${EDGE_IPV6}:80:--target-http-proxy=${EDGE_HTTP}"; do
    IFS=: read -r name address port target <<<"$rule"
    exists gcloud compute forwarding-rules describe "$name" --global ||
      gcloud compute forwarding-rules create "$name" --global \
        --load-balancing-scheme=EXTERNAL_MANAGED --address="$address" --ports="$port" "$target"
  done

  # Through the load balancer, by its address, so that a DNS answer cached in
  # Cloud Shell can't hold this up: the certificate, once Google has issued
  # it, already shows that the world's DNS points here.
  through_edge() { curl -fsS -o /dev/null --resolve "${DOMAIN}:443:${EDGE_ADDRESS}" "https://${DOMAIN}/api/boards"; }
  cert_state() { gcloud compute ssl-certificates describe "$EDGE_CERT" --global --format='value(managed.status)'; }

  if [[ "$TRUST_PROXY" == 1 ]]; then
    RESOLVED="$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{ print $1; exit }' || true)"
    if [[ -n "$RESOLVED" && "$RESOLVED" != "$EDGE_ADDRESS" ]]; then
      cat >&2 <<EOF

WARNING: ${DOMAIN} points at ${RESOLVED}, not at the load balancer's ${EDGE_ADDRESS}.
If ${RESOLVED} is Cloudflare's, the record is proxied (orange cloud): set it to
"DNS only". Otherwise the A record has the wrong address. Google can't issue
the certificate until it is fixed. The records:

$(dns_records)
EOF
    fi
    step "Waiting for https://${DOMAIN} to answer (Ctrl-C is safe; run the script again later)"
    # Google issues the certificate once DNS points here, usually well within
    # an hour, and the load balancer then takes a few minutes to use it.
    # Meanwhile the site stays on run.app, unchanged.
    live=""
    for ((minute = 0; ; minute++)); do
      state="$(cert_state || true)"
      if [[ "$state" == ACTIVE ]] && through_edge 2>/dev/null; then live=1; break; fi
      printf '  %2d min: certificate %s\n' "$minute" "${state:-unreadable}"
      ((minute < 20)) || break
      sleep 60
    done
    if [[ -n "$live" ]]; then
      step "Moving visitors to ${DOMAIN}: run.app closes, and the app counts the load balancer's hop"
      gcloud run services update "$SERVICE" --region "$REGION" \
        --ingress internal-and-cloud-load-balancing --update-env-vars EDUCHAN_TRUST_PROXY=2
      TRUST_PROXY=2
    fi
  fi
fi

URL="$(gcloud run services describe "$SERVICE" --region "$REGION" --format='value(status.url)')"
SITE="$URL"
check=(curl -fsS -o /dev/null "${URL}/api/boards")
if [[ "$TRUST_PROXY" == 2 ]]; then
  SITE="https://${DOMAIN}"
  check=(through_edge)
  # Cloud Run answers 404, from its front end, to an outside request that
  # ingress refuses.
  step "Checking that ${URL} is closed to the public"
  direct="$(curl -s -o /dev/null -w '%{http_code}' "${URL}/api/boards" || true)"
  if [[ "$direct" == 2* ]]; then
    cat >&2 <<EOF
WARNING: ${URL}/api/boards still answers ${direct} from the internet, so a client
can reach the app past the load balancer and choose its own flood-control
bucket. Close it, then run this script again:
  gcloud run services update ${SERVICE} --region ${REGION} --ingress internal-and-cloud-load-balancing
EOF
  else
    echo "closed (HTTP ${direct})"
  fi
fi
# Not /healthz: Cloud Run reserves some paths ending in "z" on run.app URLs.
# The board list also reads the database, so it proves the same thing.
step "Checking ${SITE}/api/boards"
answered=""
tries=6
for ((i = 0; i < tries; i++)); do
  if "${check[@]}"; then answered=1; echo "ok"; break; fi
  sleep 5
done
HEADING="educhan is live at:"
if [[ -n "$DOMAIN" && "$TRUST_PROXY" == 1 ]]; then
  # Not switched yet: the site is on run.app, as checked above.
  cat <<EOF

${DOMAIN} is not serving yet: its certificate is $(cert_state || true). Google issues it
once ${DOMAIN} points at the load balancer, usually within an hour of the DNS
records going in. The records:

$(dns_records)

Check with this (ACTIVE means issued), then run this script again to move
visitors from run.app to ${DOMAIN}:

  gcloud compute ssl-certificates describe ${EDGE_CERT} --global --format='value(managed.status,managed.domainStatus)'
EOF
fi
if [[ -z "$answered" ]]; then
  HEADING="educhan is deployed, but did not answer (see the warning above), at:"
  cat >&2 <<EOF

WARNING: ${SITE}/api/boards did not answer in ${tries} tries. A 403 above means the
service is not public: run
  gcloud run services update ${SERVICE} --region ${REGION} --no-invoker-iam-check
Anything else, read what the service logged, newest first:
  gcloud logging read 'resource.type="cloud_run_revision" AND resource.labels.service_name="${SERVICE}"' \\
    --freshness=1h --limit=20 --format='value(timestamp,severity,jsonPayload,textPayload)'
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
run this here. It must print nothing.

  gcloud logging read '"YOUR.IP.ADDRESS"' --freshness=1d --limit=5

After the demo, stop paying for the warm instance:

  gcloud run services update ${SERVICE} --region ${REGION} --min-instances 0

To ship a new version later, git pull in this folder and run the same command
again: it skips what already exists, builds the new commit, and moves both the
service and the sweeper Job onto it. It finds the load balancer, if there is
one, by itself, so DOMAIN can be left off.

  bash deploy/first-deploy.sh ${PROJECT_ID} ${REGION}
EOF
