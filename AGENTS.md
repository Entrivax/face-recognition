# AGENTS.md — recogn

Read this first. It tells you what the project is, how it's wired, what already
works, and the gotchas that will otherwise waste your time.

## What this is

`recogn` is a **face-recognition application** — CLI + REST API + minimal web UI,
written in **Go**. It maintains a database of known people (from a `people/`
photo folder) and, given any photo, **detects every face** in it and identifies
each one (name + confidence, or `unknown`). It runs entirely on CPU.

Current state: **complete and working.** Enrolled 11 people / 38 photos, all
tests pass, Docker image builds and runs.

## Architecture (important — don't reinvent this)

Pure-Go face recognition is impractical, so the design is a **Go core + a Python
ONNX inference sidecar**. This split is deliberate; keep it.

- **Go** owns everything user-facing and stateful: CLI, HTTP API, web UI, the
  face database, enrollment orchestration, face alignment, and matching.
- **Python** (`python/infer.py`) does only tensor inference and speaks **NDJSON
  over stdin/stdout**. Go spawns it once as a persistent process and
  mutex-serializes requests (CPU inference is single-stream). On a transport
  error Go kills and restarts it transparently.

Models (insightface `buffalo_l` pack, downloaded into `models/`):
- **SCRFD** `det_10g.onnx` — multi-face detection, returns bbox + 5 landmarks.
- **ArcFace** `w600k_r50.onnx` — aligned 112×112 face → 512-d L2-normalized embedding.

Matching: **cosine similarity** of a query embedding vs. every enrolled embedding,
best score per person wins; `>= threshold` (default **0.45**) → identity, else
`unknown`. Alignment is a **Umeyama similarity transform** (in Go) mapping the 5
landmarks onto the ArcFace 112×112 reference template.

## Layout

```
main.go                  CLI entry: enroll | recognize | people | serve
internal/
  config/config.go       paths, threshold; all settings via RECOGN_* env vars
  engine/engine.go       Face/KnownPerson types, pipeline, Umeyama align, cosine
  engine/sidecar.go      spawn/manage python sidecar, NDJSON protocol, restart
  db/db.go               JSON face DB (data/embeddings.json), CRUD, atomic saves
  enroll/enroll.go       scan people/ → embeddings (incremental by content hash)
  api/api.go             REST handlers; depends on an Engine INTERFACE (testable)
  web/web.go + static/   embedded single-page UI (go:embed, no build step)
python/infer.py          ONNX sidecar (SCRFD + ArcFace), NDJSON loop
python/requirements.txt  pinned: onnxruntime, opencv-python-headless, numpy
models/                  det_10g.onnx, w600k_r50.onnx  (gitignored; downloaded)
people/<Name>/*.jpg      the dataset — 11 people, 38 photos
data/embeddings.json     generated face DB (gitignored)
Dockerfile, docker-compose.yml, .dockerignore
Makefile, README.md
```

## Sidecar protocol (if you touch engine/sidecar.go or python/infer.py)

One JSON object per line, both directions. `"id"` is echoed back.

- `{"id":N,"cmd":"ping"}` → `{"id":N,"status":"ok"}`
- `{"id":N,"cmd":"detect","image":"<b64>"}` →
  `{"id":N,"faces":[{"bbox":[x,y,w,h],"score":f,"landmarks":[[x,y]×5]}]}`
- `{"id":N,"cmd":"embed","image":"<b64 112×112 aligned>"}` → `{"id":N,"embedding":[512 floats]}`
- Error → `{"id":N,"error":"..."}`

The sidecar reads model paths from env `RECOGN_DET_MODEL` / `RECOGN_EMB_MODEL`
(absolute paths set by `engine.New`). **stdout is protocol-only** — diagnostics
go to stderr (Go prefixes them `[sidecar]`). Never print to stdout from Python.

## REST API

| Method | Endpoint | Purpose |
|--------|----------|---------|
| POST | `/api/recognize` | multipart `image` → `{count, faces:[{bbox,name,person_id,confidence,score,landmarks}]}` |
| GET | `/api/people` | list people + photo counts |
| GET/DELETE | `/api/people/{name}` | get photos / remove person |
| POST | `/api/people/{name}/enroll` | add photo(s) (field `images`) to new/existing person |
| POST | `/api/enroll?force=true` | rescan `people/` (incremental unless force) |
| GET/POST | `/api/config` | read/set match threshold |
| GET | `/api/health` | status, people count, threshold |
| GET | `/` | web UI (+ `/app.js`, `/style.css`) |

