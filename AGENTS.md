# AGENTS.md — recogn

Read this first. It tells you what the project is, how it's wired, what already
works, and the gotchas that will otherwise waste your time.

## What this is

`recogn` is a **face-recognition application** — CLI + REST API + minimal web UI,
written in **Go** with **CGO**. It maintains a database of known people (from a
`people/` photo folder) and, given any photo, **detects every face** in it and
identifies each one (name + confidence, or `unknown`). It runs on CPU.

Current state: **complete and working.** Inference runs **in-process via CGO +
the ONNX Runtime C API** (no Python, no subprocess). Enrolled 24 people / 66
photos, all tests pass, Docker image builds and runs (~508 MB).

## Architecture (important — don't reinvent this)

Go owns the **entire** pipeline. The only thing delegated to native code is
executing the two ONNX models, done in-process through `libonnxruntime` via a
small CGO wrapper. **There is no Python and no sidecar** (both were removed after
the CGO backend proved output-parity).

- **Go**: CLI, HTTP API, web UI, the face DB (bbolt + JSON interchange),
  enrollment orchestration, face alignment (Umeyama), matching (cosine), AND
  the detector's pre/post-processing (letterbox, CHW tensor build, SCRFD
  decode, NMS).
- **CGO** (`internal/onnxrt`): a thin shim over the ORT C API — open a session,
  run one float tensor, read output tensors. All `OrtApi` calls live in
  `onnxrt.c`; the Go side sees plain `[]float32`.

Models (insightface `buffalo_l` pack, downloaded into `models/`):
- **SCRFD** `det_10g.onnx` — multi-face detection, returns bbox + 5 landmarks.
  Raw output is 9 tensors (3 FPN strides × scores/bboxes/landmarks); decoded in
  `internal/engine/scrfd.go`.
- **ArcFace** `w600k_r50.onnx` — aligned 112×112 face → 512-d L2-normalized embedding.

Matching: **cosine similarity** of a query embedding vs. every enrolled embedding,
best score per person wins; `>= threshold` (default **0.45**) → identity, else
`unknown`. Alignment is a **Umeyama similarity transform** mapping the 5
landmarks onto the ArcFace 112×112 reference template (`engine.go`).

## Layout

```
main.go                  CLI entry: enroll | recognize | people | export | serve |
                         hash-password (+ password_unix/windows/other.go: hidden-input
                         terminal prompt for hash-password)
internal/
  config/config.go       paths, threshold; settings via RECOGN_* env vars
  onnxrt/onnxrt.{c,h,go} CGO binding to the ONNX Runtime C API
  engine/engine.go       Face/KnownPerson, pipeline, Umeyama align, cosine
  engine/preprocess.go   image → CHW float tensor (letterbox + normalise)
  engine/scrfd.go        SCRFD output decode + NMS (pure Go)
  engine/cgo_backend.go  inferencer impl using onnxrt (bounded-concurrency
                         gate, parallel per-face embedding)
  db/db.go               face DB: bbolt store (data/faces.db) + JSON
                         interchange import/export (embeddings.json), CRUD
  enroll/enroll.go       scan people/ → embeddings (incremental by content hash)
  netutil/netutil.go     serve listener: addr normalization + bind, and the
                         URLs a wildcard listener covers (per-interface)
  api/api.go             REST handlers; admin routes behind an auth
                         middleware; depends on an Engine INTERFACE (testable)
  auth/                  admin auth: sessions, login rate limit, passkeys
  web/web.go             serves the embedded UI bundle (go:embed all:dist)
  web/dist/              Vite build output (gitignored except .gitkeep;
                         rebuilt by `make ui`; missing → UI serves 503)
web/                     the front-end: Preact + TypeScript, built by Vite
  index.html             Vite entry (fonts, favicon, <div id="root">)
  vite.config.ts         preact preset, outDir ../internal/web/dist, dev proxy
  package.json           npm project (`npm --prefix web run dev|build`)
  public/logo.svg        static asset copied verbatim into the bundle
  src/
    main.tsx             entry: <ToastProvider><App/> into #root
    style.css            all styling (class names + ids unchanged from the
                         old static UI — the stylesheet is untouched)
    types.ts             API DTO types mirroring internal/api/api.go JSON
    api.ts               typed fetch wrappers for /api/* (422-on-enroll not
                         thrown); throws Error(j.error || fallback) otherwise
    overlay.ts           shared drawFaces canvas renderer (corner brackets)
    util.ts              fmtSize, initials, clipboard helpers
    modalStack.ts        Escape-stack registry (topmost modal closes first)
    toast.tsx            ToastProvider + useToast context
    components/
      App.tsx            state owner: health, people, threshold, modal open
                         state, editor-open flag, paste routing, Escape keydown
      Topbar.tsx         brand + status pill
      Stage.tsx          main stage: dropzone, preview, overlay, Edit /
                         Cancel-edits buttons (open the photo editor / revert
                         to the pristine original), face rows (matches +
                         "name this face" enroll form)
      EditModal.tsx      stage photo editor: rotate / crop / pad with
                         multi-step undo–redo; always re-derives from the
                         stage's pristine original and restores the last
                         session per file (reset on any new upload)
      PeoplePanel.tsx    enrolled-people list, search, threshold slider, rescan
      Modal.tsx          shared modal shell: backdrop, focus trap, focus
                         save/restore, Escape-stack membership, busy lock
      EnrollModal.tsx    enroll modal + pre-submit face-check chain
      PhotosModal.tsx    photos manager (grid + detail, add/delete/avatar/rename)
      FaceCheckModal.tsx enlarged face-check viewer (read-only, stacked)
      CompareModal.tsx   admin compare tool: two photo slots → face-to-face
                         cosine similarity (no identity DB; POST /api/compare)
      Avatar.tsx         thumbnail img with initials fallback
third_party/onnxruntime/ ORT C header + libonnxruntime.so (via `make ort`)
models/                  det_10g.onnx, w600k_r50.onnx  (gitignored; downloaded)
people/<Name>/*.jpg      the dataset — 24 people, 66 photos enrolled
data/faces.db            generated face DB (bbolt, gitignored)
data/embeddings.json     JSON interchange copy (gitignored): auto-imported when
                         the store is empty; written by `recogn export`
data/thumbs/             face thumbnail sidecars, one per person (gitignored)
data/passkeys.json       registered WebAuthn passkey credentials (gitignored;
                         corrupt file refuses startup)
Dockerfile, docker-compose.yml, .dockerignore
Makefile, README.md, scripts/dataset-test.sh
```

