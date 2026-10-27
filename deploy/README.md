# Deploying educhan

educhan builds to a single static binary with the web UI and the SQL migrations
embedded, so a deployment is one container plus a PostgreSQL database. The image
is built by **Google Cloud Build** from the `Dockerfile` and pushed to
**Artifact Registry**; this directory holds the Cloud Run definition that runs
it.

| File | Purpose |
| --- | --- |
| `Dockerfile` | Multi-stage build; distroless runtime, non-root. Cloud Build consumes it directly |
| `cloudbuild.yaml` | The build itself: one image, tagged with the short commit SHA and `latest`, pushed to Artifact Registry |
| `.gcloudignore` | What `gcloud builds submit` uploads as the build source |
| `deploy/cloudrun/service.yaml` | Declarative Cloud Run service, Cloud SQL attached, media in Cloud Storage |

Nothing below needs a Docker daemon on your machine, and neither does local
development — the container is a deployment artifact, not a development tool.

---

## 1. Locally, with no container at all

Local development runs the server natively against a real PostgreSQL. It needs
Go 1.22+ and nothing else: `make db-up` starts a user-owned PostgreSQL cluster on
`localhost:5432` and ensures the `educhan` role and database exist. It is
`.cursor/scripts/start.sh`, it is idempotent, and it is safe to re-run.

```bash
make db-up                                        # PostgreSQL on localhost:5432
EDUCHAN_MOD_KEY=dev-mod-key go run ./cmd/server   # http://localhost:8080
```

`make run` is the second line without the moderation key. Migrations are applied
at startup, and `0006` seeds twenty boards: thirteen subject boards (`/math/` to
`/lang/`), then seven student-life ones (`/adm/` to `/meta/`).
Attachments go to `./data/uploads`, because `EDUCHAN_MEDIA_BACKEND` defaults to
`local`.

```bash
curl -s localhost:8080/healthz     # {"status":"ok"}
curl -s localhost:8080/api/boards

# Posting, with an attachment
curl -s -X POST localhost:8080/api/boards/meta/threads \
  -F subject="lab setup" -F body="finally works" -F image=@photo.jpg

# Moderation, using the key the server was started with
curl -s -X POST localhost:8080/api/boards/meta/threads/1/sticky \
  -H 'Authorization: Bearer dev-mod-key'
```

Point it at any other PostgreSQL by exporting `DATABASE_URL`; the default is
`postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable`.

```bash
psql postgres://educhan:educhan@localhost:5432/educhan
```

Two settings are worth setting even locally. `EDUCHAN_POSTER_ID_SALT` is random
per process unless you set it, so poster IDs change on every restart; and
`EDUCHAN_MOD_KEY` is empty unless you set it, which makes every moderation
endpoint answer `403`. Use throwaway values — they are development conveniences,
not secrets, and nothing reachable should run with either of them.

**The image is not part of this loop.** A change to the `Dockerfile` is checked
by submitting a build (section 2.5), not by building locally.

---

## 2. Cloud Run

