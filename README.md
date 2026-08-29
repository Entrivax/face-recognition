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

Go owns the CLI, HTTP API, web UI and the face database. The tensor inference
runs in a small **Python ONNX sidecar** (`python/infer.py`, using
`onnxruntime` + OpenCV) that Go spawns and talks to over stdio. This keeps the
Go binary dependency-light while using best-in-class models, and runs entirely
on CPU.

## Layout

```
recogn                 the single binary (built)
main.go                CLI entry: enroll | recognize | people | serve
internal/
  config/              paths, threshold, env/flags
  engine/              detect→align→embed→match pipeline + sidecar client
  db/                  JSON face database (data/embeddings.json)
  enroll/              people/ folder scanning + embedding
  api/                 REST API handlers
  web/                 embedded web UI (static/)
python/infer.py        ONNX inference sidecar (SCRFD + ArcFace)
models/                det_10g.onnx, w600k_r50.onnx  (downloaded)
people/                <Person Name>/*.jpg ...       (your dataset)
data/embeddings.json   generated face DB
```

## Run with Docker (easiest)

The image is all-in-one: Go binary + Python ONNX sidecar + baked-in models.
You only need Docker.

```sh
docker compose up --build      # build and start
# open http://localhost:8080
```

On first start, if the face DB is empty, the container **auto-enrolls** from
the mounted `./people` folder before serving. The generated database lives in a
named volume (`recogn-db`) so it survives rebuilds and restarts.

- **Dataset**: `./people` is mounted read-only at `/data/people`.
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
docker run -p 8080:8080 -v "$PWD/people:/data/people:ro" -v recogn-db:/data/db recogn
```

## Run from source

### Prerequisites

- Go 1.22+
- Python 3 with `onnxruntime`, `opencv-python` (`cv2`) and `numpy` (see `python/requirements.txt`)
- The two ONNX models in `./models` (see below)

### Setup

```sh
make models     # download SCRFD + ArcFace into ./models (~289 MB, once)
make build      # build ./recogn
```

## Use

### CLI

```sh
# Enroll everyone under people/ (incremental; --force re-embeds everything)
./recogn enroll

# Identify every face in one or more photos
./recognize photo.jpg group.jpg
./recogn recognize photo.jpg --json

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
| `POST` | `/api/recognize` | multipart `image` → `{count, faces:[{bbox,name,person_id,confidence,score,landmarks}]}` |
| `GET`  | `/api/people` | list enrolled people + photo counts |
| `GET`  | `/api/people/{name}` | one person's enrolled photos |
| `POST` | `/api/people/{name}/enroll` | add photo(s) (field `images`) for a new/existing person |
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
2. **Upload photos** at runtime: `POST /api/people/<name>/enroll`.

Photos with no detectable face are skipped with a warning, never silently
poisoning the database.

## Notes & tuning

- **Threshold** — default `0.45`. Raise it to be stricter (fewer false
  positives, more `unknown`s), lower it to be more permissive. On this dataset
  held-out photos of the right person score ~0.47–0.80 while other people
  score <0.20, so `0.45` has comfortable margin.
- **Multiple photos per person** improve robustness — enrollment keeps every
  photo's embedding and matches against the best.
- **Concurrency** — inference is serialized through one sidecar process
  (CPU inference is single-stream); the HTTP layer itself is concurrent and
  the DB saves are atomic.
- **No GPU required** — everything runs on CPU via onnxruntime.