## The inference seam (if you touch internal/engine or internal/onnxrt)

`engine.Engine` talks to an unexported `inferencer` interface
(`detect` / `embedImage` / `embedBatch` / `ping` / `close`). The production
impl is `cgoInferencer`. `engine.New(detModel, embModel, thresh)` builds it
with the default concurrency; `engine.NewWithConcurrency(..., concurrency)`
bounds the parallel model Runs; `engine.NewWithInferencer` is for tests. Keep
inference behind this seam. `embedBatch` embeds N aligned faces by fanning
per-face Runs across the concurrency gate — **never** batch them into one
[N,3,112,112] Run: although ArcFace's input batch dim is dynamic, this
export's BatchNormalization normalises over the batch axis at Run time, so
batched outputs diverge from per-face ones (measured cosine 0.007–0.59;
pinned by `TestRunBatchEmbedder` / `TestCGOBatchedEmbedParity`).

**Preprocessing must match the models exactly** (these were validated
bit-for-bit against the reference Python/insightface pipeline):
- **Detector**: letterbox to 640×640 (aspect preserved, top-left, zero pad),
  then per-pixel `(v - 127.5) * (1/128)`, RGB, CHW. Coordinates scale back by
  `1/detScale` where `detScale = newHeight/origHeight`.
- **Embedder**: the aligned 112×112 image → `(v - 127.5) * (1/127.5)`, RGB, CHW.
- Go decodes to RGB; the models expect RGB, so **no channel swap** is needed.
- **SCRFD decode**: bbox and landmark predictions must be **multiplied by the
  stride** {8,16,32} before `distance2bbox`/`distance2kps` (a past bug was
  omitting this, yielding tiny boxes). Then NMS (IoU 0.4), threshold 0.5.

## ⚠️ Build/test gotchas (read before running go)

This environment **sandboxes the default Go caches** (`~/go`, `~/.cache/go-build`)
and they're not writable. All go commands must redirect caches into the workspace:

```sh
export GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gomodcache GOCACHE=$PWD/.gocache \
       GOFLAGS=-mod=mod GOPROXY=off CGO_ENABLED=1
```

**Prefer the `Makefile`** — it sets all of these plus the CGO include/lib flags:
`make build` / `make test` / `make test-race` / `make vet` / `make serve` /
`make ort` / `make ui` / `make dataset-test`.
`make build`/`serve`/`test` depend on `ui`, which needs **Node ≥ 20 + npm**
(see web/package.json); it skips `npm ci` when `web/node_modules` exists.

