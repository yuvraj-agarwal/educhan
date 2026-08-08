# Deploying educhan

educhan builds to a single static binary with the web UI and the SQL migrations
embedded, so a deployment is one container plus a PostgreSQL database. This
directory holds the Google Cloud Run definition; the `Dockerfile` and
`docker-compose.yml` in the repository root cover building and running it.

| File | Purpose |
| --- | --- |
| `Dockerfile` | Multi-stage build; distroless runtime, non-root, ~21 MB |
| `docker-compose.yml` | Local stack: educhan + PostgreSQL + volumes |
| `deploy/cloudrun/service.yaml` | Declarative Cloud Run service, Cloud SQL attached |

---

## 1. Locally with Docker Compose

The only prerequisite is Docker with the Compose plugin.

```bash
docker compose up --build      # http://localhost:8080
```

That builds the image, starts PostgreSQL 16, waits for it to pass `pg_isready`,
and starts educhan. Migrations are applied on first boot; the seeded boards are
`placements`, `research`, and `general`.

```bash
curl -s localhost:8080/healthz     # {"status":"ok"}
curl -s localhost:8080/api/boards

# Posting, with an attachment
curl -s -X POST localhost:8080/api/boards/general/threads \
  -F subject="lab setup" -F body="finally works" -F image=@photo.jpg

# Moderation, using the compose default key
curl -s -X POST localhost:8080/api/boards/general/threads/1/sticky \
  -H 'Authorization: Bearer dev-mod-key'
```

```bash
docker compose logs -f app     # structured JSON logs
docker compose down            # stop, keeping the database and uploads
docker compose down -v         # stop and discard both volumes
```

Two named volumes hold the state: `pgdata` for PostgreSQL and `uploads` for
originals and thumbnails, both of which survive `down` and a rebuild. Override
any setting by exporting it or putting it in a `.env` beside the compose file —
`EDUCHAN_MOD_KEY`, the cooldowns, and `EDUCHAN_POSTER_ID_SALT` all read from
there. PostgreSQL is published on `127.0.0.1:5433` so it never collides with a
cluster already using 5432 on the host:

```bash
psql postgres://educhan:educhan@localhost:5433/educhan
```

This stack is for development and testing. The compose file ships a fixed
poster-ID salt and a `dev-mod-key` so restarts are predictable; neither is
suitable for anything reachable.

---

## 2. Cloud Run

### 2.1 One-time project setup

```bash
export PROJECT_ID=your-project
export REGION=asia-south1

gcloud config set project "$PROJECT_ID"
gcloud services enable \
  run.googleapis.com \
  artifactregistry.googleapis.com \
  sqladmin.googleapis.com \
  secretmanager.googleapis.com \
  cloudbuild.googleapis.com

# Somewhere to push the image
gcloud artifacts repositories create educhan \
  --repository-format=docker --location="$REGION"

# The identity the service runs as — not the default compute account
gcloud iam service-accounts create educhan-run --display-name="educhan Cloud Run"
export RUN_SA="educhan-run@${PROJECT_ID}.iam.gserviceaccount.com"
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${RUN_SA}" --role=roles/cloudsql.client
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

### 2.3 Secrets

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

### 2.4 Build and push

Either build locally:

```bash
gcloud auth configure-docker "${REGION}-docker.pkg.dev"
export IMAGE_TAG="$(git rev-parse --short HEAD)"
export IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/educhan/educhan:${IMAGE_TAG}"

docker build -t "$IMAGE" .
docker push "$IMAGE"
```

or let Cloud Build do it, which needs no local Docker:

```bash
export IMAGE_TAG="$(git rev-parse --short HEAD)"
export IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/educhan/educhan:${IMAGE_TAG}"
gcloud builds submit --tag "$IMAGE"
```

### 2.5 Deploy

Either apply the service definition, which keeps the whole configuration in
version control:

```bash
envsubst < deploy/cloudrun/service.yaml > /tmp/educhan-service.yaml
gcloud run services replace /tmp/educhan-service.yaml --region "$REGION"

# services replace does not set the invoker policy; make it public once
gcloud run services add-iam-policy-binding educhan --region "$REGION" \
  --member=allUsers --role=roles/run.invoker
```

(`envsubst` substitutes `PROJECT_ID`, `REGION`, `CLOUDSQL_INSTANCE`, and
`IMAGE_TAG` from the environment. Without gettext installed, edit the four
placeholders by hand.)

Or do it in one command:

```bash
gcloud run deploy educhan \
  --image "$IMAGE" \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CLOUDSQL_INSTANCE" \
  --set-secrets "DATABASE_URL=educhan-database-url:latest,EDUCHAN_POSTER_ID_SALT=educhan-poster-id-salt:latest,EDUCHAN_MOD_KEY=educhan-mod-key:latest" \
  --set-env-vars "EDUCHAN_TRUST_PROXY=1,EDUCHAN_UPLOAD_DIR=/tmp/uploads,EDUCHAN_LOG_FORMAT=json" \
  --cpu 1 --memory 512Mi --concurrency 80 --max-instances 4 \
  --allow-unauthenticated
