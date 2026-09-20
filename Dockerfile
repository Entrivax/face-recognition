# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# recogn — all-in-one image: Go app (CLI + REST API + web UI) with in-process
# ONNX inference via CGO + the ONNX Runtime C API. CPU-only. No Python.
#
# Stage 1 fetches the ONNX Runtime C library + headers (used at build time to
# compile the CGO shim and embed the library).
# Stage 2 downloads the ONNX models (SCRFD detector + ArcFace embedder).
# Stage 3 builds the web UI (Preact + TypeScript) with Vite.
# Stage 4 builds the CGO-enabled Go binary with the ORT library embedded.
# Stage 5 is the slim runtime: debian-slim + self-contained binary + models.
#
# Build:  docker build -t recogn .
# Run:    docker run -p 8080:8080 -v "$PWD/people:/data/people" recogn
# (see docker-compose.yml for the convenient form; the people mount must be
# writable so photos enrolled through the API/UI are saved back into it)
# ---------------------------------------------------------------------------

# ---- Stage 1: ONNX Runtime C library + headers ----------------------------
# Reuses the repo's `make ort` recipe instead of duplicating the
# download/extract steps here; the ORT version is pinned in the Makefile.
# (tar + gzip ship with debian-slim; only make/curl/ca-certificates are added.)
FROM debian:bookworm-slim AS ort
RUN apt-get update \
 && apt-get install -y --no-install-recommends make curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /ort
COPY Makefile .
RUN make ort

# ---- Stage 2: ONNX models --------------------------------------------------
# Baked into the image so the container is self-contained (no runtime
# download). Override the pack URL via build arg if you mirror it yourself.
FROM debian:bookworm-slim AS models
ARG BUFFALO_URL=https://github.com/deepinsight/insightface/releases/download/v0.7/buffalo_l.zip
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates unzip \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /models
RUN curl -fSL -o /tmp/buffalo_l.zip "$BUFFALO_URL" \
 && unzip -o /tmp/buffalo_l.zip det_10g.onnx w600k_r50.onnx -d /models \
 && rm /tmp/buffalo_l.zip

# ---- Stage 3: build the web UI (Preact + TypeScript → Vite bundle) ---------
FROM node:22-bookworm-slim AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# Emits to /src/internal/web/dist (outDir ../internal/web/dist).
RUN npm run build

# ---- Stage 4: build the CGO-enabled Go binary ------------------------------
FROM golang:1.27.1-bookworm AS gobuild
# CGO needs a C toolchain.
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src

# ORT C library + headers for the cgo build, inside the module tree so the
# go:embed pattern in ort_embed_linux.go (relative to /src) matches and the
# #cgo ${SRCDIR} include path resolves.
COPY --from=ort /ort/third_party/onnxruntime /src/third_party/onnxruntime

# Cache module downloads separately from source changes.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# The Vite-built UI bundle (dist/ is gitignored; Docker always builds its own).
COPY --from=ui /src/internal/web/dist ./internal/web/dist

ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I/src/third_party/onnxruntime/include" \
    CGO_LDFLAGS="-ldl"
RUN go build -trimpath -ldflags="-s -w" -o /out/recogn .

# ---- Stage 5: runtime -------------------------------------------------------
FROM debian:bookworm-slim AS runtime

# curl is used by the docker-compose healthcheck; ca-certificates for any TLS.
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# The ONNX Runtime shared library is embedded in the binary (go:embed) and
# loaded at runtime, so no system install is needed.

WORKDIR /app
COPY --from=gobuild /out/recogn /app/recogn
COPY --from=models /models/ models/

# Default layout inside the container. Everything is overridable via env.
#   /app          app + models
#   /data/people  the dataset (mount your people/ folder here)
#   /data/db      the generated face database (persisted via volume)
ENV RECOGN_MODELS_DIR=/app/models \
    RECOGN_PEOPLE_DIR=/data/people \
    RECOGN_DATA_DIR=/data/db \
    RECOGN_ADDR=:8080 \
    RECOGN_THRESHOLD=0.45 \
    # Keep the ORT extraction cache out of the /data volume.
    XDG_CACHE_HOME=/tmp/.cache

RUN mkdir -p /data/people /data/db

# Drop privileges.
RUN useradd --system --uid 10001 --home /data recogn \
 && chown -R recogn:recogn /data /app
USER recogn

EXPOSE 8080

# Persist the generated face database across container restarts.
VOLUME ["/data/db"]

# Default command: start the API + web UI. On first run, if the DB is empty
# and a people/ dataset is mounted, the server auto-enrolls before serving.
#
# For one-off CLI commands, override the entrypoint args, e.g.:
#   docker run --rm -v $PWD/people:/data/people recogn enroll
#   docker run --rm -v $PWD/people:/data/people recogn recognize /data/people/Yana/img_0103.jpg
ENTRYPOINT ["/app/recogn"]
CMD ["serve", "--addr", ":8080"]