- **CGO is required** (`CGO_ENABLED=1`) and needs `gcc`. The ORT C lib+header
  must exist in `third_party/onnxruntime` — `make ort` fetches them (needs
  network once). The Go `#cgo` directive bakes an rpath of
  `$ORIGIN/third_party/onnxruntime/lib`, so the binary runs in place; keep
  `third_party/` next to the binary.
- `GOPROXY=off` works because the Go module deps (`golang.org/x/image`,
  `go.etcd.io/bbolt`) are in the local module cache; adding a new Go dep needs
  network (`GOPROXY=https://proxy.golang.org,direct`) + in-workspace `GOPATH`
  so the checksum db is writable.
- Flags are **per-subcommand** (`flag.NewFlagSet`) and Go stops flag parsing at
  the first positional arg: `./recogn recognize --json img.jpg` ✓, `... img.jpg --json` ✗.
- The repo is a plain **git** repo — history exists, no special workflow (no hooks/CI).

## Verified baseline (don't regress these)

- `go vet ./...` clean; `go test ./...` all pass (engine, db, api, onnxrt).
- Held-out accuracy (CGO, own enrollment + recognition): **11/11 = 100%**
  (held-out scores 0.47–0.81, wrong-person <0.20 → 0.45 threshold has margin).
- CGO↔Python parity (measured before the sidecar was removed): detection bbox
  IoU ≥ 0.995, score |Δ| ≤ 0.009 across all 38 photos; ORT numerics are
  bit-identical between CGO and Python for the same input tensor. (Embeddings
  differ ~0.98–0.995 cosine only because the old sidecar lossy-JPEG-encoded the
  aligned crop; the CGO path tensorizes directly and is *more* faithful.)
- Multi-face: a two-person composite returns both identities with separate boxes.
- No-face photo → `{count:0, faces:[]}` (HTTP 200), not an error.
- Docker: `docker compose up --build` serves UI+API, healthcheck `healthy`,
  DB persists in the mounted `./data` → `/data/db`. Image ~508 MB.

## Docker

`Dockerfile` is multi-stage: (1) fetch ORT C lib, (2) fetch models, (3) build
the web UI with Vite (`node:22-bookworm-slim`, `npm ci && npm run build`),
(4) build the CGO binary with `gcc` + ORT (rpath set to `/usr/lib/recogn`),
(5) slim `debian:bookworm-slim` runtime with `libonnxruntime` in
`/usr/lib/recogn` + `ldconfig`, `curl` for the healthcheck, non-root user,
`EXPOSE 8080`, `VOLUME /data/db`. `docker-compose.yml` mounts `./people`
writable at `/data/people` (API enrollments save uploaded photos back into
it), persists the DB via `./data` → `/data/db`, sets `RECOGN_THRESHOLD`,
healthcheck via `curl /api/health`. `.dockerignore` excludes `people/`,
`models/`, `data/`, `third_party/`, `python/`, caches, `web/node_modules/`,
and the host `internal/web/dist/` (the image builds its own bundle).

**Docker CLI commands need elevated sandbox permissions** (the daemon socket and
`~/.docker/buildx` state live outside the workspace) — retry with
`sandbox_permissions: danger-full-access` when a `docker`/`docker compose`
command hits a permission error.

## Common tasks

