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
# Requests a minute from one address before Cloud Armor answers 429. Unset,
# the rule is made with 1200 where the policy has none, and a rule that is
# there is left exactly as it stands. Set, every part of the rule is set again,
# with this limit.
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
# From 1 to 1000000, the largest count that a Cloud Armor throttle rule takes.
if [[ -n "$EDGE_RATE" ]] && ! { matches "$EDGE_RATE" '^[1-9][0-9]{0,6}$' && ((EDGE_RATE <= 1000000)); }; then
  echo "EDGE_RATE must be a whole number of requests a minute, from 1 to 1000000, such as 1200 (not ${EDGE_RATE})." >&2
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
# never pass for "nothing there". It is tried for about a minute, because a
# newly enabled API can go on refusing calls that long; a failure after that
# stops the script.
must_read() {
  local out try
  for try in 1 2 3 4 5 6; do
    if out="$("$@" </dev/null)"; then
      printf '%s\n' "$out"
      return 0
    fi
    if ((try < 6)); then
      echo "That read failed (its error is above). Trying again in 10 seconds." >&2
      sleep 10
    fi
  done
  echo "That read failed six times in a row, so the script stops here." >&2
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
  # Each certificate with the names it is for, which is one name on each of
  # the script's own.
  EDGE_CERTS="$(must_read gcloud compute ssl-certificates list --global --format='value(name,managed.domains.list())')"
  EDGE_DOMAIN="$(awk -v name="$EDGE_CERT" '$1 == name { split($2, names, ","); print names[1] }' <<<"$EDGE_CERTS")"
  if [[ -z "$EDGE_DOMAIN" ]]; then
    # While the bare name's certificate is being replaced (section 2.10), the
    # www. one still says which domain this load balancer serves.
    EDGE_DOMAIN="$(awk -v name="$EDGE_WWW_CERT" '$1 == name { split($2, names, ","); sub(/^www[.]/, "", names[1]); print names[1] }' <<<"$EDGE_CERTS")"
  fi
  if [[ -z "$EDGE_DOMAIN" && -n "$DOMAIN" ]]; then
    # Neither of the script's certificates, and yet there can be a load
    # balancer: while the one certificate of a www. DOMAIN is being replaced,
    # the proxy holds only the spare (section 2.10). The run has DOMAIN to go
    # by then, and it goes on only if DOMAIN is the name that what is left of
    # the load balancer serves. Its URL map sends www.NAME on to NAME where
    # DOMAIN was a bare NAME. Where DOMAIN was a www. name the map has no such
    # redirect, and the name is one of the www. names on the certificates that
    # the proxy holds.
    served="$(must_read gcloud compute url-maps list --global --format='value(name,pathMatchers[].defaultUrlRedirect.hostRedirect)' |
      awk -v name="$EDGE_MAP" '$1 == name && $2 != "" { print "  " $2 }')"
    if [[ -z "$served" ]]; then
      proxy_certs="$(must_read gcloud compute target-https-proxies list --global --format='value(name,sslCertificates[].basename())' |
        awk -v name="$EDGE_HTTPS" '$1 == name { print $2 }')"
      served="$(awk -v held=";${proxy_certs};" \
        '$2 != "" && index(held, ";" $1 ";") { n = split($2, names, ","); for (i = 1; i <= n; i++) if (names[i] ~ /^www[.]/) print "  " names[i] }' <<<"$EDGE_CERTS")"
    fi
    if [[ -n "$served" ]] && ! grep -qxF "  ${DOMAIN}" <<<"$served"; then
      cat >&2 <<EOF
This project's load balancer already serves another name, not ${DOMAIN}:

${served}

Moving to another domain is not a re-run: see deploy/README.md section 2.10.
EOF
      exit 1
    fi
  fi
  if [[ -z "$EDGE_DOMAIN" && -z "$DOMAIN" ]]; then
    # The load balancer's address, reserved and in use by nothing: a run with
    # DOMAIN that stopped early leaves it, and so does a load balancer that is
    # being taken down, whose DNS records may still point at it.
    LEFTOVER="$(must_read gcloud compute addresses list --global --format='value(name,status,address)' |
      awk -v name="$EDGE_IP" '$1 == name && $2 == "RESERVED" { print $3 }')"
  fi
