# recogn

<p align="center">
  <img src="./web/public/logo.svg" width="96">
</p>

Face recognition for a known set of people — **CLI + REST API + minimal web UI**, in Go.

Point it at a folder of people, enroll them once, then feed it any photo: it
detects **every face** in the image and tells you who each one is (or
`unknown`), with a confidence score.

## How it works

- **Detection** — [SCRFD](https://github.com/deepinsight/insightface) (`det_10g`) finds all faces and their 5-point landmarks.
- **Alignment** — each face is warped to a canonical 112×112 crop (Umeyama similarity transform, in Go).
- **Recognition** — [ArcFace](https://github.com/deepinsight/insightface) (`w600k_r50`) turns the crop into a 512-d embedding; a face is identified by **cosine similarity** against every enrolled embedding, matched per-person by best score.
- A match above the **threshold** (default `0.45`, tunable) names the person; below it the face is reported as `unknown`.

Go owns the whole pipeline: CLI, HTTP API, web UI, face database, face
alignment (Umeyama), matching (cosine similarity), and the detection
pre/post-processing. The only thing delegated to native code is running the two
ONNX models, which happens **in-process via CGO + the [ONNX Runtime C
API](https://onnxruntime.ai)** (`libonnxruntime`). There is no Python and no
separate inference process — the binary is self-contained apart from the ORT
shared library. Everything runs on CPU.

## Layout

```
recogn                 the single binary (built)
main.go                CLI entry: enroll | recognize | people | export | serve
internal/
  config/              paths, threshold, env/flags
  onnxrt/              minimal CGO binding to the ONNX Runtime C API
  engine/              detect→align→embed→match pipeline
    preprocess.go        image → CHW float tensor (SCRFD + ArcFace)
    scrfd.go             SCRFD output decode + NMS
    cgo_backend.go       in-process inference backend (onnxrt)
  db/                  face database: bbolt store (data/faces.db) + JSON import/export
  enroll/              people/ folder scanning + embedding
  api/                 REST API handlers
  web/                 serves the embedded UI bundle (dist/ via go:embed)
web/                   the web UI sources: Preact + TypeScript, built by Vite
  src/                 components, typed API client, styles
third_party/onnxruntime/  ORT C header + libonnxruntime (via `make ort`)
third_party/onnxruntime-win/  Windows ORT C header + onnxruntime.dll (via `make ort-win`)
models/                det_10g.onnx, w600k_r50.onnx  (downloaded)
people/                <Person Name>/*.jpg ...       (your dataset)
data/faces.db          generated face DB (bbolt, single file)
data/embeddings.json   JSON interchange copy — auto-imported when the DB is
                       empty, written by `recogn export`
```

## Run with Docker (easiest)

The image is all-in-one: the CGO-enabled Go binary, the ONNX Runtime library,
and the models — no Python. You only need Docker.

```sh
docker compose up --build      # build and start
# open http://localhost:8080
```

On first start, if the face DB is empty, the container **auto-enrolls** from
the mounted `./people` folder before serving. The generated database lives in a
named volume (`recogn-db`) so it survives rebuilds and restarts.

- **Dataset**: `./people` is mounted writable at `/data/people` so photos
  enrolled through the API/UI are saved back into it (as `<Name>/<sha1>.<ext>`).
- **Threshold**: set `RECOGN_THRESHOLD` in `docker-compose.yml`.
- **One-off CLI** (against the same image):

  ```sh
  docker compose run --rm recogn people
  docker compose run --rm recogn recognize /data/people/Yana/img_0103.jpg
  docker compose run --rm recogn enroll --force
  ```

Without compose, plain Docker works too:

```sh
docker build -t recogn .
docker run -p 8080:8080 -v "$PWD/people:/data/people" -v recogn-db:/data/db recogn
```

## Run from source

### Prerequisites

- Go 1.22+
- A C toolchain (`gcc`) — inference uses CGO
- Node.js 20+ with npm — the web UI (Preact + TypeScript) is built by Vite
- The ONNX Runtime C library + header (fetched into `third_party/onnxruntime` by `make ort`)
- The two ONNX models in `./models` (see below)

### Setup

```sh
make models     # download SCRFD + ArcFace into ./models (~289 MB, once)
make build      # fetch the ORT C library (make ort), build the web UI (make ui),
                # and build ./recogn with CGO
```

`make build` depends on `make ort`, which downloads `libonnxruntime` +
`onnxruntime_c_api.h` into `third_party/onnxruntime`, and on `make ui`, which
installs the web UI's npm dependencies (once) and builds it into
`internal/web/dist/` — that folder is embedded into the binary. The binary is
linked with an `$ORIGIN`-relative rpath, so it runs in place as long as
`third_party/` stays next to it.

### Web UI development

The front-end (`web/`) is a standalone Vite project. For UI work, run the Go
server and the Vite dev server side by side — API calls are proxied, so there
is no rebuild loop:

```sh
make serve                    # Go API + (previously built) UI on :8080
npm --prefix web run dev      # Vite dev server on http://localhost:5173
```

`npm --prefix web run build` type-checks (`tsc --noEmit`) and rebuilds the
bundle that the Go binary embeds.

### Cross-compile for Windows

You can build a Windows binary (`recogn.exe`) from Linux or macOS using
[mingw-w64](https://www.mingw-w64.org/):

```sh
# Install the cross-compiler (once):
#   Debian/Ubuntu : sudo apt install gcc-mingw-w64-x86-64
#   Fedora        : sudo dnf install mingw64-gcc
#   macOS (brew)  : brew install mingw-w64

make build-windows   # fetch Windows ORT + cross-compile recogn.exe
```

This downloads the Windows ONNX Runtime distribution into
`third_party/onnxruntime-win/` and produces `recogn.exe`. To run the binary on
Windows, `onnxruntime.dll` must be next to the `.exe` (or on `PATH`).

To produce a self-contained zip with the binary, DLL, and models:

```sh
make dist-windows    # → dist/recogn-windows-x64.zip
```

Unzip on a Windows machine, open a terminal in the folder, and use
`recogn.exe` exactly like the Linux binary (e.g. `recogn.exe serve`).

## Use

### CLI

```sh
# Enroll everyone under people/ (incremental; --force re-embeds everything)
./recogn enroll

# Identify every face in one or more photos
# (flags go before the image paths)
./recogn recognize --json photo.jpg group.jpg

# The same, plus an annotated copy with numbered boxes + labels drawn on it:
# single image  -> --draw takes an output .jpg path
# several files -> --draw takes a directory (one <name>.annotated.jpg each)
./recogn recognize --draw annotated.jpg photo.jpg
./recogn recognize --draw out/ photo1.jpg photo2.jpg

# List enrolled identities
./recogn people
```

### Web UI + API

```sh
./recogn serve --addr :8080
# open http://localhost:8080
```

The web UI lets you drag-and-drop a photo to see every detected face boxed and
labelled, browse the enrolled people, add a new person by uploading photos, and
re-scan the `people/` folder. Unknown faces can be named right in the results
list ("Enroll" under an unknown face enrolls that specific face — matched by
its row number, which the canvas overlay also shows next to the name). The
people panel has a filter box and a threshold slider (persisted server-side).

#### REST API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/recognize` | multipart `image` → `{count, faces:[{bbox,name,person_id,confidence,score,landmarks,matches}]}` — `matches` ranks every identity above the threshold (best per person); near-tied top scores hint at duplicate people. `?draw=1` responds with the annotated JPEG (numbered boxes + labels) instead of JSON |
| `GET`  | `/api/people` | list enrolled people + photo counts (incl. `thumb` URL when a face thumbnail exists) |
| `GET`  | `/api/people/{name}` | one person's enrolled photos (`thumb_src` = photo the avatar comes from) |
| `POST` | `/api/people/{name}/enroll` | add photo(s) (field `images`) for a new/existing person; each enrolled image is also saved into `people/<name>/` |
| `POST` | `/api/people/{name}/enroll-face` | enroll one specific face of an uploaded photo: multipart `image` + `face_index` (1-based, in the detection order `/api/recognize` reports) — used by the UI's "name this face" action on unknown results |
| `GET`  | `/api/people/{name}/photos/{path}` | one of the person's enrolled photo files (from the people folder) |
| `GET`  | `/api/people/{name}/photos/{path}/detect` | detect + recognize faces on an enrolled photo (for the photos-manager quality check) |
| `DELETE` | `/api/people/{name}/photos/{path}` | remove one photo: DB entry + file in `people/<name>/`; the avatar is regenerated from another photo (or cleared) if it was the thumbnail source |
| `POST` | `/api/people/{name}/thumbnail` | regenerate the face thumbnail from a chosen enrolled photo — JSON `{"photo": "<path>"}` |
| `POST` | `/api/people/{name}/rename` | rename a person — JSON `{"name": "<new name>"}`; moves `people/<name>/`, re-derives the person ID, renames the thumbnail sidecar and updates the DB in one step |
| `GET`  | `/api/thumbs/{id}.jpg` | a person's face thumbnail (square face crop; `?v=` cache-buster follows the chosen photo) |
| `DELETE` | `/api/people/{name}` | remove a person |
| `POST` | `/api/enroll?force=true` | re-scan the `people/` folder (incremental unless `force`); `?prune=true` also drops DB photo entries whose files are missing (CLI: `recogn enroll --prune`) |
| `GET`/`POST` | `/api/config` | read/set the match threshold; POSTed values are persisted in the DB and survive restarts (an explicit `--threshold` flag still wins) |
| `GET`  | `/api/health` | status, people count, threshold |

Example:

```sh
curl -F "image=@photo.jpg" http://localhost:8080/api/recognize
```

## Expanding the database

To add someone new, either:

1. **Drop a folder** `people/<Their Name>/` with a few photos and run
   `./recogn enroll` (or `POST /api/enroll`), **or**
2. **Upload photos** at runtime: `POST /api/people/<name>/enroll`. Each
   successfully enrolled upload is written to `people/<name>/<sha1>.<ext>`
   (content-derived name, so re-uploading the same photo is idempotent) and
   its DB entry points at that file — a later `POST /api/enroll` rescan
   recognizes it as already enrolled.

**Storage**: the database lives in a single bbolt file, `data/faces.db` —
writes touch only the changed person/photo record (no whole-file rewrite),
startup loads it directly, and a file lock refuses two `recogn` processes on
the same DB. For humans and backups there is a JSON interchange file,
`data/embeddings.json`: it is **imported automatically whenever the store is
empty** (so deleting `faces.db` and restarting restores the last exported
snapshot — the JSON is never renamed or consumed), and `./recogn export
[--out path]` **writes** the current database to it. The JSON is not kept in
sync by day-to-day enrollment; run `recogn export` to refresh it. Corrupt
JSON fails startup loudly rather than silently starting empty.

**Face thumbnails**: the first enrolled photo that yields a face also produces
a square face-crop thumbnail, stored as a sidecar next to the database file
(`data/thumbs/<person-id>.jpg`) and served at `/api/thumbs/<id>.jpg` — the web
UI's people list shows it as the person's avatar. Click a person's avatar (or
row) to open the **photos manager**: a modal listing their enrolled photos
where you can add more (upload happens immediately) or remove them; deleting
the avatar's source photo regenerates it from another photo. Clicking a photo
runs detection and draws the found face(s) over it, so you can judge whether
it's a good enrollment shot, and re-select it as the avatar. The enroll modal
runs the same face check on every photo *before* you confirm, so a photo with
no detectable face is flagged before it's ever sent. The **Rename** button in
the same modal renames a person everywhere at once — the `people/<Name>/`
folder, their derived person ID, the `thumbs/<id>.jpg` sidecar and the DB
record — so recognition results, photo URLs and avatars all follow the new
name (`POST /api/people/{name}/rename` does the same from a script). Datasets
enrolled before thumbnails existed backfill automatically on the next rescan,
without re-embedding.

Photos with no detectable face are skipped with a warning, never silently
poisoning the database.

## Notes & tuning

- **Threshold** — default `0.45`. Raise it to be stricter (fewer false
  positives, more `unknown`s), lower it to be more permissive. On this dataset
  held-out photos of the right person score ~0.47–0.80 while other people
  score <0.20, so `0.45` has comfortable margin.
- **Spotting duplicate people** — recognition returns *all* identities above
  the threshold per face (the web UI lists them under the face's name). If two
  different people score nearly the same on a photo, they are probably the
  same person enrolled twice; the UI flags near-ties (within 0.05) with a
  hint.
- **Multiple photos per person** improve robustness — enrollment keeps every
  photo's embedding and matches against the best.
- **Concurrency** — inference is *bounded*, not serialised: up to
  `RECOGN_CONCURRENCY` model Runs execute in parallel on the ORT sessions
  (safe because each session runs with intra-op threads = 1, and ORT's CPU
  execution provider is thread-safe for concurrent Runs — verified by
  `TestConcurrentRunParity`). Multi-face photos embed their faces through
  `embedBatch`, which fans per-face Runs across that gate (tensor batching is
  intentionally *not* used: the ArcFace export's BatchNormalization couples
  faces within a batch, see `TestRunBatchEmbedder`). Batch jobs (enrollment
  scan, CLI recognize, multi-upload) fan out over the same worker budget; the
  HTTP layer is concurrent and the DB saves are atomic.
  `RECOGN_CONCURRENCY` defaults to `min(NumCPU, 4)`; set it to `1` to restore
  strictly serial inference.
- **No GPU required** — everything runs on CPU via the ONNX Runtime C library.
