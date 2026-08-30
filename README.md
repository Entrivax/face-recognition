# recogn

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
main.go                CLI entry: enroll | recognize | people | serve
internal/
  config/              paths, threshold, env/flags
  onnxrt/              minimal CGO binding to the ONNX Runtime C API
  engine/              detect→align→embed→match pipeline
    preprocess.go        image → CHW float tensor (SCRFD + ArcFace)
    scrfd.go             SCRFD output decode + NMS
    cgo_backend.go       in-process inference backend (onnxrt)
  db/                  JSON face database (data/embeddings.json)
  enroll/              people/ folder scanning + embedding
  api/                 REST API handlers
  web/                 embedded web UI (static/)
third_party/onnxruntime/  ORT C header + libonnxruntime (via `make ort`)
third_party/onnxruntime-win/  Windows ORT C header + onnxruntime.dll (via `make ort-win`)
models/                det_10g.onnx, w600k_r50.onnx  (downloaded)
people/                <Person Name>/*.jpg ...       (your dataset)
data/embeddings.json   generated face DB
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
- The ONNX Runtime C library + header (fetched into `third_party/onnxruntime` by `make ort`)
- The two ONNX models in `./models` (see below)

### Setup

```sh
make models     # download SCRFD + ArcFace into ./models (~289 MB, once)
make build      # fetch the ORT C library (make ort) and build ./recogn with CGO
```

`make build` depends on `make ort`, which downloads `libonnxruntime` +
`onnxruntime_c_api.h` into `third_party/onnxruntime`. The binary is linked with
an `$ORIGIN`-relative rpath, so it runs in place as long as `third_party/`
stays next to it.

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
re-scan the `people/` folder.

#### REST API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/recognize` | multipart `image` → `{count, faces:[{bbox,name,person_id,confidence,score,landmarks,matches}]}` — `matches` ranks every identity above the threshold (best per person); near-tied top scores hint at duplicate people |
| `GET`  | `/api/people` | list enrolled people + photo counts (incl. `thumb` URL when a face thumbnail exists) |
| `GET`  | `/api/people/{name}` | one person's enrolled photos (`thumb_src` = photo the avatar comes from) |
| `POST` | `/api/people/{name}/enroll` | add photo(s) (field `images`) for a new/existing person; each enrolled image is also saved into `people/<name>/` |
| `GET`  | `/api/people/{name}/photos/{path}` | one of the person's enrolled photo files (from the people folder) |
| `POST` | `/api/people/{name}/thumbnail` | regenerate the face thumbnail from a chosen enrolled photo — JSON `{"photo": "<path>"}` |
| `GET`  | `/api/thumbs/{id}.jpg` | a person's face thumbnail (square face crop; `?v=` cache-buster follows the chosen photo) |
| `DELETE` | `/api/people/{name}` | remove a person |
| `POST` | `/api/enroll?force=true` | re-scan the `people/` folder (incremental unless `force`) |
| `GET`/`POST` | `/api/config` | read/set the match threshold |
| `GET`  | `/api/health` | status, people count, threshold |

Example:

```sh
curl -F "image=@photo.jpg" http://localhost:8080/api/recognize
```

## Expanding the database

The database is a single editable JSON file, `data/embeddings.json`. To add
someone new, either:

1. **Drop a folder** `people/<Their Name>/` with a few photos and run
   `./recogn enroll` (or `POST /api/enroll`), **or**
2. **Upload photos** at runtime: `POST /api/people/<name>/enroll`. Each
   successfully enrolled upload is written to `people/<name>/<sha1>.<ext>`
   (content-derived name, so re-uploading the same photo is idempotent) and
   its DB entry points at that file — a later `POST /api/enroll` rescan
   recognizes it as already enrolled.

**Face thumbnails**: the first enrolled photo that yields a face also produces
a square face-crop thumbnail, stored as a sidecar next to the database file
(`data/thumbs/<person-id>.jpg`) and served at `/api/thumbs/<id>.jpg` — the web
UI's people list shows it as the person's avatar. Click a person's avatar to
open a chooser modal and pick which of their photos generates the thumbnail
(applies immediately; the DB records the chosen photo). Datasets enrolled
before thumbnails existed backfill automatically on the next rescan, without
re-embedding.

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
- **Concurrency** — inference is serialized through a single mutex around the
  ORT sessions (CPU inference is single-stream); the HTTP layer itself is
  concurrent and the DB saves are atomic.
- **No GPU required** — everything runs on CPU via the ONNX Runtime C library.