fi
# The service as it stands, as "ingress wanted|ingress in force|traffic|URL":
# the ingress setting; the one Cloud Run is applying right now, which trails
# the first while a change goes through; where requests go, which holds
# "True=100" when all of them go to the latest revision; and the service's
# run.app address. Nothing at all for a service that is not deployed yet.
service_state() {
  gcloud run services list --region "$REGION" \
    --format='value[separator="|"](metadata.name,metadata.annotations."run.googleapis.com/ingress",metadata.annotations."run.googleapis.com/ingress-status",spec.traffic.map().format("{0}={1}", latestRevision, percent).list(),status.url)' |
    awk -F'|' -v name="$SERVICE" '$1 == name { print $2 "|" $3 "|" $4 "|" $5 }'
}
read_service() {
  local state
  state="$(must_read service_state)" || return 1
  IFS='|' read -r INGRESS IN_FORCE TRAFFIC URL <<<"$state"
}
# The settings of the service's latest revision, as NAME=value joined by
# commas. Nothing at all for a service that is not deployed yet.
service_env() {
  gcloud run services list --region "$REGION" \
    --format='value[separator="|"](metadata.name,spec.template.spec.containers[0].env.map().format("{0}={1}", name, value).list())' |
    awk -F'|' -v name="$SERVICE" '$1 == name { print substr($0, length($1) + 2) }'
}
# A rollback made in the console pins traffic to one revision, and a pinned
# service goes on serving that revision whatever is deployed after it.
pinned() { [[ -n "$TRAFFIC" && ",${TRAFFIC}," != *",True=100,"* ]]; }
# What run.app answers a request from here, which is outside the project's
# network: 404 once ingress refuses it, 000 if nothing answered at all.
run_app_code() { curl -s -o /dev/null --max-time 20 -w '%{http_code}' "${URL}/api/boards" || true; }
# Is that request refused? Only a refusal counts: the 404 that Cloud Run's
# front end answers for ingress, or a 403. No answer at all proves nothing
# either way, and nor does any other answer. A service whose run.app address
# is switched off has no address left to close.
OUTSIDE=""
run_app_refuses() {
  [[ -n "$URL" ]] || return 0
  OUTSIDE="$(run_app_code)"
  [[ "$OUTSIDE" == 403 || "$OUTSIDE" == 404 ]]
}
# Why run.app does not count as closed: WHY names the reason and WHY_TEXT says
# it. It goes by the ingress Cloud Run last reported in force ($1), the setting
# itself where it was read along with that ($2, left out where it was not), and
# the last answer to a request from here. The setting comes first: set to
# anything else, it is the reason, whatever is in force. A request is made only
# once both read closed, so its answer is the reason only then.
WHY=""
WHY_TEXT=""
why_not_closed() {
  local in_force="$1" setting="${2-$CLOSED}"
  if [[ -n "$setting" && "$setting" != "$CLOSED" ]]; then
    WHY=changed
    WHY_TEXT="the ingress setting is now \"${setting}\", which something other than
this run set while it waited"
  elif [[ -z "$in_force" ]]; then
    WHY=unread
    WHY_TEXT="Cloud Run did not say which ingress setting is in force"
  elif [[ "$in_force" != "$CLOSED" ]]; then
    WHY=waiting
    WHY_TEXT="Cloud Run reports ingress \"${in_force}\" in force, not
\"${CLOSED}\""
  elif [[ -z "$setting" ]]; then
    WHY=nosetting
    WHY_TEXT="Cloud Run did not say what the ingress setting is"
  elif [[ "$OUTSIDE" == 000 ]]; then
    WHY=silent
    WHY_TEXT="a request from here to
${URL}
got no answer at all, which says nothing either way"
  else
    WHY=answered
    WHY_TEXT="a request from here to
${URL}
was answered HTTP ${OUTSIDE}, where a closed address answers 404 or 403"
  fi
}
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
if [[ -n "$EDGE_RATE" && -z "$DOMAIN" ]]; then
  echo "EDGE_RATE sets the flood limit of the load balancer that DOMAIN builds, and this" >&2
  echo "project has none. Run the script with DOMAIN=your.domain as well, or without EDGE_RATE." >&2
  exit 2
fi

# run.app open while the count is two is the one state in which a client can
# choose its own flood-control bucket (the comment before the deploy has the
# whole of it). This script never leaves a service so, but a hand that opened
# run.app can, and the deploy that would put it right is a build away, which
# can fail first. So the count goes back to one here, before anything slow. A
# pinned service is left to the deploy: what it serves is an older revision,
# which an update to the settings would not change.
if [[ "$INGRESS" != "$CLOSED" ]] && ! pinned; then
  env_now="$(must_read service_env)"
  if [[ ",${env_now}," == *",EDUCHAN_TRUST_PROXY=2,"* ]]; then
    cat >&2 <<EOF

WARNING: the ${SERVICE} service's run.app address is open to the internet (its
ingress is "${INGRESS:-all}") while the app counts two hops (EDUCHAN_TRUST_PROXY=2).
So a client can send an X-Forwarded-For of its own straight to run.app and
choose its own flood-control bucket. This script never leaves a service in
that state, so something else has changed one of the two. The count goes back
to one now, before the build.
EOF
    gcloud run services update "$SERVICE" --region "$REGION" --update-env-vars EDUCHAN_TRUST_PROXY=1 || {
      echo "That update failed, so the count is still two. The deploy further down sets it" >&2
      echo "to one as well. If this run stops before then, run the update by hand:" >&2
      echo "  gcloud run services update ${SERVICE} --region ${REGION} --update-env-vars EDUCHAN_TRUST_PROXY=1" >&2
    }
  fi
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
  # An IPv4 address and no IPv6 one, for now. The app counts an IPv6 client by
  # its /64, but Cloud Armor's limit counts every address apart, and the count
  # of X-Forwarded-For entries has not been checked over IPv6
  # (deploy/README.md section 2.10).
  # Was the address there before this run? An earlier run printed its records
  # then, and the lines below are worded for that.
  ADDRESS_WAS_THERE=1
  if ! exists gcloud compute addresses describe "$EDGE_IP" --global; then
    ADDRESS_WAS_THERE=""
    gcloud compute addresses create "$EDGE_IP" --global --ip-version=IPV4
  fi
  # The address, and IN_USE once the load balancer's forwarding rules hold it.
  address="$(gcloud compute addresses describe "$EDGE_IP" --global --format='value(address,status)')"
  read -r EDGE_ADDRESS EDGE_ADDRESS_STATUS <<<"$address"
  dns_records() {
    printf '    %-5s %-28s %s\n' Type Name Content A "$DOMAIN" "$EDGE_ADDRESS"
    if [[ -n "$WWW" ]]; then
      printf '    %-5s %-28s %s\n' A "$WWW" "$EDGE_ADDRESS"
    fi
  }
  if [[ "$INGRESS" == "$CLOSED" && "$EDGE_ADDRESS_STATUS" == IN_USE ]]; then
    # The site has moved behind the load balancer, which it does only once the
    # bare name's record leads there.
    cat <<EOF

