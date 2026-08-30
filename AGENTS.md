# AGENTS.md — recogn

Read this first. It tells you what the project is, how it's wired, what already
works, and the gotchas that will otherwise waste your time.

## What this is

`recogn` is a **face-recognition application** — CLI + REST API + minimal web UI,
written in **Go** with **CGO**. It maintains a database of known people (from a
`people/` photo folder) and, given any photo, **detects every face** in it and
identifies each one (name + confidence, or `unknown`). It runs on CPU.

Current state: **complete and working.** Inference runs **in-process via CGO +
the ONNX Runtime C API** (no Python, no subprocess). Enrolled 11 people / 38
photos, all tests pass, Docker image builds and runs (~508 MB).

## Architecture (important — don't reinvent this)

Go owns the **entire** pipeline. The only thing delegated to native code is
executing the two ONNX models, done in-process through `libonnxruntime` via a
small CGO wrapper. **There is no Python and no sidecar** (both were removed after
the CGO backend proved output-parity).

- **Go**: CLI, HTTP API, web UI, the JSON face DB, enrollment orchestration,
  face alignment (Umeyama), matching (cosine), AND the detector's
  pre/post-processing (letterbox, CHW tensor build, SCRFD decode, NMS).
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
main.go                  CLI entry: enroll | recognize | people | serve
internal/
  config/config.go       paths, threshold; settings via RECOGN_* env vars
  onnxrt/onnxrt.{c,h,go} CGO binding to the ONNX Runtime C API
  engine/engine.go       Face/KnownPerson, pipeline, Umeyama align, cosine
  engine/preprocess.go   image → CHW float tensor (letterbox + normalise)
  engine/scrfd.go        SCRFD output decode + NMS (pure Go)
  engine/cgo_backend.go  inferencer impl using onnxrt (mutex-serialised)
  db/db.go               JSON face DB (data/embeddings.json), CRUD, atomic saves
  enroll/enroll.go       scan people/ → embeddings (incremental by content hash)
  api/api.go             REST handlers; depends on an Engine INTERFACE (testable)
  web/web.go + static/   embedded single-page UI (go:embed, no build step)
third_party/onnxruntime/ ORT C header + libonnxruntime.so (via `make ort`)
models/                  det_10g.onnx, w600k_r50.onnx  (gitignored; downloaded)
people/<Name>/*.jpg      the dataset — 11 people, 38 photos
data/embeddings.json     generated face DB (gitignored)
data/thumbs/             face thumbnail sidecars, one per person (gitignored)
Dockerfile, docker-compose.yml, .dockerignore
Makefile, README.md, scripts/dataset-test.sh
```

## The inference seam (if you touch internal/engine or internal/onnxrt)

`engine.Engine` talks to an unexported `inferencer` interface
(`detect` / `embedImage` / `ping` / `close`). The production impl is
`cgoInferencer`. `engine.New(detModel, embModel, thresh)` builds it;
`engine.NewWithInferencer` is for tests. Keep inference behind this seam.

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
`make build` / `make test` / `make vet` / `make serve` / `make ort` / `make dataset-test`.

- **CGO is required** (`CGO_ENABLED=1`) and needs `gcc`. The ORT C lib+header
  must exist in `third_party/onnxruntime` — `make ort` fetches them (needs
  network once). The Go `#cgo` directive bakes an rpath of
  `$ORIGIN/third_party/onnxruntime/lib`, so the binary runs in place; keep
  `third_party/` next to the binary.
- `GOPROXY=off` works because the single Go module dep (`golang.org/x/image`)
  is vendored in the local module cache; adding a new Go dep needs network
  (`GOPROXY=https://proxy.golang.org,direct`) + in-workspace `GOPATH` so the
  checksum db is writable.
- Flags are **per-subcommand** (`flag.NewFlagSet`) and Go stops flag parsing at
  the first positional arg: `./recogn recognize --json img.jpg` ✓, `... img.jpg --json` ✗.
- Not a git repo yet — `git init` if you want history.

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
the CGO binary with `gcc` + ORT (rpath set to `/usr/lib/recogn`), (4) slim
`debian:bookworm-slim` runtime with `libonnxruntime` in `/usr/lib/recogn` +
`ldconfig`, `curl` for the healthcheck, non-root user, `EXPOSE 8080`, `VOLUME
/data/db`. `docker-compose.yml` mounts `./people` writable at `/data/people`
(API enrollments save uploaded photos back into it), persists the DB via
`./data` → `/data/db`, sets `RECOGN_THRESHOLD`, healthcheck
via `curl /api/health`. `.dockerignore` excludes `people/`, `models/`, `data/`,
`third_party/`, `python/`, caches.

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
- **Run the server**: `make serve` (or `./recogn serve --addr :8080` with the
  env exports above). Auto-enrolls if the DB is empty and `people/` exists.
- **Re-verify the dataset pipeline**: `RECOGN_DATASET=1 go test ./internal/engine/
  -run TestCGODatasetPipeline` (checks every enrolled photo detects a face and
  that intra-person embeddings cluster tighter than inter-person).

## Conventions to keep

- **Minimal Go deps** — stdlib + `x/image` only. Image crop/resize/warp are
  hand-rolled in `engine.go`; reuse them.
- The engine is safe for concurrent use; a **single mutex** serialises CGO
  inference (matches ORT CPU single-stream semantics). Don't run sessions
  concurrently without checking ORT thread-safety.
- DB writes are atomic (temp file + rename) and mutex-guarded — preserve this.
- Embeddings are stripped from API/CLI JSON output (`Face.Embedding` is `json:"-"`
  or nil-ed) — don't leak 512-float arrays to clients.
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
  `embeddings.json` (see `db.SetThumbnail`/`ThumbFile`, crop via
  `engine.FaceThumb`). Auto-generated at first enrollment only (enrollment
  callers skip when one exists); the API's `POST /api/people/{name}/thumbnail`
  re-selects the source photo, which **overwrites** the sidecar and records it
  as `Person.ThumbSrc`. Always **best-effort**: thumbnail failures must never
  fail an enrollment. Missing thumbnails are backfilled by rescans without
  re-embedding. Thumbnail URLs carry a `?v=` cache-buster derived from
  `ThumbSrc`.
- **onnxrt memory discipline**: every `OrtValue`/buffer allocated in the C shim
  is freed (tensor data via `ort_free`, sessions via `ort_close`). If you extend
  the shim, keep the ownership rules in `onnxrt.h` accurate and re-run the
  repeated-run test (`internal/onnxrt/onnxrt_test.go`) to catch leaks.

## Known limitations / possible next tasks

- **CPU-only** inference (~0.2–0.6 s/photo). GPU = use the ORT GPU build of
  `libonnxruntime` + enable a CUDA execution provider in the C shim + a CUDA
  base image.
- Single serialized inference stream — high-throughput batch work would need a
  session pool or batched tensors.
- CGO means no static/cross-compiled binary; the binary links glibc +
  libonnxruntime. Builds are for the host (linux/amd64) unless you set up a
  cross C toolchain.
- No auth/TLS on the API — it's a local tool; add middleware if you expose it.
- No screenshot-based UI test (no headless browser in this env).