- **Add a person**: drop `people/<Name>/*.jpg`, then `./recogn enroll` (or
  `POST /api/enroll`, or the UI's "Rescan people folder"). Or upload at runtime:
  `POST /api/people/<name>/enroll` — successful uploads are also written to
  `people/<Name>/<sha1-12><ext>` (content-derived name; the DB photo path is
  the matching basename), so the dataset folder and DB stay in sync and a
  rescan recognizes them as already enrolled. The Docker `people` mount must
  therefore be writable (container uid 10001 needs write access on the host
  folder).
- **Tune strictness**: raise/lower `RECOGN_THRESHOLD` / `--threshold` /
  `POST /api/config`. Higher = fewer false positives, more `unknown`s.
- **Secure the admin surface** (optional): `RECOGN_ADMIN_PASSWORD_HASH` — an
  argon2id PHC hash generated by `recogn hash-password` (interactive prompt,
  `--password`, or stdin pipe) — enables password login (browser → HttpOnly
  `recogn_session` cookie; scripts → the cookie or `Authorization: Bearer
  <session token>`).
  WebAuthn passkeys are the second method
  (`RECOGN_WEBAUTHN_RPID` / `RECOGN_WEBAUTHN_ORIGIN` / `RECOGN_WEBAUTHN_RP_NAME`,
  default rp name "recogn"; login needs HTTPS or localhost; credentials persist
  in `data/passkeys.json`). `RECOGN_SESSION_TTL` (default 24h, sliding) bounds
  in-memory sessions — restarts log everyone out. With neither method
  configured, every route stays public and a warning is logged at startup.
- **Run the server**: `make serve` (or `./recogn serve --addr :8080` with the
  env exports above). Auto-enrolls if the DB is empty and `people/` exists.
- **Re-verify the dataset pipeline**: `RECOGN_DATASET=1 go test ./internal/engine/
  -run TestCGODatasetPipeline` (checks every enrolled photo detects a face and
  that intra-person embeddings cluster tighter than inter-person).

## Conventions to keep

- **Minimal Go deps** — stdlib + `x/image` + `bbolt` (DB) only. Image
  crop/resize/warp are hand-rolled in `engine.go`; reuse them.
- **Front-end is Preact + TypeScript, built by Vite** — sources live under
  `web/`, the build lands in `internal/web/dist/` and is embedded by
  `go:embed all:dist` (`internal/web/web.go`). Build with `make ui`
  (npm ci + `tsc --noEmit && vite build`; needs Node ≥ 20). During UI work run
  `npm --prefix web run dev` for the Vite dev server on :5173 — it proxies
  `/api` to the Go server on :8080, so no rebuild is needed for API-side
  checks. Architecture notes:
  - **State lives in `App`** (health, people, threshold, which modal is
    open); children refresh each other via `refreshAll()` callbacks — the
    old `on*Change` callback modules are gone.
  - **Modals stay mounted** and toggle `hidden` (the stylesheet keys off
    `.modal[hidden]`); the shared `Modal` shell owns the focus trap, focus
    save/restore, busy lock, and Escape-stack membership.
  - **Escape closes the topmost modal** via `modalStack.ts` (photos modal
    cancels its rename edit first); paste routing (`App`) routes clipboard
    images enroll → photos → stage, and never hijacks text pasted into
    inputs.
  - `class`/`id` names mirror the original DOM — keep them when editing
    components or `style.css` stops applying.
  - TestIndexServed extracts every local asset URL from the served
    index.html and GETs it — adding a local asset is covered automatically.
- The engine is safe for concurrent use. CGO inference is bounded, not
  serialised: a semaphore in `cgoInferencer` admits up to
  `RECOGN_CONCURRENCY` (default `min(NumCPU, 4)`) parallel model Runs — safe
  because each session uses intra-op threads = 1 (Runs execute on the calling
  goroutine) and ORT's CPU execution provider is thread-safe for concurrent
  Runs on one session, enforced by `TestConcurrentRunParity` (run
  `make test-race` after touching anything parallel). Only the Run holds a
  slot; Go-side pre/post-processing stays outside the gate. `close()` drains
  every slot before closing the sessions.
- **DB storage is bbolt** (`data/faces.db`, dep `go.etcd.io/bbolt`) with an
  in-memory mirror behind the DB RWMutex — reads never touch the file. Every
  mutating method commits a targeted `bolt.Update` FIRST, then updates the
  cache, so a failed write leaves memory and disk consistent. The bbolt file
  holds an exclusive lock: a second `recogn` process on the same DB fails at
  `Open` (tests must `Close()` before reopening the same path — same-process
  double-open deadlocks). **JSON interchange** (`embeddings.json` next to the
  DB): imported once per empty store (parse failure fails `Open` loudly;
  the file is never renamed, so deleting `faces.db` re-imports it — the JSON
  can be stale relative to the store, hence the stderr hint) and written by
  `db.Export`/`recogn export` (people name-sorted for byte-stable diffs).
  Photo records on disk: `uvarint hashLen + hash + uvarint dim + dim×float32
  LE` (`encodePhoto`/`decodePhoto`; decode rejects size mismatches).
- Embeddings are stripped from API/CLI JSON output (`Face.Embedding` is `json:"-"`
  or nil-ed) — don't leak 512-float arrays to clients.
- **API request bodies are capped**: 32 MiB (`maxUpload`) on `/api/recognize`
  and the enroll endpoints (multipart images; `/api/compare` caps the whole
  two-photo body the same way), 1 MiB on JSON bodies
  (`POST /api/config`, `POST /api/people/{name}/rename`, …). Wrap new
  handlers' bodies in `http.MaxBytesReader`/`io.LimitReader` the same way.
- **Admin auth** (`internal/auth`): `api.New` returns `(*Server, error)` and
  builds the auth service; admin routes (enroll, people/photo mutations,
  `POST /api/config`, face comparison, full-res photo serving) sit behind an
  auth middleware.
  Public: `/api/recognize`, `GET /api/people`, `/api/thumbs/{id}.jpg`,
  `/api/health`, and the auth endpoints. Passkey credentials persist in
  `data/passkeys.json` (corrupt file refuses startup); sessions are in-memory.
- Enrollment stores **one embedding per photo** (largest face) and matches
  per-person by best similarity. Photos with no detectable face are skipped with
  a warning, never stored. DB photo paths are **basenames relative to the
  person's folder** (folder scans and API uploads both derive the same name);
  API uploads additionally persist the image bytes into `people/<Name>/`.
  `Recognize` also fills `Face.Matches`: every person above the threshold,
  ranked best-first (one entry per person, per-person best) — the UI lists
  them to help spot duplicate people; the single best match still drives
  `Name`/`PersonID`/`Confidence`.
- **Face thumbnails** are a DB sidecar: `thumbs/<personID>.jpg` next to
  the DB file (`data/thumbs/`; see `db.SetThumbnail`/`ThumbFile`, crop via
  `engine.FaceThumb`). Auto-generated at first enrollment only (enrollment
  callers skip when one exists); the API's `POST /api/people/{name}/thumbnail`
  re-selects the source photo, which **overwrites** the sidecar and records it
  as `Person.ThumbSrc`. Always **best-effort**: thumbnail failures must never
  fail an enrollment. Missing thumbnails are backfilled by rescans without
  re-embedding. Thumbnail URLs carry a `?v=` cache-buster derived from
  `ThumbSrc`.
- **Per-photo management** (photos-manager modal): `GET
  /api/people/{name}/photos/{path}/detect` recognizes an enrolled photo's
  faces for the UI's quality check (embeddings stripped, same rule as
  `/api/recognize`); `DELETE /api/people/{name}/photos/{path}` removes the DB
  entry + the file from `people/<Name>/`, and when the deleted photo was
  `ThumbSrc`, regenerates the thumbnail from the first remaining photo that
  still detects a face — or clears it (`db.ClearThumbnail`) when none does.
  Deleting a person's last photo leaves the person enrolled with 0 photos.