The DNS records for ${DOMAIN}, as they should already stand. In Cloudflare
each has Proxy status "DNS only" (the grey cloud).

$(dns_records)
EOF
  elif [[ -n "$ADDRESS_WAS_THERE" ]]; then
    cat <<EOF

The DNS records for ${DOMAIN}. If they are not in place yet, add them now,
while the rest of this runs. In Cloudflare that is DNS > Records, with Proxy
status "DNS only" (the grey cloud) on each: proxied, Cloudflare would see
every visitor's address, and Google could not issue the certificate or renew
it. The names should have no other A, AAAA or CNAME record; the load balancer
answers on IPv4 only, so there is no AAAA record to add.

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
# So the count is two only where run.app is closed for certain: Cloud Run
# reports the closing in force, read again here because the build above took
# minutes, and a request from here to run.app is refused. The run that moves
# the site closes run.app further down, makes the same two checks, and only
# then raises the count (deploy/README.md section 2.10).
read_service
TRUST_PROXY=1
EDGE_INGRESS=()
RESUME=""
if [[ -n "$DOMAIN" && "$INGRESS" == "$CLOSED" ]]; then
  if [[ "$IN_FORCE" == "$CLOSED" ]]; then
    # No answer at all is asked for again, twice: it says nothing either way.
    for try in 1 2 3; do
      if run_app_refuses; then
        TRUST_PROXY=2
        EDGE_INGRESS=(--ingress "$CLOSED")
        break
      fi
      [[ "$OUTSIDE" == 000 && "$try" != 3 ]] || break
      sleep 5
    done
  fi
  if [[ "$TRUST_PROXY" == 1 ]]; then
    # An earlier run closed run.app, or asked Cloud Run to, and this one cannot
    # confirm it. The deploy counts one hop, the safe side, and the move is
    # finished further down, through the load balancer.
    RESUME=1
    why_not_closed "$IN_FORCE"
    cat <<EOF

NOTE: run.app is set to closed, but this run cannot confirm that it is:
${WHY_TEXT}.
The service is deployed counting one hop, which is safe, and the count goes up
further down once the closing is confirmed.
EOF
  fi
fi
# Is this the run that takes the count from one to two? It is where the latest
# revision counted one. It may be where traffic is pinned to an older revision,
# whose count this script does not read. Asked only for the note that sends the
# owner to check the count, so a read that fails is let go, and the note with
# it. COUNT_RAISED is set once the deploy has gone through.
COUNT_RAISED=""
will_raise=""
if [[ "$TRUST_PROXY" == 2 ]]; then
  if pinned; then
    will_raise=pin
  else
    env_was="$(service_env 2>/dev/null </dev/null || true)"
    if [[ ",${env_was}," == *",EDUCHAN_TRUST_PROXY=1,"* ]]; then
      will_raise=1
    fi
  fi
fi
# The note goes out at the end of the run, and at the end of a run that fails
# after the count went up as well: the next run finds the count at two already
# and cannot tell that nobody has checked it.
count_note() {
  local how="$COUNT_RAISED"
  COUNT_RAISED=""
  case "$how" in
    "") ;;
    pin)
      cat <<EOF

This run ended a pin, and the revision that serves now counts two hops to find
each visitor's address, which the pinned one may not have done. Check once,
with two devices, that it finds the right one: "Check the count once it is
live" in deploy/README.md section 2.10.
EOF
      ;;
    *)
      cat <<EOF

As of this run the app counts two hops to find each visitor's address. Check
once, with two devices, that it finds the right one: "Check the count once it
is live" in deploy/README.md section 2.10.
EOF
      ;;
  esac
}
trap count_note EXIT
# After a deploy that counts two, one more request to run.app from outside.
# Cloud Run answers 404, from its front end, to a request that ingress refuses.
# An answer from the app itself means run.app is open after all, and the count
# goes back to one at once.
run_app_last_look() {
  local direct
  if [[ -z "$URL" ]]; then
    step "run.app"
    echo "This service has no run.app address (its default URL is switched off), so there is none to close."
    return 0
  fi
  step "Checking that ${URL} is closed to the public"
  direct="$(run_app_code)"
  case "$direct" in
    403 | 404) echo "closed (HTTP ${direct})" ;;
    2*)
      cat >&2 <<EOF
${URL}/api/boards answers ${direct} from the internet, though it was refused
earlier in this run. With the count at two, a client that reaches the app
there, past the load balancer, can choose its own flood-control bucket. So the
count goes back to one now, which stops that: everyone who comes through
${DOMAIN} then shares one bucket. See what opened run.app:

  gcloud run services describe ${SERVICE} --region ${REGION} --format='yaml(metadata.annotations)'

(run.googleapis.com/ingress is the setting, and ingress-status the one in
force.) Then run this script again.
EOF
      COUNT_RAISED=""
      gcloud run services update "$SERVICE" --region "$REGION" --update-env-vars EDUCHAN_TRUST_PROXY=1 || {
        echo "That update failed, so the count is still two. Run it by hand now:" >&2
        echo "  gcloud run services update ${SERVICE} --region ${REGION} --update-env-vars EDUCHAN_TRUST_PROXY=1" >&2
      }
      exit 1
      ;;
    000) echo "could not tell: it gave no answer at all, where a closed address answers 404 or 403" ;;
    *) echo "could not tell: it answered HTTP ${direct}, where a closed address answers 404 or 403" ;;
  esac
}

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
COUNT_RAISED="$will_raise"
if [[ "$TRUST_PROXY" == 2 ]]; then
  run_app_last_look
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

