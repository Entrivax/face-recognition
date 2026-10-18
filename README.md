# recogn

<p align="center">
  <img src="./web/public/logo.svg" width="96">
</p>

Face recognition for a known set of people — **CLI + REST API + minimal web UI**, in Go.

People live in **repositories** — each one a dataset of person photo folders
(`people/<repo-uuid>/<Person>/*.jpg`), created by the admin. Users log in and
work inside the repositories they've been granted (read or write). Enroll the
people once, then feed the recognizer any photo: it detects **every face** in
the image and tells you who each one is (or `unknown`), with a confidence
score. The server always requires login — the admin is created once through a
first-run setup key, and there is no open mode.

## How it works

- **Detection** — [SCRFD](https://github.com/deepinsight/insightface) (`det_10g`) finds all faces and their 5-point landmarks.
- **Alignment** — each face is warped to a canonical 112×112 crop (Umeyama similarity transform, in Go).
- **Recognition** — [ArcFace](https://github.com/deepinsight/insightface) (`w600k_r50`) turns the crop into a 512-d embedding; a face is identified by **cosine similarity** against every enrolled embedding, matched per-person by best score.
- A match above the **threshold** (default `0.45`, tunable) names the person; below it the face is reported as `unknown`.
- Recognition is **scoped to one repository** — CLI flags and API routes take a `repo` (name or id) and match only that repository's enrolled people.

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
main.go                CLI entry: enroll | recognize | people | repo | export | serve
internal/
  config/              paths, threshold, env/flags
  onnxrt/              minimal CGO binding to the ONNX Runtime C API
  engine/              detect→align→embed→match pipeline
    preprocess.go        image → CHW float tensor (SCRFD + ArcFace)
    scrfd.go             SCRFD output decode + NMS
    cgo_backend.go       in-process inference backend (onnxrt)
  db/                  face database: bbolt store (data/faces.db) + JSON import/export
  users/               user accounts, sessions, per-repo grants, API keys
                       (data/users.db)
  auth/                login sessions + rate limiting
  enroll/              repository folder scanning + embedding
  api/                 REST API handlers
  web/                 serves the embedded UI bundle (dist/ via go:embed)
web/                   the web UI sources: Preact + TypeScript, built by Vite
  src/                 components, typed API client, styles
third_party/onnxruntime/  ORT C header + libonnxruntime (via `make ort`)
third_party/onnxruntime-win/  Windows ORT C header + onnxruntime.dll (via `make ort-win`)
models/                det_10g.onnx, w600k_r50.onnx  (downloaded)
people/                <repo-uuid>/<Person Name>/*.jpg ...  (repositories;
                       legacy flat <Person>/ folders until `recogn repo import`)
data/faces.db          generated face DB (bbolt, single file; schema v2 —
                       a repos bucket, person IDs <repo-uuid>-<slug>)
data/users.db          users, per-repo grants, hashed API keys (bbolt)
data/embeddings.json   JSON interchange copy — auto-imported when the DB is
                       empty, written by `recogn export`
```

## Run with Docker (easiest)

The image is all-in-one: the CGO-enabled Go binary and the embedded ONNX
Runtime library — no Python. You only need Docker. The ONNX models are **not**
baked into the image (they'd add ~191 MB): on first start they download into
the mounted `./models` folder, or you can pre-populate it with `make models`.

```sh
docker compose up --build    # 1. build and start
docker compose logs recogn   # 2. look for: setup key: XXXX-XXXX-XXXX-XXXX
# 3. open http://localhost:8080, enter the setup key and pick the admin
#    username + password — then create repositories and users from the
#    admin UI (users get per-repo reader/writer roles and can mint
#    personal API keys)
```

The compose sample ships **secure by default**: login is always required —
there is no open mode (only the setup view, `/api/login`, `/api/health` and
any enabled share link are reachable unauthenticated) — and it publishes the
port on `127.0.0.1` only. To serve other machines, point a TLS reverse proxy
at it (or switch the commented `8080:8080` binding).

There is no default repository. To wrap an existing legacy dataset (flat
`people/<Person>/` folders) into one repository, then enroll it:

```sh
docker compose run --rm recogn repo import --name "My people"
docker compose run --rm recogn enroll --repo "My people"
```

The databases persist in `./data` (mounted at `/data/db` — the face DB and
the user DB) so they survive rebuilds and restarts.

- **Models**: `./models` is mounted at `/data/models`. On first start the app
  downloads the insightface buffalo_l pack (~289 MB) into it and keeps only
  the two `.onnx` files it needs — so the folder must be writable by the
  container's uid 10001 once (`sudo chown -R 10001 ./models`). Pre-populate
  it with `make models` and nothing is downloaded (a read-only mount then
  works too). Point `RECOGN_MODELS_URL` at your own pack for mirrors /
  air-gapped hosts; `RECOGN_AUTO_DOWNLOAD=0` disables the download entirely.
- **Datasets**: `./people` is the **repos root** — one folder per repository
  (`./people/<repo-uuid>/<Person>/*.jpg`), mounted writable so photos
  enrolled through the API/UI are saved back into it (as
  `<Person>/<sha1>.<ext>`).
- **Threshold**: set `RECOGN_THRESHOLD` in `docker-compose.yml`.
- **Users**: created from the admin UI (or `POST /api/users`); per-repo
  reader/writer roles, personal API keys, optional per-repo share links —
  see [Users and access control](#users-and-access-control).
- **One-off CLI** (against the same image; `--repo` takes a name or id):

  ```sh
  docker compose run --rm recogn people --repo "My people"
  docker compose run --rm recogn recognize --repo "My people" /data/people/<repo-uuid>/Yana/img_0103.jpg
  docker compose run --rm recogn enroll --repo "My people" --force
  ```

Without compose, plain Docker works too:

```sh
docker build -t recogn .
docker run -p 8080:8080 -v "$PWD/people:/data/people" \
  -v "$PWD/models:/data/models" -v "$PWD/data:/data/db" recogn
```

## Run from source

### Prerequisites

- Go 1.27.1+
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
`internal/web/dist/` — that folder is embedded into the binary. The ONNX
Runtime shared library is embedded too (`go:embed` from the main package) and
loaded at runtime: on first use it is extracted to
`~/.cache/recogn/ort/<version>/` and dlopen'd, so no external files are
needed. Deploy just the executable — models load from `models/` or
auto-download when missing.

First run: `./recogn serve --addr :8080` — with zero users the console prints
a one-time setup key (`setup key: XXXX-XXXX-XXXX-XXXX`); open the web UI,
enter the key and pick the admin username + password (the key is in-memory
only and regenerated on every start until an admin exists). Then create
repositories and users from the admin UI.

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
`third_party/onnxruntime-win/` and produces `recogn.exe`. The
`onnxruntime.dll` is embedded into the `.exe` and extracted + loaded at
runtime via `LoadLibrary`, so the `.exe` is self-contained — no external DLL
is needed. (Note: the official ORT Windows DLL depends on the Visual C++
runtime, so a bare Windows machine may still need the VC redistributable.)

To produce a self-contained zip with just the binary:

```sh
make dist-windows    # → dist/recogn-windows-x64.zip
```

Unzip on a Windows machine, open a terminal in the folder, and use
`recogn.exe` exactly like the Linux binary (e.g. `recogn.exe serve`).

## Use

### CLI

Dataset subcommands are per-repository — they take a required `--repo`
(a repository's name or id):

```sh
# Enroll a repository's dataset (incremental; --force re-embeds everything,
# --prune drops DB entries whose files are missing)
./recogn enroll --repo "My people"

# Identify every face in one or more photos
# (flags go before the image paths)
./recogn recognize --repo "My people" --json photo.jpg group.jpg

# The same, plus an annotated copy with numbered boxes + labels drawn on it:
# single image  -> --draw takes an output .jpg path
# several files -> --draw takes a directory (one <name>.annotated.jpg each)
./recogn recognize --repo "My people" --draw annotated.jpg photo.jpg
./recogn recognize --repo "My people" --draw out/ photo1.jpg photo2.jpg

# List the enrolled identities of a repository
./recogn people --repo "My people" --json

# Wrap the legacy flat dataset (root-level people/<Person>/ folders) into one
# new repository: moves the folders under people/<uuid>/, rewrites the person
# IDs to <uuid>-<slug> and renames the thumbnail sidecars
./recogn repo import --name "My people"

# Write the whole database (all repositories) to the JSON interchange file
./recogn export
```

### Web UI + API

```sh
./recogn serve --addr :8080
# open http://localhost:8080
```

After login, the web UI lets you drag-and-drop a photo to see every detected
face boxed and labelled, browse the enrolled people of the selected
repository, add a new person by uploading photos, and re-scan the
repository's folder. Unknown faces can be named right in the results
list ("Enroll" under an unknown face enrolls that specific face — matched by
its row number, which the canvas overlay also shows next to the name). The
people panel has a filter box and a threshold slider (persisted server-side).
Admins additionally manage repositories (create/rename/delete, optional
per-repo share links), users and their per-repo reader/writer roles from the
UI; every user manages their own password and personal API keys there.

**Compare two photos** (any logged-in user): the people panel has a *Compare two photos*
button that opens a face-to-face comparison tool — drop in any two photos and
it reports how similar their faces are. The largest face of each photo is
detected, aligned and embedded, and the two embeddings' **cosine similarity**
is shown with a "likely same / different person" verdict against the match
threshold. This works entirely without the enrolled-people database, so you
can e.g. check whether two `unknown` photos show the same person. The same
check can be launched straight from the enrollment modal (compare buttons on
the pending-photo thumbnails) and from the photos manager (compare buttons on
the photo tiles and a *Compare faces…* button in the photo detail view): the
first pick marks photo A, the second pick runs the comparison — both photos
always come from the photos the modal currently shows.

#### REST API

Routes are grouped by access level below: **public** (no login), **any user**
(any logged-in account), **admin**, and **reader/writer** — the per-repo role
required from the caller's grant on the repository a person belongs to.
Scripts authenticate with the session cookie or a personal API key
(`Authorization: Bearer rk_...`); a share link grants anonymous reader access
to one repository via `X-Share-Token: <repoID>/<token>`.

| Method | Endpoint | Access | Description |
|--------|----------|--------|-------------|
| `GET`  | `/api/health` | public | `{"status":"ok"}` |
| `GET`  | `/api/auth/session` | public | `{authenticated, user:{name, admin}, setup_required}` — drives the UI's login vs. setup view |
| `POST` | `/api/setup` | public (first run) | create the admin: setup key + username + password; a wrong key or a call after completion → 403 |
| `POST` | `/api/login` | public | username + password → HttpOnly `recogn_session` cookie (`POST /api/logout` clears it) |
| `GET`  | `/api/repos` | any user | the repositories the caller can access |
| `POST` | `/api/repos` | admin | create a repository (a new `people/<uuid>/` dataset folder) |
| `POST` | `/api/repos/{id}/rename` | admin | rename a repository's display name (the folder stays `<uuid>`) |
| `DELETE` | `/api/repos/{id}` | admin | delete a repository |
| `POST` | `/api/repos/{id}/share` | admin | toggle the read-only share link — JSON `{"enabled": true|false}` → `{share_url: "/share/<id>/<token>"}` (enabling mints a fresh token, i.e. rotates the link) |
| `GET` | `/api/repos/{id}/share` | admin | re-read the current share link `{share_url}` (same token — no rotation; `share_url: null` while unshared) |
| `GET`/`POST` | `/api/users` | admin | list / create users |
| `DELETE` | `/api/users/{u}` | admin | delete a user (the admin cannot be deleted) |
| `POST` | `/api/users/{u}/password` | admin | reset a user's password |
| `POST` | `/api/users/{u}/repos` | admin | set a user's per-repo grants — JSON `{"grants":[{"repo":"<name or id>","role":"reader"}]}` |
| `POST` | `/api/auth/password` | any user | change your own password |
| `GET`/`POST`/`DELETE` | `/api/auth/keys` | any user | personal API keys (created key shown once as `rk_...`; a key acts as its user) |
| `GET`  | `/api/config` | any user | read the match threshold (global) |
| `POST` | `/api/config` | admin | set the match threshold; POSTed values are persisted in the DB and survive restarts (an explicit `--threshold` flag still wins) |
| `POST` | `/api/compare` | any user | compare the faces of two photos without the identity DB — multipart `image1` + `image2` → `{similarity, threshold, image1: {count, faces: [{index, bbox, score, used}]}, image2: {…}}`; the largest face of each photo is embedded and their cosine similarity returned (`used` marks the compared face; `threshold` is the recognizer's match threshold, for the same-person verdict) |
| `GET`  | `/api/people?repo=` | reader+ | list a repository's enrolled people + photo counts (incl. `thumb` URL when a face thumbnail exists, and `aliases` when the person has metadata) |
| `GET`  | `/api/people/{name}` | reader+ | one person's **details document**: `{id, name, photos:<count>, aliases?, birthdate?, urls?, description?}` — optional metadata only, no enrolled photo paths; never-filled fields are omitted |
| `GET`  | `/api/people/{name}/photos` | reader+ | one person's enrolled photos (`thumb_src` = photo the avatar comes from) — the photos manager's payload |
| `POST` | `/api/people/{name}/meta` | writer+ | replace a person's metadata — JSON `{"aliases": [...], "birthdate": {"year","month","day"}, "urls": [...], "description": "<markdown>"}`; every field optional, omitted/empty fields clear the stored value (full-replace). Validation: aliases deduped (max 20 × 120 chars), URLs must be absolute `http(s)` (max 50 × 2048), birthdate parts are range-checked with a day requiring a month or year (max year 2100; a complete date must exist on the calendar), description max 20 000 chars |
| `POST` | `/api/people/{name}/enroll` | writer+ | add photo(s) (field `images`) for a new/existing person; each enrolled image is also saved into the person's folder |
| `POST` | `/api/people/{name}/enroll-face` | writer+ | enroll one specific face of an uploaded photo: multipart `image` + `face_index` (1-based, in the detection order `/api/recognize` reports) — used by the UI's "name this face" action on unknown results |
| `GET`  | `/api/people/{name}/photos/{path}` | writer+ | one of the person's enrolled photo files (full resolution) |
| `GET`  | `/api/people/{name}/photos/{path}/detect` | reader+ | detect + recognize faces on an enrolled photo (for the photos-manager quality check) |
| `DELETE` | `/api/people/{name}/photos/{path}` | writer+ | remove one photo: DB entry + file in the person's folder; the avatar is regenerated from another photo (or cleared) if it was the thumbnail source |
| `POST` | `/api/people/{name}/thumbnail` | writer+ | regenerate the face thumbnail from a chosen enrolled photo — JSON `{"photo": "<path>"}` |
| `POST` | `/api/people/{name}/rename` | writer+ | rename a person — JSON `{"name": "<new name>"}`; moves the person's folder, updates the DB record and the thumbnail sidecar in one step |
| `DELETE` | `/api/people/{name}` | writer+ | remove a person |
| `GET`  | `/api/thumbs/{id}.jpg` | reader+ | a person's face thumbnail (square face crop; `?v=` cache-buster follows the chosen photo) |
| `POST` | `/api/recognize?repo=` | reader+ | multipart `image` → `{count, faces:[{bbox,name,person_id,confidence,score,landmarks,matches}]}` — `matches` ranks every identity above the threshold (best per person); near-tied top scores hint at duplicate people. `?draw=1` responds with the annotated JPEG (numbered boxes + labels) instead of JSON; **400 when `repo` is missing** |
| `POST` | `/api/enroll?repo=` | writer+ | re-scan the repository's folder (incremental unless `force`); `?prune=true` also drops DB photo entries whose files are missing (CLI: `recogn enroll --repo … --prune`) |

Example:

```sh
# Log in once and reuse the cookie — or skip this by sending a personal API
# key instead: -H "Authorization: Bearer rk_..."
curl -s -c cookies.txt -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"change-me"}' http://localhost:8080/api/login
curl -s -b cookies.txt -F "image=@photo.jpg" \
  "http://localhost:8080/api/recognize?repo=My people"
curl -s -b cookies.txt -F "image1=@a.jpg" -F "image2=@b.jpg" \
  http://localhost:8080/api/compare
```

Every route requires a logged-in user (session cookie or API key) except
`/api/setup`, `/api/login`, `/api/logout`, `/api/auth/session`, `/api/health`
and any enabled share link — see
[Users and access control](#users-and-access-control). The per-repo reader /
writer role is checked against the repository the route acts on.

## Users and access control

The server **always requires login** — there is no open mode. The only
unauthenticated routes are the first-run setup view, `/api/login`,
`/api/logout`, `/api/auth/session`, `/api/health` and any enabled share link
(below).

**First run — setup key**: `recogn serve` with zero users prints a one-time
setup key to the console (`setup key: XXXX-XXXX-XXXX-XXXX`, on stdout and in
the log). Enter it in the UI's setup view together with the admin username
and password (`POST /api/setup`). The key lives only in memory and is
regenerated on every start until an admin exists; `/api/setup` answers 403
for a wrong key or once an admin has been created.

**Users and roles**: users log in with username + password and are granted a
per-repository role:

| Role | Access |
|------|--------|
| reader | browse a repository's people, details and thumbnails; recognize against it |
| writer | reader + enroll / photo management / full-resolution photos |
| admin | implicitly full access everywhere, plus user/repo/share management |

Users change their own password (`POST /api/auth/password`); the admin resets
anyone's (`POST /api/users/{u}/password`) and cannot be deleted.

**API keys**: every user can create personal API keys in the UI (`GET`/`POST`/`DELETE /api/auth/keys`; the full `rk_...` key is shown once at creation)
and revoke them there. A key acts as its user — scripts send `Authorization:
Bearer rk_...` and get that user's roles on every route.

**Share links**: the admin can enable one optional read-only link per
repository (`POST /api/repos/{id}/share {"enabled": true}` → the
`/share/<repoID>/<token>` URL). The URL can be re-read at any time
(`GET /api/repos/{id}/share`, or the "Show link" button in the Repositories
dialog) — revealing it never rotates the token; only disabling and
re-enabling does. Non-admins can neither see nor list share links — hand the
URL to the people who need it. Visiting it in the browser, or sending
`X-Share-Token: <repoID>/<token>`, grants anonymous **reader** access to that
one repository only.

Sessions are in-memory (HttpOnly `recogn_session` cookie, `SameSite=Lax`,
`Secure` over TLS): a restart logs everyone out, and `RECOGN_SESSION_TTL`
(default 24h, sliding) bounds their lifetime. Failed logins are rate-limited
per IP (10 failures / 5 min → HTTP 429 with `Retry-After`).

| Variable | Default | Purpose |
|----------|---------|---------|
| `RECOGN_SESSION_TTL` | `24h` | sliding session lifetime (Go duration); sessions are in-memory, so restarts log everyone out |
| `RECOGN_TRUSTED_PROXY_CIDR` | unset | comma-separated reverse-proxy CIDRs trusted for client-IP extraction (e.g. `10.0.0.0/8`) — gives each proxied client its own login rate-limit bucket; the proxy must append client IPs. Unset (default) keys rate limiting on the peer address and ignores `X-Forwarded-For`, so all clients behind one proxy share a bucket |

Scripts log in once and reuse the session — or send a personal API key:

```sh
# Login once, keep the cookie, call a scoped endpoint with it
curl -s -c cookies.txt -X POST http://localhost:8080/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"change-me"}'
curl -s -b cookies.txt "http://localhost:8080/api/people?repo=My people"

# Or present a personal API key (acts as its user) as a Bearer credential
curl -s -H "Authorization: Bearer rk_..." \
  "http://localhost:8080/api/people?repo=My people"
```

**Security notes**: over plain HTTP the password crosses the wire in
cleartext — put a TLS reverse proxy in front for anything remote.

## Expanding the database

People live in **repositories** — admin-created datasets, each a folder
`people/<repo-uuid>/` of person folders. To add someone new, either:

1. **Drop a folder** `people/<repo-uuid>/<Their Name>/` with a few photos and
   run `./recogn enroll --repo <name|id>` (or `POST /api/enroll?repo=`), **or**
2. **Upload photos** at runtime: `POST /api/people/<name>/enroll` (writer).
   Each successfully enrolled upload is written into the person's folder as
   `<sha1>.<ext>` (content-derived name, so re-uploading the same photo is
   idempotent) and its DB entry points at that file — a later rescan
   recognizes it as already enrolled.

**Coming from the old flat layout?** `recogn repo import --name "My people"`
wraps the legacy root-level `people/<Person>/` folders into one new
repository: it moves the folders under `people/<uuid>/`, rewrites the person
IDs to `<uuid>-<slug>` and renames the thumbnail sidecars. Run it once, then
enroll the repository as above.

**Storage**: the face database lives in a single bbolt file, `data/faces.db`
(schema v2 — a repos bucket plus every person's repository ID; person IDs are
`<repo-uuid>-<slug>`) — writes touch only the changed person/photo record (no
whole-file rewrite), startup loads it directly, and a file lock refuses two
`recogn` processes on the same DB. Users, per-repo grants and hashed API keys
live in a second bbolt file, `data/users.db`. For humans and backups there is
a JSON interchange file, `data/embeddings.json`: it is **imported
automatically whenever the store is empty** (so deleting `faces.db` and
restarting restores the last exported snapshot — the JSON is never renamed or
consumed), and `./recogn export [--out path]` **writes** the current database
— all repositories — to it. The JSON is not kept in sync by day-to-day
enrollment; run `recogn export` to refresh it. Corrupt JSON fails startup
loudly rather than silently starting empty.

**Face thumbnails**: the first enrolled photo that yields a face also produces
a square face-crop thumbnail, stored as a sidecar next to the database file
(`data/thumbs/<person-id>.jpg`) and served at `/api/thumbs/<id>.jpg` — the web
UI's people list shows it as the person's avatar. Clicking a person (their
avatar or row) opens the **details modal**: a view of who they are
(see *Person metadata* below), visible to any reader of the repository,
plus, for logged-in writers, the entry points
into managing them — **Edit details** (inline metadata form) and
**Manage photos** (the photos-manager modal, stacked on top). In the photos
manager you can add photos (upload happens immediately) or remove them;
deleting the avatar's source photo regenerates it from another photo.
Clicking a photo
runs detection and draws the found face(s) over it, so you can judge whether
it's a good enrollment shot, and re-select it as the avatar. The enroll modal
runs the same face check on every photo *before* you confirm, so a photo with
no detectable face is flagged before it's ever sent. The **Rename** button in
the same modal renames a person everywhere at once — the person's folder, the
`thumbs/<id>.jpg` sidecar and the DB
record — so recognition results, photo URLs and avatars all follow the new
name (`POST /api/people/{name}/rename` does the same from a script). Datasets
enrolled before thumbnails existed backfill automatically on the next rescan,
without re-embedding.

Photos with no detectable face are skipped with a warning, never silently
poisoning the database.

**Person metadata**: each person carries optional descriptive data — aliases,
a partial birthdate, a list of reference URLs (SSN records, articles,
interviews, …) and a free-text markdown description. The birthdate is edited
in the UI as one text field using a dashed template with `-` for unknown
slots — `YYYY-MM-DD`, `YYYY--DD` (year + day), `YYYY-MM`, `YYYY`, `-MM-DD`,
`-MM-` — so any amount of partial info fits. The details modal shows the
assembled date plus the age (exact for a full date, `≈` for partial ones,
floored to the start of the month/year when the day/month is unknown) and
the days until the next birthday when month and day are known. Metadata is
consulted in the UI by clicking a person in the list (the details modal is
visible to any reader of the repository; only the
fields that were actually filled are shown) and edited by writers through the
modal's **Edit details** form or the enroll modal's **Person details**
section. Under the hood it lives on the person's DB
record: rescans and photo changes can't lose it, renames and exports carry it
(`embeddings.json` round-trips it), and deleting the person deletes it with
the record — re-enrolling their folder starts with empty metadata. The
details document is served to readers at `GET /api/people/{name}` and written
(writer-only) via `POST /api/people/{name}/meta`.

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