- **Renaming a person** (Rename button in the photos-manager modal →
  `POST /api/people/{name}/rename`, body `{"name": ...}`): one transaction
  across disk and DB — `os.Rename` of `people/<Old>/` → `people/<New>/` (a
  409 refuses to clobber a different existing folder; `os.SameFile` lets
  case-only renames work on case-insensitive filesystems), then
  `db.RenamePerson` re-derives the ID from the name (`db.newID`), renames the
  thumbnail sidecar to `<newID>.jpg` and persists. A DB failure after the
  folder move rolls the folder back. Photo paths and `ThumbSrc` are
  folder-relative basenames, so they need no rewrite; the engine identity set
  reloads via `s.reload()` afterwards.
- **Deleting a person** (Remove button in the people list →
  `DELETE /api/people/{name}`): removes the DB record and the thumbnail
  sidecar, then deletes `people/<Name>/` best-effort — otherwise the next
  rescan would silently re-enroll the person. The response reports
  `"folder_removed"` (false when the folder couldn't be removed).
- **onnxrt memory discipline**: every `OrtValue`/buffer allocated in the C shim
  is freed (tensor data via `ort_free`, sessions via `ort_close`). If you extend
  the shim, keep the ownership rules in `onnxrt.h` accurate and re-run the
  repeated-run test (`internal/onnxrt/onnxrt_test.go`) to catch leaks.

## Known limitations / possible next tasks

- **CPU-only** inference (~0.2–0.6 s/photo; parallel Runs scale across cores
  via `RECOGN_CONCURRENCY`). GPU = use the ORT GPU build of `libonnxruntime`
  + enable a CUDA execution provider in the C shim + a CUDA base image.
- Detection cannot be batched across images: `det_10g.onnx` declares a fixed
  batch dim of 1. ArcFace's input batch dim is dynamic, but its export still
  normalises over the batch axis (fixed {1,512} output shape), so faces must
  never share a Run — `embedBatch` fans per-face Runs instead. Cross-image
  throughput comes from concurrent Runs, not batched tensors.
- CGO means no static/cross-compiled binary; the binary links glibc +
  libonnxruntime. Builds are for the host (linux/amd64) unless you set up a
  cross C toolchain.
- No TLS in-app. Admin auth exists (password argon2id hash
  `RECOGN_ADMIN_PASSWORD_HASH` + WebAuthn passkeys) but is opt-in: with neither
  configured, every endpoint stays public (a warning is logged at startup).
  Expose remotely only behind a TLS reverse proxy.
- No screenshot-based UI test (no headless browser in this env).