# The service's run.app address, which a new service has only now.
read_service
if [[ -z "$URL" && "$TRUST_PROXY" == 1 && -z "$RESUME" ]]; then
  cat >&2 <<EOF

The ${SERVICE} service is deployed, but it has no run.app address: its default
URL is switched off, which this script never does. The script checks the
service there, and moves a site to its domain only from a run.app address that
answers, so it stops here. Switch the address back on and run it again:

  gcloud run services update ${SERVICE} --region ${REGION} --default-url
EOF
  exit 1
fi
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
SEEN_WANTED=""
SEEN_IN_FORCE=""
ARMOR_NOTE=""
CERTS_UNREAD=""
CERT_STATE=""
EDGE_ERROR=""
APEX_AT=""
DNS_NOTES=""
CERT_NOTES=""
SPARE_CERTS=""
STOPPED=""
# Visitors are moved only once the domain is seen, from here, to lead to the
# load balancer, and a machine with no getent cannot look. Cloud Shell has it.
CAN_LOOK_UP=1
command -v getent >/dev/null 2>&1 || CAN_LOOK_UP=""

if [[ -n "$DOMAIN" ]]; then
  step "Load balancer for ${DOMAIN}"
  # How the load balancer reaches the service: a serverless network endpoint
  # group in the service's region, behind a global load balancer.
  exists gcloud compute network-endpoint-groups describe "$EDGE_NEG" --region "$REGION" ||
    gcloud compute network-endpoint-groups create "$EDGE_NEG" --region "$REGION" \
      --network-endpoint-type=serverless --cloud-run-service="$SERVICE"

  # Where the site answers at this point of the run, for the messages of a
  # step below that fails.
  site_now() {
    if [[ "$INGRESS" == "$CLOSED" ]]; then
      echo "https://${DOMAIN}"
    else
      echo "${URL:-its run.app address}"
    fi
  }

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

Meanwhile the site is deployed, at
$(site_now)
EOF
    if [[ "$EDGE_ADDRESS_STATUS" != IN_USE && -z "$EDGE_DOMAIN" ]]; then
      cat >&2 <<EOF

