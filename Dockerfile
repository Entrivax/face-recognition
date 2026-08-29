# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# recogn — all-in-one image: Go app (CLI + REST API + web UI) + Python ONNX
# inference sidecar (SCRFD detector + ArcFace embedder), CPU-only.
#
# Stage 1 builds the static Go binary.
# Stage 2 downloads the ONNX models.
# Stage 3 is the slim runtime: Python for the sidecar + the Go binary.
#
# Build:  docker build -t recogn .
# Run:    docker run -p 8080:8080 -v $PWD/people:/data/people:ro recogn
# (see docker-compose.yml for the convenient form)
# ---------------------------------------------------------------------------

# ---- Stage 1: build the Go binary ---------------------------------------
FROM golang:1.26-bookworm AS gobuild
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# CGO off -> fully static binary that runs in the slim final image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/recogn .

# ---- Stage 2: fetch the ONNX models --------------------------------------
# Models are baked into the image so the container is self-contained and does
# not hit the network at runtime. Override the pack URL via build arg if you
# mirror it yourself.
FROM debian:bookworm-slim AS models
ARG BUFFALO_URL=https://github.com/deepinsight/insightface/releases/download/v0.7/buffalo_l.zip
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates unzip \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /models
RUN curl -fSL -o /tmp/buffalo_l.zip "$BUFFALO_URL" \
 && unzip -o /tmp/buffalo_l.zip det_10g.onnx w600k_r50.onnx -d /models \
 && rm /tmp/buffalo_l.zip

# ---- Stage 3: runtime -----------------------------------------------------
FROM python:3.12-slim-bookworm AS runtime

# libgl1 + libglib2.0-0 are needed by OpenCV even in headless form.
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgl1 libglib2.0-0 \
 && rm -rf /var/lib/apt/lists/*

# Python sidecar dependencies.
WORKDIR /app
COPY python/requirements.txt python/requirements.txt
RUN pip install --no-cache-dir -r python/requirements.txt

# App binary, sidecar script, models, and the baked-in web UI.
COPY --from=gobuild /out/recogn /app/recogn
COPY python/infer.py python/infer.py
COPY --from=models /models/ models/

# Default layout inside the container. Everything is overridable via env.
#   /app          app, sidecar, models
#   /data/people  the dataset (mount your people/ folder here)
#   /data/db      the generated face database (persisted via volume)
ENV RECOGN_PYTHON=python3 \
    RECOGN_SIDECAR=/app/python/infer.py \
    RECOGN_MODELS_DIR=/app/models \
    RECOGN_PEOPLE_DIR=/data/people \
    RECOGN_DATA_DIR=/data/db \
    RECOGN_ADDR=:8080 \
    RECOGN_THRESHOLD=0.45

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
# For one-off CLI commands instead, override the entrypoint args, e.g.:
#   docker run --rm -v $PWD/people:/data/people:ro recogn enroll
#   docker run --rm -v $PWD/people:/data/people:ro recogn recognize /data/people/Yana/img_0103.jpg
ENTRYPOINT ["/app/recogn"]
CMD ["serve", "--addr", ":8080"]
