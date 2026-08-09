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
at startup; the seeded boards are `placements`, `research`, and `general`.
Attachments go to `./data/uploads`, because `EDUCHAN_MEDIA_BACKEND` defaults to
`local`.

```bash
curl -s localhost:8080/healthz     # {"status":"ok"}
curl -s localhost:8080/api/boards

# Posting, with an attachment
curl -s -X POST localhost:8080/api/boards/general/threads \
  -F subject="lab setup" -F body="finally works" -F image=@photo.jpg

# Moderation, using the key the server was started with
curl -s -X POST localhost:8080/api/boards/general/threads/1/sticky \
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
  storage.googleapis.com

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
so the same person shows a different ID depending on which container answered.

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
an older commit rather than a rebuild:

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
# fetch it — then confirm the object is really in Cloud Storage.
curl -s -X POST "$URL/api/boards/general/threads" \
  -F subject="storage check" -F body="does this survive" -F image=@photo.jpg
curl -s "$URL/api/boards/general/catalog"     # threads[].attachment.url
curl -sI "$URL/media/original/general/2026/08/<name>.jpg"   # 200, image/jpeg
gcloud storage ls -r "gs://${MEDIA_BUCKET}/media/originals/**"
```

If the bucket is missing or the grant did not land, the revision fails to start
and the log says so by name — `cloud storage bucket "…" is not usable` — rather
than serving a site whose images all 404.

Migrations are applied at startup inside a Postgres advisory lock, so rolling out
a revision while the old one still serves traffic is safe: whichever container
gets there first applies them and the rest wait.

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

Each container keeps its own `pgxpool`, sized to at least four connections. Total
usage is that pool times the number of running instances, so keep `maxScale`
within what the instance tier allows, or bound it explicitly with
`&pool_max_conns=4` in the connection string.

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

`EDUCHAN_TRUST_PROXY` is the one worth getting right. It is a hop count, and a
value higher than the real chain lets a client pick its own rate-limit bucket and
poster ID by sending an `X-Forwarded-For` of its own. Cloud Run is one hop; put a
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
name, so `originals/general/2026/08/{random}.jpg` addresses a file on disk and an
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
