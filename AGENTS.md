# AGENTS.md — recogn

Read this first. It tells you what the project is, how it's wired, what already
works, and the gotchas that will otherwise waste your time.

## What this is

`recogn` is a **face-recognition application** — CLI + REST API + minimal web UI,
written in **Go** with **CGO**. It maintains databases of known people —
**repositories**, each a `people/<repo-uuid>/<Person>/` photo folder — and, given
any photo, **detects every face** in it and identifies each one (name +
confidence, or `unknown`). It runs on CPU.

Current state: **complete and working, multi-user.** Inference runs **in-process
via CGO + the ONNX Runtime C API** (no Python, no subprocess). The server always
requires login: a first-run setup key creates the admin, users get per-repo
reader/writer roles, and API keys / share links cover scripts and anonymous
readers. The sample dataset (24 people / 66 photos) lives in a repository; all
tests pass, Docker image builds and runs (~157 MB; models are a
mounted/downloaded volume, not baked in).

## Architecture (important — don't reinvent this)

Go owns the **entire** pipeline. The only thing delegated to native code is
executing the two ONNX models, done in-process through `libonnxruntime` via a
small CGO wrapper. **There is no Python and no sidecar** (both were removed after
the CGO backend proved output-parity).

- **Go**: CLI, HTTP API, web UI, the face DB (bbolt + JSON interchange), the
  user DB, enrollment orchestration, face alignment (Umeyama), matching
  (cosine), AND the detector's pre/post-processing (letterbox, CHW tensor
  build, SCRFD decode, NMS).
- **CGO** (`internal/onnxrt`): a thin shim over the ORT C API — open a session,
  run one float tensor, read output tensors. All `OrtApi` calls live in
  `onnxrt.c`; the Go side sees plain `[]float32`.

Models (insightface `buffalo_l` pack, downloaded into `models/`):
- **SCRFD** `det_10g.onnx` — multi-face detection, returns bbox + 5 landmarks.
  Raw output is 9 tensors (3 FPN strides × scores/bboxes/landmarks); decoded in
  `internal/engine/scrfd.go`.
- **ArcFace** `w600k_r50.onnx` — aligned 112×112 face → 512-d L2-normalized embedding.

Matching: **cosine similarity** of a query embedding vs. every enrolled embedding
**of the selected repository**, best score per person wins; `>= threshold`
(default **0.45**) → identity, else `unknown`. Alignment is a **Umeyama
similarity transform** mapping the 5 landmarks onto the ArcFace 112×112
reference template (`engine.go`).

## Layout