```

### 2.6 Verify

```bash
URL="$(gcloud run services describe educhan --region "$REGION" --format='value(status.url)')"
curl -s "$URL/healthz"     # {"status":"ok"} — this also pings PostgreSQL
curl -s "$URL/api/boards"  # the seeded boards, so migrations ran
open "$URL"                # the UI
```

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
ones whose value differs in a container or on Cloud Run:

| Variable | Local (compose) | Cloud Run | Notes |
| --- | --- | --- | --- |
| `PORT` | unset | injected | Cloud Run's contract. Used as the listen port when `EDUCHAN_ADDR` is unset |
| `EDUCHAN_ADDR` | `:8080` | leave unset | Setting it overrides `PORT`, which breaks Cloud Run's health checking |
| `DATABASE_URL` | `postgres://educhan:educhan@db:5432/educhan?sslmode=disable` | from Secret Manager | Unix-socket form on Cloud Run; see section 3 |
| `EDUCHAN_UPLOAD_DIR` | `/data/uploads` (volume) | `/tmp/uploads` | `/tmp` is the writable path on Cloud Run, and it is in-memory |
| `EDUCHAN_POSTER_ID_SALT` | fixed dev value | from Secret Manager | Must be stable across instances and revisions |
| `EDUCHAN_MOD_KEY` | `dev-mod-key` | from Secret Manager | Unset means every moderation endpoint answers `403` |
| `EDUCHAN_VERIFY_SECRET` | `dev-verify-secret` | **unset** | Institutional verification. Unset switches it off; rotating it revokes every badge. Left off on Cloud Run until a mail `Sender` exists — see below |
| `EDUCHAN_TRUST_PROXY` | `0` | `1` | Cloud Run's front end appends exactly one hop to `X-Forwarded-For` |
| `EDUCHAN_LOG_FORMAT` | `json` | `json` | JSON is what Cloud Logging parses into structured fields |
| `EDUCHAN_THREAD_COOLDOWN` / `_POST_` / `_UPLOAD_` | `30` / `5` / `15` | same | Seconds; `0` disables one |
| `EDUCHAN_MAX_UPLOAD_BYTES` | `8388608` | `8388608` | Cloud Run's own request limit is 32 MiB |

`EDUCHAN_TRUST_PROXY` is the one worth getting right. It is a hop count, and a
value higher than the real chain lets a client pick its own rate-limit bucket and
poster ID by sending an `X-Forwarded-For` of its own. Cloud Run is one hop; put a
load balancer or CDN in front and it becomes two.

---

## 5. Limitations

**Uploads do not survive an instance.** `EDUCHAN_UPLOAD_DIR` points at `/tmp`,
which on Cloud Run is per-instance and in-memory: it is discarded when the
container stops, it counts against the memory limit, and an image uploaded to one
instance answers `404` from another. Options, in order of effort:

- Set `--max-instances 1` and accept that uploads are lost on every scale-to-zero
  or new revision. Fine for a demo, not for anything real.
- Mount a Cloud Storage bucket with Cloud Run's volume mounts, which puts a FUSE
  filesystem behind the same `EDUCHAN_UPLOAD_DIR` — no code change, but FUSE
  latency and semantics apply.
- Implement a Cloud Storage `media.Store`, which is what the interface is there
  for (section 6). This is the intended answer.

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

**Not covered here:** CI/CD, custom domains and certificates, Cloud CDN, Cloud
Armor, and backups beyond Cloud SQL's automatic ones.

---

## 6. Preparing for Google Cloud Storage

Nothing in the handlers or the SQL touches the filesystem. `internal/media.Store`
is the only route to blob storage:

```go
type Store interface {
	Put(ctx context.Context, key, contentType string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadSeekCloser, Object, error)
	Delete(ctx context.Context, key string) error
}
```

`LocalStore` in `internal/media/local.go` is the implementation used today, and
`app.New` is the single place it is constructed. A `GCSStore` alongside it —
selected by, say, an `EDUCHAN_MEDIA_BUCKET` being set — is the whole change:
keys are already slash-separated, relative, and validated by `CleanKey`, which is
exactly an object name. Serving can stay as it is, with `/media/…` streaming
through the process, or move to signed URLs later.