The address reserved for the load balancer (${EDGE_IP}, ${EDGE_ADDRESS}) stays
reserved, and Google bills an unused address by the hour: should the domain
wait, a run of this script without DOMAIN says how to give the address up
safely.
EOF
    fi
  }
  exists gcloud compute security-policies describe "$EDGE_POLICY" --global ||
    gcloud compute security-policies create "$EDGE_POLICY" --global \
      --description="educhan: a flood limit per address" ||
    { armor_quota_hint; exit 1; }
  # The rule is made where the policy has none at priority 1000: on the first
  # run, and again if someone has deleted it. Whether it is there is read from
  # the policy's own list of rules, so that a read that fails stops the run
  # and is not taken for a rule that is missing.
  rule_priorities="$(must_read gcloud compute security-policies describe "$EDGE_POLICY" --global \
    --format='value(rules[].priority)')"
  rule_there=""
  if grep -qx 1000 <<<"${rule_priorities//;/$'\n'}"; then
    rule_there=1
  fi
  rate="${EDGE_RATE:-1200}"
  rule_flags=(
    --security-policy "$EDGE_POLICY"
    --description "At most ${rate} requests a minute from one address"
    --src-ip-ranges '*'
    --action throttle --conform-action allow --exceed-action deny-429
    --rate-limit-threshold-count "$rate" --rate-limit-threshold-interval-sec 60
    --enforce-on-key IP
  )
  if [[ -z "$rule_there" ]]; then
    gcloud compute security-policies rules create 1000 "${rule_flags[@]}" >/dev/null ||
      { armor_quota_hint; exit 1; }
    echo "Cloud Armor answers 429 past ${rate} requests a minute from one address."
  else
    # A rule that is there is left alone unless EDGE_RATE is set: a rule tuned
    # by hand, in a flood say, outlives the next deploy whole. With EDGE_RATE
    # the update names every part of the script's own rule, and takes the rule
    # out of preview, in which it stops nothing: gcloud sends only what a
    # command names, and Cloud Armor keeps the rest of the rule.
    if [[ -n "$EDGE_RATE" ]]; then
      gcloud compute security-policies rules update 1000 "${rule_flags[@]}" --no-preview >/dev/null || {
        cat >&2 <<EOF

That update of Cloud Armor's rule 1000 failed (its error is above). If the
error is about the rule itself, the rule may hold a setting made by hand that
an update cannot write over. To replace it, delete it and run this script
again with the same EDGE_RATE, which makes it anew; between the two the site
has no flood limit at the edge:

  gcloud compute security-policies rules delete 1000 --security-policy ${EDGE_POLICY} --quiet

Meanwhile the site is deployed, at
$(site_now)
EOF
        exit 1
      }
    fi
    # The rule as it now stands, read back even after an update, which can
    # leave in place a part that was set by hand. The expression comes last
    # because it can hold a "|", and several lines.
    rule="$(must_read gcloud compute security-policies rules describe 1000 --security-policy "$EDGE_POLICY" \
      --format='value[separator="|"](action,rateLimitOptions.rateLimitThreshold.count,rateLimitOptions.rateLimitThreshold.intervalSec,rateLimitOptions.exceedAction,rateLimitOptions.conformAction,rateLimitOptions.enforceOnKey,preview,match.config.srcIpRanges.list(),match.expr.expression)')"
    rule="${rule//$'\n'/ }"
    IFS='|' read -r rule_action rule_count rule_interval rule_exceed rule_conform rule_key rule_preview rule_ranges rule_expr <<<"$rule"
    # Is it a flood limit of the script's kind: every request counted by its
    # address, 429 past the limit, and enforced? With EDGE_RATE it must also
    # hold the limit just set.
    rule_ok=""
    if [[ "$rule_action" == throttle && "$rule_exceed" == "deny(429)" && "$rule_conform" == allow &&
          "$rule_key" == IP && -n "$rule_count" && -n "$rule_interval" && "$rule_preview" != True &&
          "$rule_ranges" == "*" && -z "$rule_expr" ]]; then
      rule_ok=1
    fi
    if [[ -n "$EDGE_RATE" && ( "$rule_count" != "$rate" || "$rule_interval" != 60 ) ]]; then
      rule_ok=""
    fi
    if [[ -n "$rule_ok" && -n "$EDGE_RATE" ]]; then
      echo "Cloud Armor answers 429 past ${rate} requests a minute from one address."
    elif [[ -n "$rule_ok" ]]; then
      echo "Cloud Armor's rule is left as it stands: 429 past ${rule_count} requests in ${rule_interval} seconds from one address."
    else
      rule_scope="addresses ${rule_ranges:--}"
      if [[ -n "$rule_expr" ]]; then
        rule_scope="requests matching ${rule_expr}"
      fi
      ARMOR_NOTE="  action: ${rule_action:--}   limit: ${rule_count:--} requests in ${rule_interval:--} seconds   past it: ${rule_exceed:--}   under it: ${rule_conform:--}
  counted by: ${rule_key:--}   preview: ${rule_preview:--}   applies to: ${rule_scope}"
      if [[ -n "$EDGE_RATE" ]]; then
        echo "Cloud Armor took the change, but rule 1000 is still not the script's own (there is a note about it at the end)."
      else
        echo "Cloud Armor's rule is left as it stands (there is a note about it at the end)."
      fi
    fi
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
  # the Armor policy attached; and HSTS on the service's answers, so a browser
  # that has been to the site once asks for https:// from then on. The flag
  # replaces the backend's whole list of response headers, so any other header
  # that someone added there is read first and sent back with this one.
  held_headers="$(must_read gcloud compute backend-services describe "$EDGE_BACKEND" --global \
    --flatten='customResponseHeaders[]' --format='value(customResponseHeaders)')"
  held_headers="$(grep -iv '^strict-transport-security:' <<<"$held_headers" || true)"
  response_headers=('--custom-response-header=Strict-Transport-Security: max-age=31536000')
  while IFS= read -r header; do
    if [[ -n "$header" ]]; then
      response_headers+=("--custom-response-header=${header}")
    fi
  done <<<"$held_headers"
  gcloud compute backend-services update "$EDGE_BACKEND" --global \
    --no-enable-logging --security-policy "$EDGE_POLICY" "${response_headers[@]}" >/dev/null

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
  # The certificates on the load balancer that Google manages, a spare among
  # them: name, state, and each name's own state. It fails when they cannot be
  # read.
  cert_report() {
    local rows
    rows="$(gcloud compute ssl-certificates list --global \
      --format='value(name,managed.status,managed.domainStatus)' </dev/null 2>/dev/null)" || return 1
    awk -F'\t' -v held=",${CERTS}," \
      'index(held, "," $1 ",") && $2 != "" { printf "  %-17s %-32s %s\n", $1, $2, $3 }' <<<"$rows"
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
  # nothing for one that has none. The load balancer answers on IPv4 only, so
  # an AAAA record leads somewhere else: a visitor whose network has IPv6
  # follows it in preference to the A record, and Google issues and renews no
  # certificate for a name that resolves anywhere but the load balancer. An
  # address under 64:ff9b::/96 is not a record: a network with no IPv4 of its
  # own makes those up for a name that has only an A record.
  aaaa_line() {
    local found
    command -v dig >/dev/null 2>&1 || return 0
    found="$(dig +short +time=3 +tries=2 AAAA "$1" 2>/dev/null </dev/null |
      awk '/^[0-9a-fA-F:.]+$/ && /:/ && !/^64:[fF][fF]9[bB]:/ { printf "%s%s", sep, $1; sep = " " }' || true)"
    if [[ -n "$found" ]]; then
      printf '  %s has an AAAA record (%s), and the load balancer answers on IPv4 only\n' "$1" "$found"
    fi
  }

  if [[ -n "$RESUME" ]]; then
    # run.app is closed already, or closing, so the load balancer is the only
    # way in, and the service is checked through it.
    SITE="https://${DOMAIN}"
    check=(through_edge)
    check_site
    if [[ -n "$answered" ]]; then
      DOMAIN_LIVE=1
    fi
  elif [[ "$TRUST_PROXY" == 1 ]]; then
    # That the service is up comes first, on the address it keeps until the
    # domain answers: the wait below can be a long one.
    check_site
  fi
  if [[ "$TRUST_PROXY" == 1 && -z "$RESUME" && -n "$answered" && -z "$CAN_LOOK_UP" ]]; then
    printf '\nNot waiting for https://%s to answer: this machine has no getent to look the name up with (more at the end).\n' "$DOMAIN"
  fi
  if [[ "$TRUST_PROXY" == 1 && -z "$RESUME" && -n "$answered" && -n "$CAN_LOOK_UP" ]]; then
    # A real AAAA record keeps Google from issuing the certificate at all, so
    # it is worth saying before the wait and not only after it.
    early_notes="$(
      aaaa_line "$DOMAIN"
      if [[ -n "$WWW" ]]; then aaaa_line "$WWW"; fi
    )"
    if [[ -n "$early_notes" ]]; then
      cat <<EOF