```
main.go                  CLI entry: enroll | recognize | people | repo (import) |
                         export | serve
internal/
  config/config.go       paths, threshold; settings via RECOGN_* env vars
  onnxrt/onnxrt.{c,h,go} CGO binding to the ONNX Runtime C API
  engine/engine.go       Face/KnownPerson, pipeline, Umeyama align, cosine
  engine/preprocess.go   image → CHW float tensor (letterbox + normalise)
  engine/scrfd.go        SCRFD output decode + NMS (pure Go)
  engine/cgo_backend.go  inferencer impl using onnxrt (bounded-concurrency
                         gate, parallel per-face embedding)
  db/db.go               face DB: bbolt store (data/faces.db, schema v2 with a
                         repos bucket) + JSON interchange import/export
                         (embeddings.json), CRUD
  users/                 principals: user accounts, per-repo grants, hashed
                         API keys, share tokens, in-memory sessions and the
                         first-run setup key (data/users.db)
  enroll/enroll.go       scan a repository's person folders → embeddings
                         (incremental by content hash)
  netutil/netutil.go     serve listener: addr normalization + bind, and the
                         URLs a wildcard listener covers (per-interface)
  api/api.go             REST handlers; per-repo reader/writer role checks,
                         admin routes behind an auth middleware; depends on an
                         Engine INTERFACE (testable)
  auth/                  HTTP auth layer: session-cookie issuing/validation,
                         login rate limiting, client-IP keying (principals
                         resolve through internal/users)
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
    markdown.ts          marked + DOMPurify sanitizer for the details modal's
                         markdown description (links forced to noopener)
    birthdate.ts         birthdate text template (YYYY-MM-DD / YYYY--DD /
                         YYYY-MM / YYYY / -MM-DD / -MM-), parse/serialize and
                         the age (≈ when partial) + days-to-birthday math
                         (UTC, Feb 29 → Mar 1 on non-leap years)
    util.ts              fmtSize, initials, clipboard helpers
    modalStack.ts        Escape-stack registry (topmost modal closes first)
    toast.tsx            ToastProvider + useToast context
    components/
      App.tsx            state owner: session, repos, people, threshold,
                         modal open state, editor-open flag, paste routing,
                         Escape keydown
      Topbar.tsx         brand + status pill
      Stage.tsx          main stage: dropzone, preview, overlay, Edit /
                         Cancel-edits buttons (open the photo editor / revert
                         to the pristine original), face rows (matches +
                         "name this face" enroll form)
      EditModal.tsx      stage photo editor: rotate / crop / pad with
                         multi-step undo–redo; always re-derives from the
                         stage's pristine original and restores the last
                         session per file (reset on any new upload)
      PeoplePanel.tsx    enrolled-people list, search (names + aliases, with
                         aka hints), threshold slider, rescan
      Modal.tsx          shared modal shell: backdrop, focus trap, focus
                         save/restore, Escape-stack membership, busy lock
      EnrollModal.tsx    enroll modal + pre-submit face-check chain
      PersonModal.tsx    person details modal (reader-visible): aliases,
                         partial birthdate, URLs, markdown description;
                         writers edit inline and reach the photos manager
                         from here
      PhotosModal.tsx    photos manager (grid + detail, add/delete/avatar/rename;
                         opens from the person details modal)
      FaceCheckModal.tsx enlarged face-check viewer (read-only, stacked)
      CompareModal.tsx   compare tool: two photo slots → face-to-face cosine
                         similarity (no identity DB; POST /api/compare)
      Avatar.tsx         thumbnail img with initials fallback
third_party/onnxruntime/ ORT C header + libonnxruntime.so (via `make ort`)
models/                  det_10g.onnx, w600k_r50.onnx  (gitignored; downloaded)
people/<repo-uuid>/<Name>/*.jpg   the dataset — one folder per repository
                         (24 people / 66 photos in the sample); legacy flat
                         people/<Name>/ folders until `recogn repo import`
data/faces.db            generated face DB (bbolt, gitignored) — schema v2:
                         a repos bucket + Person.RepoID; person IDs are
                         <repoUUID>-<slug>
data/users.db            users, per-repo grants, hashed API keys (bbolt,
                         gitignored)
data/embeddings.json     JSON interchange copy (gitignored): auto-imported when
                         the store is empty; written by `recogn export`
data/thumbs/             face thumbnail sidecars, one per person (gitignored)
data/passkeys.json       OBSOLETE — WebAuthn was removed; an inert leftover
                         file operators may delete
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

- **CGO is required** (`CGO_ENABLED=1`) and needs `gcc`. The ORT C header must
  exist in `third_party/onnxruntime` at build/test time — `make ort` fetches
  it (needs network once). The shared library is **not linked at build time**:
  the main package embeds it via `go:embed` (`ort_embed_*.go`; the pattern is
  relative to the repo root) and the first session extracts it to a per-version
  cache dir and dlopens it (`internal/onnxrt/embed.go`; fallbacks:
  `RECOGN_ORT_LIBRARY`, the system loader path, the repo `third_party/`
  layout). `third_party/` is only needed where the **build** runs, not next
  to the deployed binary.
- `GOPROXY=off` works because the Go module deps (`golang.org/x/image`,
  `go.etcd.io/bbolt`) are in the local module cache; adding a new Go dep needs
  network (`GOPROXY=https://proxy.golang.org,direct`) + in-workspace `GOPATH`
  so the checksum db is writable.
- Flags are **per-subcommand** (`flag.NewFlagSet`) and Go stops flag parsing at
  the first positional arg: `./recogn recognize --repo X --json img.jpg` ✓,
  `... img.jpg --json` ✗.
- The repo is a plain **git** repo — history exists, no special workflow (no hooks/CI).

## Verified baseline (don't regress these)

- `go vet ./...` clean; `go test ./...` all pass (engine, db, api, onnxrt, users).
- Held-out accuracy (CGO, own enrollment + recognition): **11/11 = 100%**
  (held-out scores 0.47–0.81, wrong-person <0.20 → 0.45 threshold has margin).
- CGO↔Python parity (measured before the sidecar was removed): detection bbox
  IoU ≥ 0.995, score |Δ| ≤ 0.009 across all 38 photos; ORT numerics are
  bit-identical between CGO and Python for the same input tensor. (Embeddings
  differ ~0.98–0.995 cosine only because the old sidecar lossy-JPEG-encoded the
  aligned crop; the CGO path tensorizes directly and is *more* faithful.)
