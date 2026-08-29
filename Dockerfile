# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# recogn — all-in-one image: Go app (CLI + REST API + web UI) with in-process
# ONNX inference via CGO + the ONNX Runtime C API. CPU-only. No Python.
#
# Stage 1 fetches the ONNX Runtime C library + headers.
# Stage 2 downloads the ONNX models (SCRFD detector + ArcFace embedder).
# Stage 3 builds the CGO-enabled Go binary against the ORT C library.
# Stage 4 is the slim runtime: debian-slim + libonnxruntime + binary + models.
#
# Build:  docker build -t recogn .
# Run:    docker run -p 8080:8080 -v $PWD/people:/data/people:ro recogn
# (see docker-compose.yml for the convenient form)
# ---------------------------------------------------------------------------

# ---- Stage 1: ONNX Runtime C library + headers ----------------------------
FROM debian:bookworm-slim AS ort
ARG ORT_VER=1.23.2
ARG ORT_URL=https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VER}/onnxruntime-linux-x64-${ORT_VER}.tgz
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fSL -o /tmp/ort.tgz "$ORT_URL" \
 && mkdir -p /ort \
 && tar xzf /tmp/ort.tgz -C /tmp \
 && mv /tmp/onnxruntime-linux-x64-${ORT_VER}/include /ort/include \
 && mv /tmp/onnxruntime-linux-x64-${ORT_VER}/lib /ort/lib \
 && rm -rf /tmp/ort.tgz /tmp/onnxruntime-linux-x64-${ORT_VER}

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

# ---- Stage 3: build the CGO-enabled Go binary ------------------------------
FROM golang:1.26-bookworm AS gobuild
# CGO needs a C toolchain.
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src

# ORT C library + headers for the cgo build.
COPY --from=ort /ort /third_party/onnxruntime

# Cache module downloads separately from source changes.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I/third_party/onnxruntime/include" \
    CGO_LDFLAGS="-L/third_party/onnxruntime/lib -lonnxruntime -Wl,-rpath,/usr/lib/recogn"
RUN go build -trimpath -ldflags="-s -w" -o /out/recogn .

# ---- Stage 4: runtime -------------------------------------------------------
FROM debian:bookworm-slim AS runtime

# curl is used by the docker-compose healthcheck; ca-certificates for any TLS.
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# The ONNX Runtime shared library, installed on the linker path.
COPY --from=ort /ort/lib /usr/lib/recogn
RUN ldconfig /usr/lib/recogn

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
# For one-off CLI commands, override the entrypoint args, e.g.:
#   docker run --rm -v $PWD/people:/data/people:ro recogn enroll
#   docker run --rm -v $PWD/people:/data/people:ro recogn recognize /data/people/Yana/img_0103.jpg
ENTRYPOINT ["/app/recogn"]
CMD ["serve", "--addr", ":8080"]