**The short way.** In [Cloud Shell](https://shell.cloud.google.com), with a
project that has billing linked, one script does every step in this section but
the Pass (2.10):
APIs, Artifact Registry, service accounts, Cloud SQL, the media bucket, secrets,
the build, the deploy, and the request-log exclusion. It leaves image uploads
and verification off, keeps one instance warm for a demo (`MIN_INSTANCES=0` in
the environment turns that off), prints the service URL, and is safe to re-run.

```bash
git clone https://github.com/yuvraj-agarwal/educhan.git && cd educhan
bash deploy/first-deploy.sh YOUR_PROJECT_ID            # region defaults to asia-south1
```

It runs builds as a service account of its own (`educhan-build`) rather than
the project's default build account, so the build grants below are the same in
every project. Sections 2.1 to 2.9 are what the script does, step by step.

### 2.1 One-time project setup

```bash
export PROJECT_ID=your-project
export REGION=asia-south1

gcloud config set project "$PROJECT_ID"
gcloud services enable \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  run.googleapis.com \
  sqladmin.googleapis.com \
  secretmanager.googleapis.com \
  storage.googleapis.com \
  cloudscheduler.googleapis.com

# Somewhere to push the image. The location is part of every image reference, so
# it has to match `_REGION` in cloudbuild.yaml (or `REGION` on the make command
# line) — a repository in the wrong region is not a repository at all.
gcloud artifacts repositories create educhan \
  --repository-format=docker \
  --location="$REGION" \
  --description="educhan container images"

# The identity the service runs as — not the default compute account
gcloud iam service-accounts create educhan-run --display-name="educhan Cloud Run"
export RUN_SA="educhan-run@${PROJECT_ID}.iam.gserviceaccount.com"
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${RUN_SA}" --role=roles/cloudsql.client
```

#### What the build runs as

Cloud Build runs every build as a service account, and *which* account depends on
how old the project is: older projects use the legacy Cloud Build account,
`PROJECT_NUMBER@cloudbuild.gserviceaccount.com`, while newer ones run builds as
the Compute Engine default account,
`PROJECT_NUMBER-compute@developer.gserviceaccount.com`. Look it up rather than
assume — the console's build details name it, as does the first build's
`serviceAccount` field:

```bash
export PROJECT_NUMBER="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"
gcloud builds list --limit=1 --format='value(serviceAccount)'

# Whichever of the two it turns out to be:
export BUILD_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
```

That account needs two grants for `cloudbuild.yaml` to work:

```bash
# Push to Artifact Registry — bound to this repository, not to the project.
gcloud artifacts repositories add-iam-policy-binding educhan \
  --location="$REGION" \
  --member="serviceAccount:${BUILD_SA}" \
  --role=roles/artifactregistry.writer

# Write build logs. cloudbuild.yaml sets `logging: CLOUD_LOGGING_ONLY`, and a
# build whose logs have nowhere to go is rejected before it starts.
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${BUILD_SA}" \
  --role=roles/logging.logWriter
```

In a project that still has the automatic grants on its default service accounts
these are no-ops, because `roles/editor` already covers both. In a project with
`constraints/iam.automaticIamGrantsForDefaultServiceAccounts` enforced the
account starts with nothing, and without them the build either never starts or
dies at the push with `denied: Permission
"artifactregistry.repositories.uploadArtifacts" denied`. Granting them
explicitly is right in both cases.

`gcloud builds submit` uploads the source to a staging bucket it creates on first
use, `gs://${PROJECT_ID}_cloudbuild`, and the build reads it back from there. If
a build fails fetching its own source, that read is what is missing:

```bash
gcloud storage buckets add-iam-policy-binding "gs://${PROJECT_ID}_cloudbuild" \
  --member="serviceAccount:${BUILD_SA}" --role=roles/storage.objectViewer
```

**Deploying is a separate identity question.** `make cloud-deploy` runs
`gcloud run deploy` as *you*, so the roles that matter are on your own account:
`roles/run.developer`, plus `roles/iam.serviceAccountUser` on `$RUN_SA` — Cloud
Run will not let you deploy a service that runs as an identity you cannot act as.
Project Owner covers both. Move that deploy into Cloud Build later — a second
step in `cloudbuild.yaml`, or a trigger — and the same two roles have to move
onto `$BUILD_SA`:

```bash
# Only needed if Cloud Build itself deploys. `make cloud-deploy` does not.
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${BUILD_SA}" --role=roles/run.developer
gcloud iam service-accounts add-iam-policy-binding "$RUN_SA" \
  --member="serviceAccount:${BUILD_SA}" --role=roles/iam.serviceAccountUser
```

### 2.2 Cloud SQL

```bash
gcloud sql instances create educhan-db \
  --database-version=POSTGRES_16 \
  --edition=ENTERPRISE \
  --tier=db-f1-micro \
  --region="$REGION" \
  --storage-size=10GB

gcloud sql databases create educhan --instance=educhan-db

# Hex keeps the password free of characters that would need escaping in a URL
export DB_PASSWORD="$(openssl rand -hex 24)"
gcloud sql users create educhan --instance=educhan-db --password="$DB_PASSWORD"

# PROJECT:REGION:INSTANCE — the connection name, not the instance name
export CLOUDSQL_INSTANCE="$(gcloud sql instances describe educhan-db \
  --format='value(connectionName)')"
```

`--edition=ENTERPRISE` is needed for `db-f1-micro`: for PostgreSQL 16 and later
Cloud SQL defaults to Enterprise Plus, which has no shared-core tiers, and the
create fails without it. Shared-core tiers carry no SLA and allow about 25
connections; section 3 covers how the pool size and `maxScale` add up.

### 2.3 The media bucket

Attachments go to Cloud Storage. The container filesystem does not outlive the
container — `/tmp` is the only writable path on Cloud Run, and it is per-instance
and in-memory — so a bucket is what makes an uploaded image survive a scale to
zero, a new revision, or simply being read by a different instance than the one
that stored it.

```bash
# Bucket names are globally unique; the project ID keeps this one distinct
export MEDIA_BUCKET="${PROJECT_ID}-educhan-media"

gcloud storage buckets create "gs://${MEDIA_BUCKET}" \
  --location="$REGION" \
  --uniform-bucket-level-access \
  --public-access-prevention
```

`--uniform-bucket-level-access` turns off per-object ACLs so access is decided
in one place, and `--public-access-prevention` makes the bucket un-publishable
even by accident. **Neither is a workaround: the bucket is meant to be private.**
educhan streams media through the process on `/media/original/…` and
`/media/thumb/…`, so nothing ever reads the bucket directly.

Grant the service account access **on the bucket**, not on the project:

```bash
gcloud storage buckets add-iam-policy-binding "gs://${MEDIA_BUCKET}" \
  --member="serviceAccount:${RUN_SA}" \
  --role=roles/storage.objectAdmin
```

`roles/storage.objectAdmin` is exactly what educhan does with the bucket and no
more: create, read, delete, and list objects. It carries no permission over the
bucket itself — not to reconfigure it, not to change its IAM policy, not to
delete it — and scoping the binding to this one bucket keeps it away from every
other bucket in the project. Do not substitute `roles/storage.admin`, and do not
grant it at the project level.

Two of those verbs are load-bearing and worth naming, because a narrower role
looks tempting and breaks something:

- **delete** is how a moderator removing a post removes its image and its
  thumbnail. `roles/storage.objectCreator` alone leaves every deleted post's
  blobs in the bucket for ever.
- **list** is how the server checks at startup that the bucket is usable. The
  API answers `404` for a missing bucket and a missing object alike, so without
  that check a mistyped name or a missing grant would show up as every image
  quietly 404ing rather than as a failure to start.

### 2.4 Secrets

Three values do not belong in a deployment command: the database URL (it carries
the password), the poster-ID salt, and the moderation key.

```bash
printf 'postgres://educhan:%s@/educhan?host=/cloudsql/%s&sslmode=disable' \
  "$DB_PASSWORD" "$CLOUDSQL_INSTANCE" \
  | gcloud secrets create educhan-database-url --replication-policy=automatic --data-file=-

openssl rand -hex 32 \
  | gcloud secrets create educhan-poster-id-salt --replication-policy=automatic --data-file=-

openssl rand -hex 32 \
  | gcloud secrets create educhan-mod-key --replication-policy=automatic --data-file=-

for secret in educhan-database-url educhan-poster-id-salt educhan-mod-key; do
  gcloud secrets add-iam-policy-binding "$secret" \
    --member="serviceAccount:${RUN_SA}" --role=roles/secretmanager.secretAccessor
done
```

Selling the Pass adds three more, made only if it goes on sale (section 2.10).

**Set the salt once and leave it alone.** It keys per-thread poster IDs and
secure tripcodes; without it each instance and each revision generates its own,
so the same browser shows a different ID depending on which container answered.
A poster ID is an HMAC of the browser's poster key (or of the badge credential's
id, on a badged post) under this salt; only a post that arrives without a poster
key falls back to the client address.

### 2.5 Build and push

`cloudbuild.yaml` in the repository root is the build. It runs one `docker build`
against the repository's `Dockerfile` on Cloud Build's machines and pushes the
result to Artifact Registry under two tags — the short commit SHA and `latest` —
which name the same digest, because the image is built once and tagged twice.

```bash
make cloud-build
```

That is exactly this, with the values filled in from the Makefile:

```bash
gcloud builds submit --config cloudbuild.yaml \
  --substitutions "_REGION=${REGION},_REPOSITORY=educhan,_IMAGE=educhan,_TAG=$(git rev-parse --short HEAD)"
```

The commit is passed in rather than read from `$SHORT_SHA` because that built-in
substitution is populated only for builds started by a Cloud Build trigger; a
build submitted by hand would be tagged with an empty string. `make cloud-build`
refuses to submit when the tag or the project would be empty, so the failure is a
sentence rather than a malformed image reference.

Everything is overridable on the command line, and the defaults are
`asia-south1`, the `educhan` repository, and `gcloud config get-value project`:

```bash
make cloud-build REGION=europe-west1 PROJECT_ID=other-project IMAGE_TAG=v0.3.0
make cloud-image        # print the image reference these produce
```

