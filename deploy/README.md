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
`local`, but image posts answer `403` until the server is started with
`EDUCHAN_UPLOADS_ENABLED=1` as well, as the attachment example below needs.

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
project that has billing linked, one script does every step in this section:
APIs, Artifact Registry, service accounts, Cloud SQL, the media bucket, secrets,
the build, the deploy, the request-log exclusion, and the archive sweeper Job
with its daily schedule. Re-running it from a newer checkout is how an update
ships: it moves the service and the Job onto the new image together, sets the
variables and secrets it names, and keeps any others added by hand since. Every
run still turns image uploads and verification off, and sets the warm instance
count: one, for a demo, unless `MIN_INSTANCES=0` is in the environment. It
prints the service URL and is safe to re-run. To take a setting off on purpose,
use `gcloud run services update educhan --region "$REGION"` with
`--remove-env-vars NAME` or, for a secret, `--remove-secrets NAME`; the sweeper
takes the same flags through `gcloud run jobs update educhan-sweep`.

```bash
git clone https://github.com/yuvraj-agarwal/educhan.git && cd educhan
bash deploy/first-deploy.sh YOUR_PROJECT_ID            # region defaults to asia-south1
```

It runs builds as a service account of its own (`educhan-build`) rather than
the project's default build account, so the build grants below are the same in
every project. The rest of this section is what the script does, step by step.

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
  --storage-size=10GB \
  --backup --backup-start-time=05:00 --retained-backups-count=7

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