- Multi-face: a two-person composite returns both identities with separate boxes.
- No-face photo → `{count:0, faces:[]}` (HTTP 200), not an error.
- Dataset regression gate: `make dataset-test` runs
  `scripts/dataset-test.sh` — the CGO dataset tests over the `people/` tree.
  `datasetImages` walks **both** layouts: repo folders
  (`people/<repo-uuid>/<Person>/*.jpg`) and the legacy flat
  `people/<Person>/` (pre-import), so the gate works before and after
  `recogn repo import`.
- Docker: `docker compose up --build` serves UI+API, healthcheck `healthy`,
  DBs persist in the mounted `./data` → `/data/db`, models in `./models` →
  `/data/models` (first run downloads them into the mount). Image ~157 MB.

## Docker

`Dockerfile` is multi-stage: (1) run `make ort` inside the image (reuses the
Makefile recipe; ORT version pinned there), (2) build
the web UI with Vite (`node:22-bookworm-slim`, `npm ci && npm run build`),
(3) build the CGO binary with `gcc`; the ORT tree is copied to
`/src/third_party/onnxruntime` so the `go:embed` pattern in
`ort_embed_linux.go` matches (embed paths resolve relative to the module dir)
and the binary dlopens the embedded library at runtime,
(4) slim `debian:bookworm-slim` runtime with just the self-contained binary +
`curl` for the healthcheck, non-root user,
`EXPOSE 8080`, `VOLUME /data/db` + `/data/models`. The ONNX models are **not**
baked into the image (they'd add ~191 MB): `RECOGN_MODELS_DIR=/data/models`
in the image ENV and `docker-compose.yml` mounts `./models` there; on first
start `models.Ensure` (internal/models, wired in `openEngine`) downloads the
insightface buffalo_l pack into the mount (~289 MB) and extracts only the two
`.onnx` files — the mount must be writable by uid 10001 for that first run;
pre-populated folders need no download (read-only mounts then work), and
`RECOGN_MODELS_URL` overrides the pack URL / `RECOGN_AUTO_DOWNLOAD=0`
disables it. `docker-compose.yml` also mounts `./people` — the **repos root**
(`people/<repo-uuid>/<Person>/`; legacy flat folders are wrapped with
`recogn repo import`) — writable at `/data/people` (API enrollments save
uploaded photos back into it), persists the DBs via `./data` → `/data/db`,
sets `RECOGN_THRESHOLD`, healthcheck via `curl /api/health` (`start_period:
5m` to cover a first-run model download). The sample ships **secure by
default**: login is always required — with zero users the first start prints
an in-memory setup key to the console (`docker compose logs recogn`), the
operator completes the setup view to create the admin; there is **no
env-based admin bootstrap** — and the port binds to `127.0.0.1`
only (a commented `8080:8080` line documents the all-interfaces
switch). `.dockerignore` excludes `people/`, `models/`, `data/`,
`third_party/`, `python/`, caches, `web/node_modules/`,
and the host `internal/web/dist/` (the image builds its own bundle).

**Docker CLI commands need elevated sandbox permissions** (the daemon socket and
`~/.docker/buildx` state live outside the workspace) — retry with
`sandbox_permissions: danger-full-access` when a `docker`/`docker compose`
command hits a permission error.

## Common tasks

- **First run**: `./recogn serve` with zero users prints
  `setup key: XXXX-XXXX-XXXX-XXXX` to the console (stdout + slog). Complete
  the UI's setup view (key + admin username + password, `POST /api/setup`).
  The key is in-memory only and regenerated on every start until an admin
  exists; `/api/setup` answers 403 on a wrong key or once an admin exists.
- **Add a person**: drop `people/<repo-uuid>/<Name>/*.jpg`, then
  `./recogn enroll --repo X` (or `POST /api/enroll?repo=X` as a writer, or the
  UI's "Rescan" for that repository). Or upload at runtime:
  `POST /api/people/<name>/enroll` — successful uploads are also written into
  the person's folder as `<sha1-12><ext>` (content-derived name; the DB photo
  path is the matching basename), so the dataset folder and DB stay in sync
  and a rescan recognizes them as already enrolled. The Docker `people` mount
  must therefore be writable (container uid 10001 needs write access on the
  host folder).
- **Import a legacy dataset**: `recogn repo import --name "My people"` wraps
  the old flat root-level `people/<Person>/` folders into one new repository —
  moves them under `people/<uuid>/`, rewrites person IDs to `<uuid>-<slug>`
  and renames the thumbnail sidecars. Run it once.
- **Manage users and access** (admin): create repositories (`POST /api/repos`
  or the admin UI) and users, hand out per-repo grants
  (`POST /api/users/{u}/repos`, `{"grants":[{"repo":"<name|id>","role":
  "reader|writer"}]}` — reader = browse/recognize, writer = + enroll/photo
  management/full-res photos; admin is implicitly full), reset passwords
  (users change their own via `POST /api/auth/password`; the admin cannot be
  deleted), toggle per-repo share links (`POST /api/repos/{id}/share` →
  `/share/<repoID>/<token>`, anonymous reader via the URL or the
  `X-Share-Token` header; the token is stored plaintext in the repo record
  and can be re-read by the admin via `GET /api/repos/{id}/share` or
  the Repositories dialog's "Show link" button — revealing never rotates;
  disabling clears it, re-enabling mints a fresh token). Users mint/revoke
  personal API keys in the UI (`rk_...`, hashed at rest in `data/users.db` —
  unlike share tokens, API keys are never recoverable; a key acts
  as its user via `Authorization: Bearer rk_...`).
- **Tune strictness**: raise/lower `RECOGN_THRESHOLD` / `--threshold` /
  `POST /api/config` (the threshold stays global). Higher = fewer false
  positives, more `unknown`s.
- **Behind a reverse proxy — client-IP keying** (optional): rate limiting
  (login lockout, recognize admission) keys on the network peer address by
  default and ignores `X-Forwarded-For` (client-controlled). Behind a proxy
  that appends the client IP, set `RECOGN_TRUSTED_PROXY_CIDR`
  (comma-separated CIDRs, e.g. `10.0.0.0/8,192.168.0.0/16`): when a request's
  direct peer is inside a trusted CIDR, the key becomes the rightmost
  non-trusted X-Forwarded-For entry (spoofed entries further left cannot
  steer the key; unparsable chains fail closed to the peer). The proxy must
  APPEND client IPs (the standard behaviour); misconfiguring a public peer as
  trusted lets clients mint arbitrary rate-limit keys. An invalid CIDR fails
  startup loudly. Without the variable, behaviour is unchanged: every client
  behind one proxy shares one lockout bucket.
- **Run the server**: `make serve` (or `./recogn serve --addr :8080` with the
  env exports above). Zero users → the setup key appears on the console.
- **Re-verify the dataset pipeline**: `make dataset-test`
  (= `scripts/dataset-test.sh`; `RECOGN_DATASET=1 go test ./internal/engine/
  -run 'TestCGODatasetPipeline|TestCGOBatchedEmbedParity' -count=1 -v`) —
  checks every enrolled photo detects a face, that intra-person embeddings
  cluster tighter than inter-person, and that batched embedding still matches
  per-face embedding on real data.

## Conventions to keep

- **Minimal Go deps** — stdlib + `x/image` + `bbolt` (DB) only. Image
  crop/resize/warp are hand-rolled in `engine.go`; reuse them.
- **Front-end is Preact + TypeScript, built by Vite** — sources live under
  `web/`, the build lands in `internal/web/dist/` and is embedded by
  `go:embed all:dist` (`internal/web/web.go`). Build with `make ui`
  (npm ci + `tsc --noEmit && vite build`; needs Node ≥ 20; frontend deps
  beyond preact are deliberately tiny: `marked` + `dompurify` for the
  details modal's markdown). During UI work run
  `npm --prefix web run dev` for the Vite dev server on :5173 — it proxies
  `/api` to the Go server on :8080, so no rebuild is needed for API-side
  checks. Architecture notes:
  - **State lives in `App`** (session, repos, people, threshold, which modal
    is open); children refresh each other via `refreshAll()` callbacks — the
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
  slot; Go-side pre/post-processing stays outside the gate. Slot release is
  deferred (`gatedRun`), so a panic inside a Run cannot leak its slot and
  wedge all inference (pinned by `TestGateSlotReleasedOnPanic`). `close()` drains
  every slot before closing the sessions.
- **DB storage is bbolt** (dep `go.etcd.io/bbolt`) in **two files**:
  `data/faces.db` (the face DB) and `data/users.db` (users, per-repo grants,
  hashed API keys). The face DB keeps an in-memory mirror behind the DB
  RWMutex — reads never touch the file. Every mutating method commits a
  targeted `bolt.Update` FIRST, then updates the cache, so a failed write
  leaves memory and disk consistent. Each bbolt file holds an exclusive
  lock: a second `recogn` process on the same DB fails at `Open` (tests must
  `Close()` before reopening the same path — same-process double-open
  deadlocks). Schema v2 of `faces.db` adds a **repos bucket** and
  `Person.RepoID`; person IDs are `<repoUUID>-<slug>`. **JSON interchange**
  (`embeddings.json` next to the face DB): imported once per empty store
  (parse failure fails `Open` loudly; the file is never renamed, so deleting
  `faces.db` re-imports it — the JSON can be stale relative to the store,
  hence the stderr hint) and written by `db.Export`/`recogn export` (people
  name-sorted for byte-stable diffs; the export covers all repositories).
  Photo records on disk: `uvarint hashLen + hash + uvarint dim + dim×float32
  LE` (`encodePhoto`/`decodePhoto`; decode rejects size mismatches).
  **People returned by the accessors are deep copies** (`copyPersonLocked`):
  `People`/`Get`/`GetByID`/`ExportTo` hand out their own `Photos` slice so
  readers never race the in-place mirror writes (element replace, sort,
  append-shift, pinned by `internal/db/alias_test.go`
  incl. a `-race` probe). `Photo.Embedding` backing arrays stay shared —
  the store never mutates an embedding after the Photo is created; keep it
  that way or the copies stop being safe. Optional person metadata
  (`Person.Meta` — aliases/birthdate/URLs/markdown description) rides on the
  person record: every site that rebuilds a record for persistence
  (`addPhoto`'s existing branch, `RenamePerson`, `SetThumbnail`,
  `ClearThumbnail`, the JSON import) must **copy the stored record** instead
  of assembling a fresh struct literal, or the metadata is silently wiped —
  pinned by `TestMetaSurvivesMutations`.
- Embeddings are stripped from API/CLI JSON output (`Face.Embedding` is `json:"-"`
  or nil-ed) — don't leak 512-float arrays to clients.
- **API request bodies are capped**: 32 MiB (`maxUpload`) on `/api/recognize`
  and the enroll endpoints (multipart images; `/api/compare` caps the whole
  two-photo body the same way), 1 MiB on JSON bodies
  (`POST /api/config`, `POST /api/repos`, `POST /api/users/{u}/repos`, …).
  Wrap new handlers' bodies in `http.MaxBytesReader`/`io.LimitReader` the
  same way. Multipart **parse memory is 10 MiB** (`multipartMemory`, not
  `maxUpload`): larger file parts spill to OS temp files instead of sitting
  RAM-resident for the request's lifetime.
- **Admission control on the inference endpoints** (`internal/api/admission.go`):
  `/api/recognize` (reader+) is rate-limited per client
  IP — token bucket, burst 30, refill 1/s → 429 + `Retry-After` — and both
  recognize and `/api/compare` share a **non-blocking in-flight gate** sized
  `2 × resolveWorkers` (floor 2); a full gate answers 503 + `Retry-After: 2`.
  Requests are deliberately never queued: queueing pins waiting memory, the
  failure mode being prevented. The limiter keys on `auth.Service.ClientIP`
  (same keying as login — see `RECOGN_TRUSTED_PROXY_CIDR` above). When adding
  another inference-heavy route, wrap it in `s.inflight.tryAcquire()`/`release`
  and pin a test in `internal/api/admission_test.go`.
- **Image decodes are pixel-gated** (`internal/engine/decode.go`): every
  engine-side decode of image bytes goes through `decodeLimited`, which reads
  only the header first and rejects images declaring more than
  `maxDecodePixels` (50 MP) before `image.Decode` allocates pixels —
  decompression-bomb guard (the 32 MiB body cap bounds
  compressed bytes only). Errors carry `engine.ErrImageTooLarge`, which the
  API maps to HTTP 400. Never call `image.Decode` on untrusted bytes in the
  engine; larger local photos are skipped with a warning at enrollment.
  `runServe` also sets a 4 GiB soft memory limit (unless `GOMEMLIMIT` is set)
  as a backstop.
- **Auth is always on** (`internal/auth` + `internal/users`): `api.New`
  returns `(*Server, error)` and builds the auth service. Public routes:
  `/api/setup` (first run only), `/api/login`, `/api/logout`,
  `/api/auth/session`, `/api/health` and enabled share links — **everything
  else requires a principal**. Principals: the HttpOnly `recogn_session`
  cookie (sessions are in-memory — a restart logs everyone out;
  `RECOGN_SESSION_TTL`, default 24h, sliding, bounds them), a personal API
  key (`Authorization: Bearer rk_...`, stored **hashed** in `data/users.db`,
  acts as its user), or a share token (`X-Share-Token: <repoID>/<token>` —
  anonymous reader on that one repository only). Scoped routes check the
  caller's per-repo role (reader/writer) on the repository the route acts
  on; repo/user/share management and `POST /api/config` are admin-only.
  First run: `serve` with zero users prints an in-memory setup key and
  `POST /api/setup` exchanges it for the admin account (403 on a wrong key
  or after completion) — this replaces the old env-based admin bootstrap,
  and it is what stops a fresh deployment from being captured by the first
  network peer. Failed logins are rate-limited per IP (10 failures / 5 min →
  429 + `Retry-After`) — keep that: it is what blunts online password
  guessing.
- Enrollment stores **one embedding per photo** (largest face) and matches
  per-person by best similarity, **within the selected repository**. Photos
  with no detectable face are skipped with a warning, never stored. DB photo
  paths are **basenames relative to the person's folder** (folder scans and
  API uploads both derive the same name); API uploads additionally persist
  the image bytes into the person's folder under `people/<repo-uuid>/`.
  `Recognize` also fills `Face.Matches`: every person above the threshold,
  ranked best-first (one entry per person, per-person best) — the UI lists
  them to help spot duplicate people; the single best match still drives
  `Name`/`PersonID`/`Confidence`.
- **Face thumbnails** are a DB sidecar: `thumbs/<personID>.jpg` next to
  the DB file (`data/thumbs/`; see `db.SetThumbnail`/`ThumbFile`, crop via
  `engine.FaceThumb`). Auto-generated at first enrollment only (enrollment
  callers skip when one exists); the API's `POST /api/people/{name}/thumbnail`
  (writer) re-selects the source photo, which **overwrites** the sidecar and
  records it as `Person.ThumbSrc`. Always **best-effort**: thumbnail failures
  must never fail an enrollment. Missing thumbnails are backfilled by rescans
  without re-embedding. Thumbnail URLs carry a `?v=` cache-buster derived
  from `ThumbSrc`.
- **Per-photo management** (photos-manager modal): `GET
  /api/people/{name}/photos/{path}/detect` (reader+) recognizes an enrolled
  photo's faces for the UI's quality check (embeddings stripped, same rule
  as `/api/recognize`); `DELETE /api/people/{name}/photos/{path}` (writer)
  removes the DB entry + the file from the person's folder, and when the
  deleted photo was `ThumbSrc`, regenerates the thumbnail from the first
  remaining photo that still detects a face — or clears it
  (`db.ClearThumbnail`) when none does. Deleting a person's last photo leaves
  the person enrolled with 0 photos.
- **Renaming a person** (Rename button in the photos-manager modal →
  `POST /api/people/{name}/rename`, body `{"name": ...}`): one transaction
  across disk and DB — `os.Rename` of the person's folder within its
  repository (a 409 refuses to clobber a different existing folder;
  `os.SameFile` lets case-only renames work on case-insensitive filesystems),
  then `db.RenamePerson` re-derives the ID from the repo UUID + name slug
  (`db.newID`), renames the thumbnail sidecar to `<newID>.jpg` and persists.
  A DB failure after the folder move rolls the folder back. Photo paths and
  `ThumbSrc` are folder-relative basenames, so they need no rewrite; the
  engine identity set reloads via `s.reload()` afterwards.
- **Deleting a person** (Remove button in the people list →
  `DELETE /api/people/{name}`, writer): removes the DB record and the
  thumbnail sidecar, then deletes the person's folder best-effort — otherwise
  the next rescan would silently re-enroll the person. The response reports
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
- No TLS in-app. Login is always required (only the first-run setup view,
  `/api/login`, `/api/logout`, `/api/auth/session`, `/api/health` and enabled
  share links are public), so expose remotely only behind a TLS reverse proxy
  — over plain HTTP the password crosses the wire in cleartext.
- No screenshot-based UI test (no headless browser in this env).