${early_notes}
  Google issues no certificate for a name while it has one. Delete the AAAA
  record in Cloudflare now. If there is none to delete, the name's A record is
  proxied (the orange cloud), and the fix is to switch that to DNS only.
EOF
    fi
    step "Waiting for https://${DOMAIN} to answer (Ctrl-C ends the wait, and nothing else)"
    # Google issues the certificate once DNS points here, usually well within
    # an hour, and the load balancer can take half an hour more to start
    # serving it. Meanwhile the site stays on run.app, unchanged. Visitors are
    # moved only when the name also resolves, from here, to the load balancer
    # and nowhere else: an issued certificate says where the name pointed on
    # the day it was issued, and with run.app closed, a name that has since
    # moved away would leave the site with no address at all.
    # Ctrl-C reaches whatever the script is running at that moment, a lookup
    # or a read as well as the sleep, and stops it. Each of those is allowed to
    # fail for that reason, and the loop ends at the next look at STOPPED.
    # A look a minute, twenty-one in all, and no new one once twenty minutes
    # have gone: a look is a lookup, a request and a read, each of which can
    # be slow.
    trap 'STOPPED=1' INT
    wait_from=$SECONDS
    for ((minute = 0; ; minute++)); do
      APEX_AT="$(points_at "$DOMAIN")" || APEX_AT=none
      [[ -z "$STOPPED" ]] || break
      if [[ "$APEX_AT" == ok ]]; then
        if edge_error="$(through_edge 2>&1)"; then
          DOMAIN_LIVE=1
          break
        fi
        [[ -z "$STOPPED" ]] || break
        EDGE_ERROR="${edge_error%%$'\n'*}"
      fi
      state="$(cert_state 2>/dev/null)" || state=""
      # A read that Ctrl-C stopped says nothing: the last answer is the one to keep.
      [[ -z "$STOPPED" ]] || break
      CERT_STATE="$state"
      case "$APEX_AT" in
        ok)
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
      ((minute < 20 && SECONDS - wait_from < 1200)) || break
      sleep 60 || true
      [[ -z "$STOPPED" ]] || break
    done
    trap - INT
    if [[ -n "$STOPPED" ]]; then
      printf '\nStopped waiting.\n'
    fi
    if [[ -z "$DOMAIN_LIVE" && -z "$CERT_STATE" ]]; then
      # The wait ended before it had read the certificate once.
      CERT_STATE="$(cert_state 2>/dev/null || true)"
    fi
  fi
  if [[ -n "$DOMAIN_LIVE" ]]; then
    # Two updates, in this order. With run.app closed first there is a moment
    # when every visitor shares one flood-control bucket; the other way round
    # there would be one when a client could choose its own.
    if [[ -n "$RESUME" ]]; then
      step "Making sure that run.app is closed to the public"
    else
      step "Moving visitors to ${DOMAIN}: closing ${URL} to the public"
    fi
    gcloud run services update "$SERVICE" --region "$REGION" --ingress "$CLOSED"
    # Closed for certain: Cloud Run reports the setting in force, and a
    # request from here, outside the project's network, is refused. Thirty
    # tries ten seconds apart, and no new one once five minutes have gone: a
    # try is a read and a request, each of which can be slow.
    echo "Waiting, about five minutes at most, for Cloud Run to report that in force and to refuse a request from here."
    waited_from=$SECONDS
    for ((i = 0; i < 30; i++)); do
      if ((i > 0)); then
        ((SECONDS - waited_from < 300)) || break
        sleep 10
      fi
      state="$(service_state 2>/dev/null </dev/null || true)"
      SEEN_WANTED="${state%%|*}"
      SEEN_IN_FORCE="${state#*|}"
      SEEN_IN_FORCE="${SEEN_IN_FORCE%%|*}"
      if [[ "$state" == "${CLOSED}|${CLOSED}|"* ]] && run_app_refuses; then
        RUN_APP_CLOSED=1
        break
      fi
    done
    if [[ -n "$RUN_APP_CLOSED" ]]; then
      step "Counting the load balancer's hop (EDUCHAN_TRUST_PROXY=2)"
      gcloud run services update "$SERVICE" --region "$REGION" --update-env-vars EDUCHAN_TRUST_PROXY=2 || {
        echo "That update failed with run.app already closed, so every visitor shares one" >&2
        echo "flood-control bucket until it goes through. Run this script again." >&2
        exit 1
      }
      TRUST_PROXY=2
      COUNT_RAISED=1
      run_app_last_look
    fi
    # Whichever way that went, the domain is where the site answers from now on.
    SITE="https://${DOMAIN}"
    check=(through_edge)
  fi
fi

if [[ "$TRUST_PROXY" == 2 ]]; then
  SITE="https://${DOMAIN}"
  check=(through_edge)
  check_site
elif [[ -z "$checked" || ( -n "$DOMAIN_LIVE" && -z "$RESUME" ) ]]; then
  # A site with no domain, which is checked here and nowhere else, or one whose
  # run.app address this run has just asked Cloud Run to close: that one is
  # checked again where it now answers. (A run that only tried to finish an
  # earlier run's move has checked there already.)
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
  if [[ "$TRUST_PROXY" == 2 || -n "$RESUME" ]]; then
    # The certificates of a site that is behind the load balancer, or should be.
    if certs="$(cert_report)"; then
      CERT_NOTES="$(awk '$2 != "ACTIVE"' <<<"$certs")"
    else
      CERTS_UNREAD=1
    fi
  fi