`api.Server` takes the engine via the `api.Engine` **interface**, so handler
tests use a stub — see `internal/api/api_test.go`. Keep it that way.

## ⚠️ Build/test gotchas (read before running go)

This environment **sandboxes the default Go caches** (`~/go`, `~/.cache/go-build`)
and they're not writable. All go commands must redirect caches into the workspace:

```sh
export GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gomodcache GOCACHE=$PWD/.gocache \
       GOFLAGS=-mod=mod GOPROXY=off
```

The **`Makefile` already sets these** — prefer `make build` / `make test` /
`make vet` / `make serve`. `GOPROXY=off` works because the single dependency
(`golang.org/x/image v0.45.0`) is vendored in the local module cache; if you add
a new dependency you'll need network (`GOPROXY=https://proxy.golang.org,direct`)
plus `GOPATH` pointed in-workspace so the checksum db is writable.

- Flags are **per-subcommand** (`flag.NewFlagSet`) and Go stops flag parsing at
  the first positional arg: `./recogn recognize --json img.jpg` ✓, `... img.jpg --json` ✗.
- Not a git repo yet — no VCS history. `git init` if you want it.

## Verified baseline (don't regress these)

- `go vet ./...` clean; `go test ./...` all pass (engine, db, api).
- Held-out accuracy on this dataset: **11/11 = 100%** (right-person scores
  0.47–0.80, wrong-person <0.20 → 0.45 threshold has good margin).
- Multi-face: a two-person composite returns both identities with separate boxes.
- No-face photo → `{count:0, faces:[]}` (HTTP 200), not an error.
- Docker: `docker compose up --build` auto-enrolls on first run, serves UI+API,
  healthcheck goes `healthy`, DB persists in the `recogn-db` volume across restarts.
  Image is ~1.11 GB.

## Docker

`Dockerfile` is multi-stage (golang build → model download → python slim
runtime), CGO off, non-root user, models baked in, `EXPOSE 8080`, `VOLUME
/data/db`, `ENTRYPOINT ["recogn"]`, `CMD ["serve"]`. `docker-compose.yml` mounts
`./people` read-only at `/data/people`, persists the DB in mounted path `./data` at `/data/db`,
sets `RECOGN_THRESHOLD`, and adds a `/api/health` healthcheck (40s start_period
for first-run enrollment). `.dockerignore` excludes `people/`, `models/`,
`data/`, caches.

**Docker CLI commands need elevated sandbox permissions** (the daemon socket and
`~/.docker/buildx` state live outside the workspace) — retry with
`sandbox_permissions: danger-full-access` when a `docker`/`docker compose`
command hits a permission error.

## Common tasks

- **Add a person**: drop `people/<Name>/*.jpg`, then `./recogn enroll` (or
  `POST /api/enroll`, or the UI's "Rescan people folder"). Or upload at runtime:
  `POST /api/people/<name>/enroll`.
- **Tune strictness**: raise/lower `RECOGN_THRESHOLD` / `--threshold` /
  `POST /api/config`. Higher = fewer false positives, more `unknown`s.
- **Run the server**: `make serve` (or `./recogn serve --addr :8080` with the
  env exports above). Auto-enrolls if the DB is empty and `people/` exists.

## Conventions to keep

- **Zero new Go dependencies** unless truly necessary — the module is stdlib +
  `x/image` only. Image crop/resize/warp are hand-rolled in `engine.go`; reuse
  them.
- The engine is safe for concurrent use; the sidecar is the serialization point.
- DB writes are atomic (temp file + rename) and mutex-guarded — preserve this.
- Embeddings are stripped from API/CLI JSON output (`Face.Embedding` is `json:"-"`
  or nil-ed) — don't leak 512-float arrays to clients.
- Enrollment stores **one embedding per photo** (largest face) and matches
  per-person by best similarity. Photos with no detectable face are skipped with
  a warning, never stored.

## Known limitations / possible next tasks

- **CPU-only** inference (~0.2–0.6 s/photo). GPU would mean swapping
  `onnxruntime` → `onnxruntime-gpu` and a CUDA base image.
- Sidecar is a single serialized stream — high-throughput batch work would need
  a pool of sidecars or batching in `infer.py`.
- No auth/TLS on the API — it's a local tool; add middleware if you expose it.
- No screenshot-based UI test was done (no headless browser in this env).
- Alignment is redone per request; a face-embedding cache by content hash could
  speed repeat queries.