What gets uploaded as the build source is governed by `.gcloudignore` — history,
`.env`, and `data/` stay behind — and inside the build, `.dockerignore` trims the
context a second time before the daemon sees it.

The pushed tags stay in the registry, which is what makes a rollback a deploy of
an older commit rather than a rebuild. **Never roll back across migration
`0005`**, to an image from before the archive existed: that code lists archived
threads as live, and its pruning deletes them with their images left in the
bucket. A binary that finds migrations it does not know logs an error saying so
and still serves, so read the log after any rollback. `-sweep` refuses to run at
all in that state. While the Pass is on, a rollback to an image from before it
retires it (section 2.10).

```bash
export IMAGE_TAG="$(git rev-parse --short HEAD)"
export IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/educhan/educhan:${IMAGE_TAG}"

gcloud artifacts docker images list \
  "${REGION}-docker.pkg.dev/${PROJECT_ID}/educhan/educhan" --include-tags
```

### 2.6 Deploy

Either apply the service definition, which keeps the whole configuration in
version control:

```bash
envsubst < deploy/cloudrun/service.yaml > /tmp/educhan-service.yaml
gcloud run services replace /tmp/educhan-service.yaml --region "$REGION"

# services replace does not set the invoker policy; make it public once
gcloud run services add-iam-policy-binding educhan --region "$REGION" \
  --member=allUsers --role=roles/run.invoker
```

(`envsubst` substitutes `PROJECT_ID`, `REGION`, `CLOUDSQL_INSTANCE`,
`MEDIA_BUCKET`, and `IMAGE_TAG` from the environment. Without gettext installed,
edit the five placeholders by hand.)

Or do it in one command:

```bash
gcloud run deploy educhan \
  --image "$IMAGE" \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest,EDUCHAN_POSTER_ID_SALT=educhan-poster-id-salt:latest,EDUCHAN_MOD_KEY=educhan-mod-key:latest" \
  --set-env-vars "EDUCHAN_TRUST_PROXY=1,EDUCHAN_MEDIA_BACKEND=gcs,EDUCHAN_GCS_BUCKET=${MEDIA_BUCKET},EDUCHAN_GCS_PREFIX=media,EDUCHAN_LOG_FORMAT=json" \
  --cpu 1 --memory 512Mi --concurrency 80 --max-instances 4 \
  --allow-unauthenticated
```

Once the service exists, every later release is just the new image:

```bash
make cloud-build && make cloud-deploy
```

`make cloud-deploy` is `gcloud run deploy educhan --image <the tag just built>`
and nothing more. Cloud Run carries the rest of the configuration — the service
account, the Cloud SQL instance, the secrets, the environment — over from the
current revision, so it is not duplicated in the Makefile and cannot drift from
it. Changing any of that is a change to `service.yaml` (or to the long form
above), which is where the service's configuration is meant to live. The target
checks the service exists first and tells you to create it if it does not, rather
than quietly creating a bare one with no database attached.

### 2.7 Verify

```bash
URL="$(gcloud run services describe educhan --region "$REGION" --format='value(status.url)')"
curl -s "$URL/healthz"     # {"status":"ok"} — this also pings PostgreSQL
curl -s "$URL/api/boards"  # the seeded boards, so migrations ran
open "$URL"                # the UI

# Media round-trips through the bucket. Post an image, read its URL back, and
# fetch it — then confirm the object is really in Cloud Storage. With
# EDUCHAN_UPLOADS_ENABLED off, as service.yaml sets it, the post answers 403
# instead; that is the check to run until uploads are switched on.
curl -s -X POST "$URL/api/boards/meta/threads" \
  -F subject="storage check" -F body="does this survive" -F image=@photo.jpg
curl -s "$URL/api/boards/meta/catalog"     # threads[].attachment.url
curl -sI "$URL/media/original/meta/2026/08/<name>.jpg"   # 200, image/jpeg
gcloud storage ls -r "gs://${MEDIA_BUCKET}/media/originals/**"
```

If the bucket is missing or the grant did not land, the revision fails to start
and the log says so by name — `cloud storage bucket "…" is not usable` — rather
than serving a site whose images all 404.

Migrations are applied at startup inside a Postgres advisory lock, so rolling out
a revision while the old one still serves traffic is safe: whichever container
gets there first applies them and the rest wait.

### 2.8 Keep client addresses out of Cloud Logging

The application never logs a client address, but Cloud Run writes its own
request log (`run.googleapis.com/requests`) for every request, and each entry
carries `httpRequest.remoteIp` and the user agent. A load balancer, once there
is one, writes another (`resource.type="http_load_balancer"`). Both land in the
`_Default` bucket and are kept for 30 days, which is a log of who posted when.
Exclude both from the `_Default` sink before the service takes public traffic.
In Cloud Shell:

```bash
gcloud logging sinks update _Default \
  --add-exclusion='name=no-request-logs,description=Request logs carry client IPs,filter=LOG_ID("run.googleapis.com/requests") OR resource.type="http_load_balancer"'
```

Check it took: the first command lists the exclusion, and the second, run after
a few requests to the service, prints nothing.

```bash
gcloud logging sinks describe _Default --format='yaml(exclusions)'
gcloud logging read 'LOG_ID("run.googleapis.com/requests")' --freshness=10m --limit=1
```

An exclusion stops entries from being stored; it does not delete the ones
already kept, which expire on the bucket's 30-day retention. The `_Required`
sink only carries audit logs, so it holds no request entries. Application logs
(stdout and stderr) are unaffected and still reach Cloud Logging, which is why
the app keeps addresses out of them itself.

### 2.9 The archive sweeper

A thread that falls off the end of its board is archived, and the sweeper
deletes it, with its images, once its board's `archive_days` (7 by default)
have passed. It is the same image run as `educhan -sweep`: one pass, then it
exits. It runs as a Cloud Run **Job**, started once a day by Cloud Scheduler,
because a Cloud Run service only has CPU while it answers a request, so a timer
inside it would fire whenever it happened to.

The Job runs as the service's account and needs only what deleting takes: the
database, and `storage.objects.get` and `storage.objects.delete` on the bucket,
both already in the `roles/storage.objectAdmin` grant from section 2.3 (it checks
that each file is there before deleting it). It gets neither the moderation key nor
`EDUCHAN_VERIFY_SECRET`, which it has no use for (and the server refuses to start
with that secret set and no mail sender). The poster-ID salt is left out too, as
nothing the sweeper does touches a poster ID. So is every educhan Pass setting,
whether or not the service sells the Pass: each run prunes the Pass's tables
without them, because every order carries the term it was sold with and every
other row its own deadline, and a Pass setting given without
`EDUCHAN_PASS_SECRET` would stop the Job from starting.