**Leave `track_commit_timestamp` off.** It is off by default, in PostgreSQL and
in Cloud SQL, and none of the commands here set it. Switched on, it records when
each transaction committed, and `pg_xact_commit_timestamp(xmin)` turns that into
the time any row was last written, to well under a second. A verification
credential issued while it was on could then be dated to that moment, not just
placed between the two posts its own transaction id falls between, until the
next sweep rewrites it and gives it the sweep's id, and in a backup taken before
then for as long as that backup is kept; and so could each committed transaction
id missing from the posts' sequence, for as long as PostgreSQL keeps its commit
time, which turns the gap a code request or confirmation leaves among the posts'
ids from the minutes between two posts into the moment of the write. The root
README's [Transaction ids, and the nightly
rewrite](../README.md#transaction-ids-and-the-nightly-rewrite) has the rest.
Never add it to `--database-flags`; this shows the instance's flags, and should
print nothing about it:

```bash
gcloud sql instances describe educhan-db --format='value(settings.databaseFlags)'
```

**Backups.** The three backup flags spell out the backup schedule and how many
backups are kept, rather than leaning on defaults. The instance keeps a daily
backup, the last seven of them, so a deleted row can sit in a backup for about a
week. Each backup starts somewhere in a four-hour window opening at 05:00 UTC,
after the archive sweeper's run at 03:17 and its one retry (section 2.9), so a
backup holds as little as possible of what the sweep deletes or rewrites. 05:00
UTC is also when the day a verification credential is dated to ends
(`credentialDayStart` in `internal/app/verify.go`), and it ends then so that the
sweep, which rewrites every credential, runs inside it, near its end; the
root README's [Transaction ids, and the nightly
rewrite](../README.md#transaction-ids-and-the-nightly-rewrite) says why. So move
none of the three times without the others: section 2.9 says how far the
schedule may move. On an instance that already has backups on, re-running
`first-deploy.sh` moves the window to 05:00 and changes nothing else; it leaves
alone an instance whose backups were turned off.

Point-in-time recovery is off: `gcloud sql instances create` leaves it off for
the Enterprise edition, where the Console would have turned it on. It would keep
seven days of write-ahead log, and every commit record in that log carries its
commit time, which would date each write exactly within those seven days,
whatever `track_commit_timestamp` (above) says. Turning it on is a trade between
recovery and that record, so it is a decision, not a default. To check what an
instance keeps:

```bash
gcloud sql instances describe educhan-db --format='yaml(settings.backupConfiguration)'
```

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
all in that state.

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

`make cloud-deploy` is `gcloud run deploy educhan --image <the tag just built>`,
plus moving the archive sweeper Job onto the same image once it exists (section
2.9), and nothing more. Cloud Run carries the rest of the configuration — the service
account, the Cloud SQL instance, the secrets, the environment — over from the
current revision, so it is not duplicated in the Makefile and cannot drift from
it. Changing any of that is a change to `service.yaml` (or to the long form
above), which is where the service's configuration is meant to live. The target
checks the service exists first and tells you to create it if it does not, rather
than quietly creating a bare one with no database attached.

### 2.7 Verify

```bash
URL="$(gcloud run services describe educhan --region "$REGION" --format='value(status.url)')"
# Not /healthz: Cloud Run reserves some paths ending in "z" on run.app URLs.
curl -s "$URL/api/boards"  # the seeded boards: the database answers and migrations ran
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
deletes it, with its images, once its board's `archive_days` (7 by default) have
passed. Each run also rewrites the verification tables, so that every credential
issued before it carries the run's transaction id rather than its own; the root
README's [Transaction ids, and the nightly
rewrite](../README.md#transaction-ids-and-the-nightly-rewrite) says how closely
that still dates it. It is the same image run as `educhan -sweep`: one pass,
then it exits. It runs as a Cloud Run **Job**, started once a day by Cloud
Scheduler, because a Cloud Run service only has CPU while it answers a request,
so a timer inside it would fire whenever it happened to.

The Job runs as the service's account and needs only what its work takes: the
database, and `storage.objects.list`, `.get` and `.delete` on the bucket, all
already in the `roles/storage.objectAdmin` grant from section 2.3 (startup lists
the bucket to check it is usable, and the sweep checks that each file is there
before deleting it). It connects with the service's `DATABASE_URL`, so as the
service's database role, which ran the migrations and owns the tables the sweep
rewrites. It gets neither the moderation key nor
`EDUCHAN_VERIFY_SECRET`, which it has no use for (and the server refuses to start
with that secret set and no mail sender). The poster-ID salt is left out too, as
nothing the sweeper does touches a poster ID.

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

That first run also checks the database role. The sweep runs `VACUUM FULL` on
the `pg_statistic` catalog, which only the database's owner, or a member of the
owner's role that inherits its privileges, may vacuum, so a role without that
fails every sweep. A database made with `gcloud sql databases create`, as in
section 2.2, should belong to Cloud SQL's `cloudsqlsuperuser` role, and the
`educhan` user, like every user `gcloud sql users create` makes, is a member of
it, so the run should pass; but no sweep has run on Cloud SQL yet. If `--wait`
says the execution failed, this prints why:

```bash
gcloud logging read 'resource.type="cloud_run_job" AND resource.labels.job_name="educhan-sweep" AND (jsonPayload.msg="fatal" OR textPayload:"ERROR fatal")' \
  --freshness=1h --limit=1 --format='value(timestamp,jsonPayload.error,textPayload)'
```

An error saying `VACUUM FULL left pg_catalog.pg_statistic in its old file` is
the role: it names the query that says whether the role has the owner's
privileges, and the root README's [archive
sweeper](../README.md#the-archive-sweeper) says what the role needs. Any other
is most likely the media settings or the store (below). A stray argument, or a
setting the image refuses as soon as it reads it (a bad `EDUCHAN_LOG_FORMAT` or
`EDUCHAN_MEDIA_BACKEND`, say), is reported before the image starts logging JSON,
so it is printed as plain text at the end of the line; a `DATABASE_URL`, Cloud
SQL instance or bucket that does not work is reported as JSON, like any other
error. If it prints nothing, the execution ended without a word from the image,
killed for want of memory, say, or before it started: `gcloud run jobs
executions describe NAME --region "$REGION"`, with the failed execution's name
from `gcloud run jobs executions list --job educhan-sweep --region "$REGION"`,
says how.

Once verification is on, start the sweep by hand only after that night's
scheduled run has finished and no later than 04:00 UTC, which leaves it and its
one retry the half hour to spare before 05:00 that the schedule has (below); or
else when nobody has verified since the last 05:00 UTC and nobody will until it,
and its retry if it fails, has finished, since its rewrite comes last and takes
in every credential issued before it. Otherwise its last rewrite splits the day
it falls in: until the next night's sweep, and in the backups taken meanwhile,
its transaction id dates the credentials issued between 05:00 and that rewrite
to those hours.

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

03:17 UTC leaves the sweep and its one retry, up to 15 minutes each, time to
finish before 05:00 UTC, when the database's backup window opens and a
verification credential's day ends (section 2.2). The day the sweep runs in has
to end half an hour or more after the latest the sweep and its retry can still
be running, and two hours at most after the scheduled time, so with this retry
and timeout the schedule can move only between 03:00 and 04:00 UTC.
`TestCredentialDaysEndAfterTheSweep` reads `--schedule`, `--max-retries` and
`--task-timeout` from this guide and from `first-deploy.sh`, and fails outside
that. Beyond it, move `credentialDayStart` in `internal/app/verify.go` and the
backup window with the schedule, and ship the three together by re-running
`first-deploy.sh` with its schedule and its `05:00` backup times edited to
match, which moves the image, the Scheduler job and the backup window in one
run; `make cloud-deploy` moves only the images. A new `credentialDayStart`
splits the credential day it is deployed in (the root README's [Transaction ids,
and the nightly rewrite](../README.md#transaction-ids-and-the-nightly-rewrite)
says what that dates), so deploy it soon after the later of the old start and
the new, once the `verification issued` lines in the server's log show that
nobody has verified since the old start. The credentials issued from the deploy
until the next new start share their expiry with no others, so a later deploy
dates them to the fewer hours left in between, for as long as they are kept. A
start moved earlier can instead go out at any time between that day's new start
and its old one, whether or not anyone has verified, once that night's sweep on
the old schedule, and its retry if it fails, has finished, and with the deploy
over before the old start: the credentials issued before the deploy and those
after it then each span at least a day less the move. A deploy before that sweep
has finished would have it split the new day, or move the Scheduler job past it
and leave that night without one.

A sweep that could not remove a file exits non-zero, so the execution shows as
failed in the console (`gcloud run jobs executions list --job educhan-sweep
--region "$REGION"`). The file's key stays in `media_deletions` and the next run,
or the Job's own retry, tries it again, so a retry is always safe. In the log,
`blobs_missing` above zero means the media settings are wrong; `blob_failures`
means the store refused or was unreachable.

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