fi

HEADING="educhan is live at:"
if [[ -z "$answered" ]]; then
  HEADING="educhan is deployed, but did not answer (see the warning above), at:"
elif [[ -n "$DOMAIN" && "$SITE" == "https://${DOMAIN}" && "$APEX_AT" != ok && "$APEX_AT" != unknown ]]; then
  HEADING="educhan answers behind the load balancer, but DNS does not lead visitors to it (see the warning above), at:"
fi
if [[ -n "$DOMAIN" && "$TRUST_PROXY" == 1 && -n "$DOMAIN_LIVE" ]]; then
  why_not_closed "$SEEN_IN_FORCE" "$SEEN_WANTED"
  cat >&2 <<EOF

WARNING: ${DOMAIN} answers, but this run could not confirm that run.app is
closed: ${WHY_TEXT}.
EOF
  case "${WHY}:${SEEN_WANTED}" in
    changed:all)
      cat >&2 <<EOF
So the app still counts one hop, and everyone who comes through ${DOMAIN}
shares one flood-control bucket. The next run of this script closes run.app
again and raises the count. Find out first what set the ingress back: someone
else at work on the service, or another way of deploying it ("Later runs" in
deploy/README.md section 2.10).
EOF
      ;;
    changed:*)
      cat >&2 <<EOF
The app still counts one hop. With that setting nothing reaches the service,
by ${DOMAIN} or by run.app, and the next run of this script stops there and
says how to open it again.
EOF
      ;;
    silent:*)
      cat >&2 <<EOF
So the app still counts one hop, and everyone who comes through ${DOMAIN}
shares one flood-control bucket. Run this script again now: it raises the
count as soon as the closing is confirmed. No answer at all is a fault on the
way from here to run.app, and no sign that run.app is open. If the next run
says the same, wait a few minutes, or start a new Cloud Shell session, and run
it again.
EOF
      ;;
    *)
      cat >&2 <<EOF
So the app still counts one hop, and everyone who comes through ${DOMAIN}
shares one flood-control bucket. Run this script again now: it raises the
count as soon as the closing is confirmed.
EOF
      if [[ -n "$RESUME" ]]; then
        cat >&2 <<EOF

An earlier run had asked for the closing already, so another run may say the
same. See what Cloud Run reports:

  gcloud run services describe ${SERVICE} --region ${REGION} --format='yaml(metadata.annotations)'

(run.googleapis.com/ingress is the setting, and ingress-status the one in
force.) "Going back to run.app" in deploy/README.md section 2.10 puts the site
back on its run.app address, where each visitor is counted apart, while that is
looked into.
EOF
      fi
      ;;
  esac
elif [[ -n "$DOMAIN" && "$TRUST_PROXY" == 1 && -n "$answered" ]]; then
  if [[ -z "$CAN_LOOK_UP" ]]; then
    cat <<EOF

The site stays on run.app: this machine has no getent, so the script could not
check that ${DOMAIN} leads to the load balancer, and it moves visitors only
once it has. Run it from Cloud Shell, which has getent, to make the move.
EOF
  elif [[ "$APEX_AT" != ok && "$APEX_AT" != unknown ]]; then
    cat <<EOF

${DOMAIN} does not lead to the load balancer yet (the warning below), so the
site stays on run.app. Run this script again once the record is right, to move
visitors to ${DOMAIN}.
EOF
    if [[ "$APEX_AT" != none ]]; then
      cat <<EOF
If the record is proxied (the orange cloud in Cloudflare), anyone who uses
${DOMAIN} meanwhile may be reaching the site through Cloudflare, which sees
their addresses, and shares one flood-control bucket with everyone else who
does. Switch the record to DNS only now.
EOF
    fi
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
cloud), which answers with Cloudflare's addresses, IPv6 ones among them
(2606:4700:...) that are no record of yours and go when the record is DNS
only. Its visitors pass through Cloudflare, which sees their addresses, and
Google cannot issue its certificate, or renew one it has issued. A record
added in the last half hour can also be missing here for a while yet.

An AAAA record of your own is one to delete. Google issues and renews no
certificate for a name that resolves anywhere but the load balancer, which has
no IPv6 address, and a visitor whose network has IPv6, as many mobile networks
do, goes to the AAAA record first.
EOF
fi
if [[ -n "$DOMAIN" && -z "$CAN_LOOK_UP" && ( "$TRUST_PROXY" == 2 || -n "$RESUME" ) ]]; then
  cat >&2 <<EOF

NOTE: this machine has no getent, so this run did not check that the DNS
records of ${DOMAIN} still lead to the load balancer. A run from Cloud Shell,
which has getent, makes that check.
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
if [[ -n "$CERTS_UNREAD" ]]; then
  cat >&2 <<EOF

NOTE: the load balancer's certificates could not be read just now, so this run
did not check them. Every one should be ACTIVE:

  gcloud compute ssl-certificates list --global
EOF
fi
if [[ -n "$ARMOR_NOTE" && -n "$EDGE_RATE" ]]; then
  cat <<EOF

NOTE: Cloud Armor's rule 1000 is still not the flood limit this script makes,
though this run set it again with EDGE_RATE: it holds something that an update
leaves in place. As it stands:

${ARMOR_NOTE}

To have the script's own rule, delete this one and run the script again with
the same EDGE_RATE, which makes it anew. Between the two the site has no flood
limit at the edge.

  gcloud compute security-policies rules delete 1000 --security-policy ${EDGE_POLICY} --quiet