```bash
gcloud run jobs deploy educhan-sweep \
  --image "$IMAGE" \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --args=-sweep \
  --set-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest" \
  --set-env-vars "EDUCHAN_MEDIA_BACKEND=gcs,EDUCHAN_GCS_BUCKET=${MEDIA_BUCKET},EDUCHAN_GCS_PREFIX=media,EDUCHAN_LOG_FORMAT=json,EDUCHAN_DB_MAX_CONNS=3" \
  --tasks 1 --max-retries 1 --task-timeout 15m \
  --cpu 1 --memory 512Mi
```

The media settings (`EDUCHAN_MEDIA_BACKEND`, `EDUCHAN_GCS_BUCKET`,
`EDUCHAN_GCS_PREFIX`) must match the service's exactly. The sweeper queues each
swept file's key in `media_deletions` in the same transaction that deletes its
rows, then removes the files one by one; a queued file the store does not hold
makes the run exit non-zero with an error naming those settings, and the key
stays queued. The Job's image must also be no older than the service's: `-sweep`
refuses to run against a database migrated by a newer release, which is why
`make cloud-deploy` moves the two together.

Run it once by hand and read what it did; the log line carries counts only,
never which boards or threads went:

```bash
gcloud run jobs execute educhan-sweep --region "$REGION" --wait
gcloud logging read 'resource.type="cloud_run_job" AND resource.labels.job_name="educhan-sweep" AND jsonPayload.msg="sweep"' \
  --freshness=1h --limit=1 --format='value(jsonPayload)'
```

Then schedule it. Cloud Scheduler calls the Run API as its own service account,
which may run this one Job and nothing else:

```bash
gcloud iam service-accounts create educhan-scheduler --display-name="educhan sweeper schedule"
export SCHEDULER_SA="educhan-scheduler@${PROJECT_ID}.iam.gserviceaccount.com"
gcloud run jobs add-iam-policy-binding educhan-sweep --region "$REGION" \
  --member="serviceAccount:${SCHEDULER_SA}" --role=roles/run.invoker

gcloud scheduler jobs create http educhan-sweep-daily \
  --location "$REGION" \
  --schedule "17 3 * * *" --time-zone "Etc/UTC" \
  --http-method POST \
  --uri "https://run.googleapis.com/v2/projects/${PROJECT_ID}/locations/${REGION}/jobs/educhan-sweep:run" \
  --oauth-service-account-email "$SCHEDULER_SA"
```

A sweep that could not remove a file exits non-zero, so the execution shows as
failed in the console (`gcloud run jobs executions list --job educhan-sweep
--region "$REGION"`). The file's key stays in `media_deletions` and the next run,
or the Job's own retry, tries it again, so a retry is always safe. In the log,
`blobs_missing` above zero means the media settings are wrong; `blob_failures`
means the store refused or was unreachable.

### 2.10 Selling the educhan Pass (optional)

The Pass is off until `EDUCHAN_PASS_SECRET` is set, and nothing else in this
guide needs it. Turn it on only when it goes on sale. The server refuses to
start with some of the Pass settings and not the others, so they go on
together, in one `gcloud run services update`, or not at all. They go on the
service only: the sweeper Job gets none of them, and would refuse to start with
any of them set without the secret (section 2.9).

What a Pass buys is skipping the captcha, so sell it only where the captcha is
on, which `GET /api/site` shows as `"captcha":true`; a service made before the
captcha gets its secret first (section 2.4). The block below checks it, and
makes nothing otherwise.

**In the Stripe dashboard first.** Under *Settings → Public details*, set a
support email: it is printed on every receipt, and a buyer who lost their code
writes there to have it reissued. Then make a restricted key (*Developers → API
keys → Create restricted key*) with **Checkout Sessions: Write** and **Prices:
Read** and nothing else. Those are the only two calls the server makes. A full
secret key also works but can do far more than the server needs; the server
refuses to start with anything but an `sk_…` or `rk_…` key. Turn on Stripe's
email receipts for successful payments, and set up no custom domain for
Checkout: the server sends buyers only to `https://checkout.stripe.com` and
refuses any other address Stripe hands back.

In Cloud Shell. Past its first lines the block runs in a subshell that stops at
the first step that fails, so a run either finishes or says where it stopped,
and the keys, read with `read`, stay out of your shell and its history:

```bash
export REGION="${REGION:-asia-south1}"   # the region the service runs in
export RUN_SA="educhan-run@$(gcloud config get-value project).iam.gserviceaccount.com"
# The one address readers use, as a bare origin: no path, and a trailing slash
# is dropped. With a domain of your own, export PUBLIC_URL=https://YOUR-DOMAIN
# before every run of this block (below); without one, it is the run.app
# address first-deploy.sh prints.
export PUBLIC_URL="${PUBLIC_URL:-$(gcloud run services describe educhan --region "$REGION" --format='value(status.url)')}"
PUBLIC_URL="${PUBLIC_URL%/}"

(
set -euo pipefail
trap 'echo "Stopped at the step that failed above; nothing after it ran. Run the block again." >&2' ERR
field() { python3 -c "import sys,json;print(json.load(sys.stdin)[\"$1\"])"; }

# Checked before anything is made in Stripe.
gcloud run services describe educhan --region "$REGION" --format='value(metadata.name)' >/dev/null ||
  { echo "No service educhan in ${REGION}: export REGION with the service's region."; exit 1; }
[[ $PUBLIC_URL =~ ^https://[^/?#]+$ ]] ||
  { echo "PUBLIC_URL must be a bare https:// origin, not ${PUBLIC_URL}."; exit 1; }
[[ $(curl -fsS "${PUBLIC_URL}/api/site") == *'"captcha":true'* ]] ||
  { echo "${PUBLIC_URL} does not answer, or its captcha is off (section 2.4)."; exit 1; }
read -rsp 'Stripe restricted key: ' STRIPE_KEY; echo
read -rsp 'Stripe secret key (for the setup calls only): ' STRIPE_ADMIN; echo
[[ $STRIPE_KEY == [rs]k_* && $STRIPE_ADMIN == [rs]k_* ]] ||
  { echo 'Those are not Stripe keys (sk_… or rk_…).'; exit 1; }

# The product and its one-time price: USD 20.00, a year of the Pass. The amount
# is in the currency's smallest unit (2000 = USD 20.00), and any tax is part of
# it, not added on top (below). A new price later is a new Price and a new
# EDUCHAN_STRIPE_PRICE_ID, not a code change.
PRODUCT=$(curl -fsS https://api.stripe.com/v1/products -u "${STRIPE_ADMIN}:" \
  -d name="educhan Pass" | field id)
PRICE_ID=$(curl -fsS https://api.stripe.com/v1/prices -u "${STRIPE_ADMIN}:" \
  -d product="$PRODUCT" -d currency=usd -d unit_amount=2000 -d tax_behavior=inclusive \
  | field id)

# The webhook endpoint, pinned to the API version the server is written
# against. Its signing secret is returned by this call and no other.
ENDPOINT_JSON=$(curl -fsS https://api.stripe.com/v1/webhook_endpoints -u "${STRIPE_ADMIN}:" \
  -d url="${PUBLIC_URL}/api/pass/stripe-webhook" -d api_version=2024-06-20 \
  -d "enabled_events[]=checkout.session.completed" \
  -d "enabled_events[]=checkout.session.async_payment_succeeded" \
  -d "enabled_events[]=checkout.session.async_payment_failed" \
  -d "enabled_events[]=checkout.session.expired" \
  -d "enabled_events[]=charge.refunded" \
  -d "enabled_events[]=charge.dispute.created")
ENDPOINT=$(field id <<<"$ENDPOINT_JSON")
WEBHOOK_SECRET=$(field secret <<<"$ENDPOINT_JSON")

# Made once and never replaced: every stored Pass is a hash under it.
gcloud secrets describe educhan-pass-secret >/dev/null 2>&1 ||
  openssl rand -hex 32 | gcloud secrets create educhan-pass-secret --replication-policy=automatic --data-file=-
for secret in educhan-stripe-secret-key educhan-stripe-webhook-secret; do
  gcloud secrets describe "$secret" >/dev/null 2>&1 ||
    gcloud secrets create "$secret" --replication-policy=automatic
done
for secret in educhan-pass-secret educhan-stripe-secret-key educhan-stripe-webhook-secret; do
  gcloud secrets add-iam-policy-binding "$secret" \
    --member="serviceAccount:${RUN_SA}" --role=roles/secretmanager.secretAccessor
done
# The two Stripe values get a new version on every run, to match what it made,
# and the service moves onto them straight away.
printf '%s' "$STRIPE_KEY" | gcloud secrets versions add educhan-stripe-secret-key --data-file=-
printf '%s' "$WEBHOOK_SECRET" | gcloud secrets versions add educhan-stripe-webhook-secret --data-file=-
gcloud run services update educhan --region "$REGION" \
  --update-secrets "EDUCHAN_PASS_SECRET=educhan-pass-secret:latest,EDUCHAN_STRIPE_SECRET_KEY=educhan-stripe-secret-key:latest,EDUCHAN_STRIPE_WEBHOOK_SECRET=educhan-stripe-webhook-secret:latest" \
  --update-env-vars "EDUCHAN_STRIPE_PRICE_ID=${PRICE_ID},EDUCHAN_PUBLIC_URL=${PUBLIC_URL}"
echo "Done. The service verifies webhook endpoint ${ENDPOINT}, under Product ${PRODUCT}."
)
```

Running the block again is safe, but each run that gets past its checks makes
another Product, Price and webhook endpoint in Stripe. An instance reads the
newest version of each secret when it starts, so once a run has added its
versions, instances started from then on verify that run's endpoint, even if
the run stopped at the update after. So run the block until it prints "Done",
and only then delete every endpoint but the one it named (*Developers →
Webhooks*), whose deliveries the server can't verify, and archive every Product
but its own.

`curl -s "${PUBLIC_URL}/api/pass"` should then answer `"enabled":true` with the
price. The Price is tax-inclusive, so every buyer pays USD 20.00. To have Stripe
Tax collect sales tax, VAT or GST, switch Stripe Tax on in the account and add
`EDUCHAN_STRIPE_AUTOMATIC_TAX=1` to the env vars: Stripe then takes any tax out
of the USD 20.00 rather than adding it. A Price made without `tax_behavior` can
still be given it once, but one made tax-exclusive can't be changed: make a new
Price the way the block does, with `tax_behavior=inclusive`, set its id with
`gcloud run services update educhan --region "$REGION" --update-env-vars
EDUCHAN_STRIPE_PRICE_ID=price_…`, and only then archive the old one, since an
archived Price can't be sold. Three more settings have working defaults and need
no change: `EDUCHAN_PASS_DAYS`, `EDUCHAN_PASS_HOURLY_POSTS` and
`EDUCHAN_PASS_COOLDOWN` (section 4).

`PUBLIC_URL` is where Stripe sends a buyer back and where it sends its
webhooks, and it has to be the one address readers use: the cookie that lets a
browser collect its code belongs to the address the checkout began on, so a
checkout begun anywhere else cannot be collected where Stripe returns the buyer.
A service answers on more than one `run.app` address (`gcloud` may print another
as "Service URL" while it deploys), so give readers only the one the block
takes, which `first-deploy.sh` prints after "educhan is live at". If readers
come in through a domain of your own, export `PUBLIC_URL` with it before running
the block, and turn the `run.app` addresses off once the domain serves the site
(`gcloud run services update educhan --region "$REGION" --no-default-url`);
`first-deploy.sh` then checks and prints an address that no longer answers, so
check your domain's `/api/pass` yourself.

**Moving to a domain once the Pass is on sale** takes the webhook endpoint with
it, and readers have to carry their Passes over: a collected Pass is kept only
in the browser, under the address it was collected on, so the domain can't read
one collected on `run.app`. The move is therefore two updates, with the
`run.app` addresses still answering in between. The first, once the domain
serves the site, moves the endpoint and the return address and pauses sales,
since a checkout begun on `run.app` from then on would come back to the domain,
where it can't be collected. Editing the endpoint's URL keeps its signing
secret, so Secret Manager needs no change:

```bash
export REGION="${REGION:-asia-south1}"
export PUBLIC_URL=https://YOUR-DOMAIN   # a bare origin, as above
PUBLIC_URL="${PUBLIC_URL%/}"

read -rsp 'Stripe secret key: ' STRIPE_ADMIN; echo
# Newest first. Move the one named by the setup run that last printed "Done",
# with no run stopped part way since (see the cleanup above).
curl -fsS 'https://api.stripe.com/v1/webhook_endpoints?limit=100' -u "${STRIPE_ADMIN}:" \
  | python3 -c 'import sys,json;[print(e["id"],e["url"]) for e in sorted(json.load(sys.stdin)["data"],key=lambda e:-e["created"])]'
read -rp 'Endpoint to move (we_…): ' ENDPOINT
# The service changes only if the domain serves the Pass and the endpoint moved.
if [[ $PUBLIC_URL =~ ^https://[^/?#]+$ && $ENDPOINT == we_* &&
      $(curl -fsS "${PUBLIC_URL}/api/pass") == *'"enabled":true'* ]] &&
   curl -fsS "https://api.stripe.com/v1/webhook_endpoints/${ENDPOINT}" -u "${STRIPE_ADMIN}:" \
     -d url="${PUBLIC_URL}/api/pass/stripe-webhook" >/dev/null; then
  gcloud run services update educhan --region "$REGION" \
    --update-env-vars "EDUCHAN_PUBLIC_URL=${PUBLIC_URL},EDUCHAN_PASS_SALES=paused"
else
  echo 'The domain does not serve the Pass yet, or the endpoint did not move, so the service is unchanged.'
fi
unset STRIPE_ADMIN
```

Then give readers the domain. While the `run.app` addresses still answer, a
holder copies their code from the Pass page there (Copy the code) and enters it
on the domain's, and a checkout begun there before the update, a slow payment
included, is still collected there. Leave them answering as long as readers
need, then turn them off and open sales again in one update:

```bash
gcloud run services update educhan --region "$REGION" \
  --no-default-url --update-env-vars EDUCHAN_PASS_SALES=open
```

If sales were paused before the move, while waiting to retire the Pass say,
leave out `--update-env-vars EDUCHAN_PASS_SALES=open` so they stay paused.