EOF
elif [[ -n "$ARMOR_NOTE" ]]; then
  cat <<EOF

NOTE: Cloud Armor's rule 1000 is not the flood limit this script makes, and
the script leaves a rule that is already there alone. As it stands:

${ARMOR_NOTE}

To put the script's own rule back, run it once with EDGE_RATE, such as
EDGE_RATE=1200 for 1200 requests a minute from one address.
EOF
fi
if [[ -n "$SPARE_CERTS" ]]; then
  cat <<EOF

NOTE: the load balancer also holds a certificate this script did not make
(${SPARE_CERTS}), and the script leaves it there. If it is the spare
from "Replacing a certificate" in deploy/README.md section 2.10, the last step
there takes it off, half an hour after the script's own certificates have all
turned ACTIVE.
EOF
fi
if [[ -z "$answered" ]]; then
  if [[ "$SITE" == "$URL" ]]; then
    cat >&2 <<EOF

WARNING: ${SITE}/api/boards did not answer in ${tries} tries. A 403 above means the
service is not public: run
  gcloud run services update ${SERVICE} --region ${REGION} --no-invoker-iam-check
A 404 above means Cloud Run refused the request before the service saw it: its
ingress setting or its default URL has been changed, and this shows both:
  gcloud run services describe ${SERVICE} --region ${REGION} --format='yaml(metadata.annotations)'
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
    if [[ -n "$RESUME" ]]; then
      cat >&2 <<EOF

The app also counts one hop for now: run.app was set to closed when this run
began, and the run could not confirm that it was. Run this script again once
${DOMAIN} answers, and it puts the count right. To have the site back on its
run.app address meanwhile, see "Going back to run.app" in deploy/README.md
section 2.10.
EOF
    fi
  fi
fi
if [[ -n "$LEFTOVER" ]]; then
  cat <<EOF

NOTE: this project holds a reserved address that no load balancer uses
(${EDGE_IP}, ${LEFTOVER}). A run with DOMAIN that stopped early leaves one,
and so does a load balancer that is being taken down. Google bills an unused
address by the hour. Run the script with DOMAIN again to build the load
balancer on it. Or give the address up, but only once no DNS record points at
it: delete the domain's A records for ${LEFTOVER}, wait out their TTL, and
then run

  gcloud compute addresses delete ${EDGE_IP} --global --quiet

Given up while a record still points at it, the address can go to another
Google Cloud customer, and the domain's visitors with it.
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
EOF
if [[ -n "$DOMAIN" && "$SITE" == "https://${DOMAIN}" ]]; then
  # Behind the load balancer the site answers on IPv4 only, so the address to
  # look for is the device's IPv4 one, and a search for "what is my ip" shows
  # the IPv6 one on a network that has both. The two redirects are the load
  # balancer's own answers, which never reach the service; a browser that has
  # been to the site goes straight to https:// (HSTS) and skips the first.
  redirects="  curl -sS -o /dev/null -w '%{http_code}\n' http://${DOMAIN}/"
  if [[ -n "$WWW" ]]; then
    redirects="${redirects}"$'\n'"  curl -sS -o /dev/null -w '%{http_code}\n' https://${WWW}/"
  fi
  cat <<EOF

Check that no log holds a visitor IP. Open the URL from your phone or laptop,
then open https://checkip.amazonaws.com on the same device: a page of Amazon's
that shows nothing but the device's IPv4 address, which is the one the site
saw. (The site answers on IPv4 only, and a search for "what is my ip" often
shows a device's IPv6 address, which the site never sees.) Wi-Fi makes a surer
test than a mobile network, where the address can change between requests.

The load balancer also answers by itself, with a redirect, where a browser
that knows the site no longer asks. Ask from here, where 301 is the right
answer:

${redirects}

000, with an error from curl, means that nothing answered: a DNS record that
is missing or, on an https:// address, a certificate that is not ACTIVE yet.

Wait a minute, then run these here. Both must print nothing: the first looks
for the device's address anywhere but the audit logs, the second for any
request log entry at all. (The audit logs record what the project's own people
do in the console and with gcloud, with the address they did it from, and
nothing a visitor does.)

  gcloud logging read '"YOUR.IP.ADDRESS" AND NOT logName:"cloudaudit.googleapis.com"' --freshness=1d --limit=5
  gcloud logging read 'httpRequest.remoteIp:*' --freshness=1d --limit=5
EOF
else
  cat <<EOF

Check that no log holds a visitor IP: open the URL from your phone or laptop,
search "what is my ip" to learn that device's address, wait a minute, then
run these here. Both must print nothing: the first looks for that address
anywhere but the audit logs, the second for any request log entry at all.
(The audit logs record what the project's own people do in the console and
with gcloud, with the address they did it from, and nothing a visitor does.)

  gcloud logging read '"YOUR.IP.ADDRESS" AND NOT logName:"cloudaudit.googleapis.com"' --freshness=1d --limit=5
  gcloud logging read 'httpRequest.remoteIp:*' --freshness=1d --limit=5
EOF
fi
count_note
cat <<EOF

After the demo, stop paying for the warm instance:

  gcloud run services update ${SERVICE} --region ${REGION} --min-instances 0

To ship a new version later, git pull in this folder and run the same command
again: it skips what already exists, builds the new commit, and moves both the
service and the sweeper Job onto it. It finds the load balancer, if there is
one, by itself, so DOMAIN can be left off.

  bash deploy/first-deploy.sh ${PROJECT_ID} ${REGION}
EOF