A holder who did not carry their code over, or whose checkout on `run.app` was
not collected by then, writes to the support address on the receipt and gets a
reissue code (the README's *Lost codes*), though a Pass whose purchase was
refunded or disputed may have no payment left to reissue it from (also there).

Re-running `first-deploy.sh` keeps these settings. Applying `service.yaml` with
`services replace`, or running the long form in section 2.6 again, does not:
each sets the whole configuration, so it takes every Pass setting off and
retires the Pass without the pause (below). If you manage the service that way,
uncomment the Pass entries in `service.yaml` with your Price ID and
`PUBLIC_URL`, and `EDUCHAN_STRIPE_AUTOMATIC_TAX` if you set it, or add them to
the long form, before you run it again, and keep `EDUCHAN_PASS_SALES=paused`
there while sales are paused, or the next run opens them. With the `run.app`
addresses turned off, `service.yaml` needs
`run.googleapis.com/default-url-disabled: "true"` under the Service's
`metadata.annotations` as well (it is there, commented out), or each
`services replace` turns them back on. The Service's metadata takes one
`annotations:` key, so if it already has one, such as section 2.6's
`invoker-iam-disabled`, add the line under it: with a second key, one of the two
is dropped.

**To stop selling, pause.** This leaves everything already sold working:

```bash
gcloud run services update educhan --region "$REGION" \
  --update-env-vars EDUCHAN_PASS_SALES=paused
```

Checkout then refuses purchases and renewals alike with `403`, before it reads
the request's body or asks Stripe anything, and the Pass page says "The Pass is not on
sale right now." where Buy and Renew were. Everything else goes on: Stripe's
webhooks are still verified and recorded, so a checkout opened before the pause
and paid after it is collected, and a refund or dispute still takes back what it
paid for; existing Passes keep skipping the captcha; and the page still
collects, shows and checks codes, and reissue codes are still made and redeemed.
`EDUCHAN_PASS_SALES=open` (the default) sells again; any other value refuses to
start. Re-running `first-deploy.sh` keeps the pause.

**Retiring the Pass is a different step:** take the secret and every other Pass
setting off together, or the server refuses to start. Removing a setting that
was never set is harmless, so the same command works whichever optional ones
you added:

```bash
gcloud run services update educhan --region "$REGION" \
  --remove-secrets EDUCHAN_PASS_SECRET,EDUCHAN_STRIPE_SECRET_KEY,EDUCHAN_STRIPE_WEBHOOK_SECRET \
  --remove-env-vars EDUCHAN_STRIPE_PRICE_ID,EDUCHAN_PUBLIC_URL,EDUCHAN_STRIPE_AUTOMATIC_TAX,EDUCHAN_PASS_DAYS,EDUCHAN_PASS_HOURLY_POSTS,EDUCHAN_PASS_COOLDOWN,EDUCHAN_PASS_SALES
```

Every Pass endpoint then answers as if there were no Pass, and existing Passes
stop skipping the captcha. The webhook answers `404`, and Stripe gives up on an
event after about three days, so an event sent meanwhile is lost unless the
settings are back before then: a payment among them never becomes a Pass, and a
refund or dispute takes nothing back. So pause first, and retire only once
nothing is in flight: no checkout still open (Stripe keeps one open for up to a
day), no payment still processing (a bank debit can take a couple of weeks,
and the server waits 30 days for one; the Dashboard's *Payments* lists any
still pending), and no refund or dispute pending. Waiting 31 days after the
pause covers the first two. The tables stay and the sweep keeps
pruning them; setting the same `educhan-pass-secret` again brings back every
Pass not yet pruned. Before that, check in the Stripe Dashboard that the
webhook endpoint is still enabled: Stripe can disable one that keeps failing.

**A rollback can retire it too.** While the Pass is on, on sale or paused,
rolling the service back to an image from before the Pass (before migration
`0012`) does what retiring does, without the pause: that code answers `404` at
the webhook. Roll back no further than the first release with the Pass, or
retire it first as above. If it has happened, roll forward within three days
and Stripe's retries land; then check the endpoint is still enabled.

**Reissuing a lost code needs the moderation key.** The owner makes a reissue
code with `POST /api/pass/reissue`, which sits behind `EDUCHAN_MOD_KEY`
(the README's *Lost codes*). `first-deploy.sh` already sets that key on the
service, so there is nothing to add here; a service set up by hand needs it
too, or no lost code can be reissued. It is not a Pass setting, so it stays
when the Pass is retired.

**Set `EDUCHAN_PASS_SECRET` once and leave it alone.** Every stored Pass code and
payment record is a hash under it, so rotating it cancels every Pass that was
sold, and leaves no record a refund or a reissue could find.

**Keep the webhook reachable.** Stripe calls `POST /api/pass/stripe-webhook` from
its own addresses, and a buyer who has paid gets nothing until that call lands.
Stripe retries for up to three days, but once a load balancer and Cloud Armor
are in front of the service, no rate or geography rule may block that path. The
service also needs outbound HTTPS to `api.stripe.com`. That is open by default;
if egress is ever restricted, allow it.

---

## 3. How the Cloud SQL connection works

Cloud Run mounts the instance's Unix socket at `/cloudsql/<connection name>`
whenever `--add-cloudsql-instances` (or the `run.googleapis.com/cloudsql-instances`
annotation) names it. `pgx` connects to a socket when the connection string's
`host` is a path, so no proxy process and no code change is involved:

```
postgres://educhan:PASSWORD@/educhan?host=/cloudsql/PROJECT:REGION:INSTANCE&sslmode=disable
```

`sslmode=disable` is correct here — the socket is local to the sandbox and Cloud
Run encrypts the hop to the instance itself. If the password contains characters
that would need percent-encoding, use the keyword/value form instead, which `pgx`
accepts just as well and needs no escaping:

```
user=educhan password=PASSWORD dbname=educhan host=/cloudsql/PROJECT:REGION:INSTANCE
```

Everywhere else — a VM, a laptop, another cloud — run the [Cloud SQL Auth
Proxy](https://cloud.google.com/sql/docs/postgres/sql-proxy) and point
`DATABASE_URL` at the local port it opens, which is the same TCP connection
string as any other PostgreSQL:

```bash
cloud-sql-proxy "$CLOUDSQL_INSTANCE" --port 5432 &
export DATABASE_URL='postgres://educhan:PASSWORD@localhost:5432/educhan?sslmode=disable'
```

Each container keeps its own `pgxpool`, capped by `EDUCHAN_DB_MAX_CONNS` (default
`3`; it overrides any `pool_max_conns` in the connection string). Total usage is
that cap times the number of running instances, and it has to fit what the tier
allows. The budget for `db-f1-micro`, which allows 25 connections with a few
reserved for Cloud SQL's own superuser:

| Use | Connections |
| --- | --- |
| Service: `maxScale` 4 × `EDUCHAN_DB_MAX_CONNS` 3 | 12 |
| A rollout's new revision starting while the old one drains | up to 12 more briefly, usually far fewer |
| One-off Cloud Run Jobs (admin bootstrap, a sweeper), at the same cap | 3 |
| You in Cloud SQL Studio or `psql` | 1–2 |

Raising either `maxScale` or the pool size means doing this sum again, or
moving to a dedicated-core tier, which allows far more connections.

---

## 4. Environment variables

The full table lives in the [README](../README.md#configuration). These are the
ones whose value differs between a development machine and Cloud Run:

| Variable | Local (`make run`) | Cloud Run | Notes |
| --- | --- | --- | --- |
| `PORT` | unset | injected | Cloud Run's contract. Used as the listen port when `EDUCHAN_ADDR` is unset |
| `EDUCHAN_ADDR` | `:8080` (default) | leave unset | Setting it overrides `PORT`, which breaks Cloud Run's health checking |
| `DATABASE_URL` | `postgres://educhan:educhan@localhost:5432/educhan?sslmode=disable` | from Secret Manager | The default matches `make db-up`. Unix-socket form on Cloud Run; see section 3 |
| `EDUCHAN_MEDIA_BACKEND` | `local` | `gcs` | Where attachments live. `local` on Cloud Run means they do not outlive the container |
| `EDUCHAN_UPLOAD_DIR` | `./data/uploads` | unused | Only read by the `local` backend |
| `EDUCHAN_GCS_BUCKET` | unused | the media bucket | Required by the `gcs` backend; the service account needs `roles/storage.objectAdmin` on it (section 2.3) |
| `EDUCHAN_GCS_PREFIX` | unused | `media` | Optional key prefix, so the bucket can hold other things too |
| `EDUCHAN_POSTER_ID_SALT` | random per process | from Secret Manager | Set it locally too, or poster IDs change on every restart. Must be stable across instances and revisions |
| `EDUCHAN_MOD_KEY` | unset; export a throwaway | from Secret Manager | Unset means every moderation endpoint answers `403` |
| `EDUCHAN_VERIFY_SECRET` | unset unless testing verification | **unset** | Institutional verification. Unset switches it off; rotating it revokes every badge. Left off on Cloud Run until a mail `Sender` exists — see below |
| `EDUCHAN_TRUST_PROXY` | `0` | `1` | Cloud Run's front end appends exactly one hop to `X-Forwarded-For` |
| `EDUCHAN_LOG_FORMAT` | `text` (default) | `json` | JSON is what Cloud Logging parses into structured fields |
| `EDUCHAN_THREAD_COOLDOWN` / `_POST_` / `_UPLOAD_` | `30` / `5` / `15` | same | Seconds; `0` disables one |
| `EDUCHAN_MAX_UPLOAD_BYTES` | `8388608` | `8388608` | Cloud Run's own request limit is 32 MiB |
| `EDUCHAN_DB_MAX_CONNS` | `3` (default) | `3` | Connection pool cap per instance; times `maxScale` it must fit the Cloud SQL tier (section 3) |
| `EDUCHAN_UPLOADS_ENABLED` | `1` when testing images | **`false`** | Off refuses image posts with `403`. Keep it off until images are stripped of metadata and scanned |
| `EDUCHAN_MAIL_DEV` | `1` when testing verification | **unset** | Allows the development sender. Never on Cloud Run, where stdout is Cloud Logging |
| `EDUCHAN_PASS_SECRET` | unset unless testing the Pass | from Secret Manager, once the Pass is on sale | Unset switches the Pass off, and any other Pass setting given without it refuses to start. Set together with the four below (section 2.10). Never on the sweeper Job, nor is any Pass setting (section 2.9) |
| `EDUCHAN_STRIPE_SECRET_KEY` / `_WEBHOOK_SECRET` | unset | from Secret Manager | The restricted API key and the webhook endpoint's `whsec_…` signing secret |
| `EDUCHAN_STRIPE_PRICE_ID` / `EDUCHAN_PUBLIC_URL` | unset | the `price_…` id / the `https://` origin readers use | Plain env; the public URL is where Stripe sends a buyer back, a bare origin (any path refuses to start), and the only one a checkout can be collected on (section 2.10) |
| `EDUCHAN_STRIPE_AUTOMATIC_TAX` | unset | unset, or `1` with Stripe Tax on | Stripe works out the tax at checkout and takes it out of the tax-inclusive Price, so every buyer still pays its amount (section 2.10) |
| `EDUCHAN_PASS_DAYS` | `365` (default) | `365` (default) | How long a Pass lasts, from `1` to `36500`. Each order keeps the term it was sold with, so a change applies only to checkouts started after it |
| `EDUCHAN_PASS_HOURLY_POSTS` | `30` (default) | `30` (default) | Posts in one clock hour that skip the captcha; at least `1` |
| `EDUCHAN_PASS_COOLDOWN` | `30` (default) | `30` (default) | Seconds between checkouts from one client; not negative |
| `EDUCHAN_PASS_SALES` | `open` (default) | `open`, or `paused` to stop selling | `paused` refuses new purchases and renewals and keeps everything sold working; any other value refuses to start (section 2.10) |

`EDUCHAN_TRUST_PROXY` is the one worth getting right. It is a hop count, and a
value higher than the real chain lets a client pick its own rate-limit bucket by
sending an `X-Forwarded-For` of its own. Poster IDs mostly no longer depend on
it: they come from the browser's poster key, and only a post sent without one
(a script, say) falls back to the client address. Cloud Run is one hop; put a
load balancer or CDN in front and it becomes two.

---

## 5. Limitations

**Media is served through the process, not from the bucket.** Every image and
thumbnail is streamed by the container on `/media/original/…` and
`/media/thumb/…`, which is what keeps the bucket private and the URLs, headers,
and range handling identical on both backends. The cost is that attachment
traffic uses container CPU and egress, and that a popular thread's images are
fetched from Cloud Storage on every cache miss rather than by the client
directly. Signed URLs would remove that hop — `/media/…` would answer `302` with
a short-lived URL instead of the bytes — and are the obvious next optimisation,
along with a CDN in front. Neither is built; both are additive, because the
storage layout and the object metadata already suit them: object names are
stable, and each object carries the same immutable `Cache-Control` the handler
sets today. Until then, keep an eye on egress if traffic grows.

**Uploads do not survive an instance on the `local` backend.** That is what
`EDUCHAN_MEDIA_BACKEND=gcs` is for, and the configuration above sets it. Leave the
backend at `local` on Cloud Run and `EDUCHAN_UPLOAD_DIR` can only point at `/tmp`,
which is per-instance and in-memory: it is discarded when the container stops, it
counts against the memory limit, and an image uploaded to one instance answers
`404` from another. Use `local` there only for a throwaway demo, and pin
`--max-instances 1` if you do.

**Flood control is per instance.** `cooldownLimiter` in
`internal/app/ratelimit.go` holds its state in process memory, so with N
instances a client gets up to N times the intended allowance. Moving it to shared
storage — Memorystore, or a table — means replacing that one type, which nothing
else depends on. The verification code cooldown rides on the same limiter, so it
is per instance too.

**So is the Pass's hourly budget.** `passBudget` in `internal/app/pass.go` counts
in process memory too, so with N instances a Pass can skip the captcha up to N
times `EDUCHAN_PASS_HOURLY_POSTS` in an hour.

**Institutional verification has no mail sender yet.** `EDUCHAN_VERIFY_SECRET` is
left unset in `service.yaml`, which switches the feature off, and it should stay
that way here until `internal/mailer` grows an SMTP or Google Workspace `Sender`.
Setting it anyway makes the server refuse to start unless `EDUCHAN_MAIL_DEV=1`
is also set, which it must not be here.
The development sender prints the code *and the address it was sent to* to
stdout, and on Cloud Run stdout is Cloud Logging — which would write into a log
sink exactly the thing the feature promises never to store. Enabling it is one
new `Sender` plus the commented-out secret block in `service.yaml`.

**Everything else is already stateless.** Boards, threads, posts, and attachment
metadata live in PostgreSQL, and the migrations are embedded in the binary and
guarded by an advisory lock, so instances can start in any order and any number.

**The image is larger than it was.** The Cloud Storage client brings a sizeable
dependency tree with it: the stripped static binary goes from about 11 MB to
about 33 MB, and the image grows by the same amount. It is paid once at deploy
time and does not affect request latency, but it does lengthen a cold pull —
`run.googleapis.com/startup-cpu-boost` is already set in `service.yaml`, and
keeping `minScale` above zero avoids the pull on a cold start altogether if the
extra cost is worth it to you.

**Builds are submitted by hand.** `cloudbuild.yaml` is run by `make cloud-build`
from a checkout, not by a Cloud Build trigger watching the repository, so nothing
builds or deploys on a push. GitHub Actions (`.github/workflows/ci.yml`) still
runs the tests on every push and does not build the image. Adding a trigger is a
console or `gcloud builds triggers create` change and needs no edit to this
config, beyond letting `$SHORT_SHA` supply `_TAG` — see section 2.5.

**Not covered here:** Cloud Build triggers and CD, custom domains and
certificates, Cloud CDN, Cloud Armor, and backups beyond Cloud SQL's automatic
ones.

---

## 6. How the storage backend is chosen

Nothing in the handlers or the SQL touches the filesystem. `internal/media.Store`
is the only route to blob storage:

```go
type Store interface {
	Put(ctx context.Context, key, contentType string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadSeekCloser, Object, error)
	Delete(ctx context.Context, key string) error
}
```

Two implementations satisfy it — `LocalStore` in `internal/media/local.go` and
`GCSStore` in `internal/media/gcs.go` — and `newMediaStore` in
`internal/app/app.go` is the single place either is constructed, from
`EDUCHAN_MEDIA_BACKEND`. Nothing above the interface knows which one answered:
no handler, no query, no column, and no URL.

Keys are the reason the swap is this cheap. They were already slash-separated,
relative, and validated by `CleanKey`, which is exactly the shape of an object
name, so `originals/meta/2026/08/{random}.jpg` addresses a file on disk and an
object in a bucket without translation. `GCSStore` applies the same validation
even though a flat namespace could not be escaped by a `..`, so that a key stays
portable: a bucket can be synced to a directory and back.

`internal/media/conformance_test.go` is what keeps the two honest. It is one
suite — round trips, content types, missing keys, traversal rejection, both
renditions written and removed, cleanup after a partial write, and serving
through `http.ServeContent` including range requests — run against each backend
in turn. `internal/app/media_backend_test.go` then runs the whole attachment
story end to end, through the real routes, over both. Both use an in-process
Cloud Storage emulator, so they need no credentials, no network, and no
container, and nothing skips for want of them.

Credentials are Application Default Credentials and nothing else: the attached
service account on Cloud Run, `GOOGLE_APPLICATION_CREDENTIALS` locally. There is
no setting that takes a key file, and none belongs in the image or the
repository.

**Not covered by this backend:** signed URLs and direct-to-bucket resumable
uploads, CDN configuration, RAW image and video support, and lifecycle or
cold-storage tiering. Object names are stable and prefixed, so a lifecycle rule
can be added later without touching the application.
