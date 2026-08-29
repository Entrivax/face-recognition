# recogn — build helpers.
# The sandbox/environment needs the Go caches to live inside the workspace,
# so we export them for every go invocation.

export GOPATH     := $(CURDIR)/.gopath
export GOMODCACHE := $(CURDIR)/.gomodcache
export GOCACHE    := $(CURDIR)/.gocache
export GOFLAGS    := -mod=mod
export GOPROXY    := off
export CGO_ENABLED := 1

BINARY  := recogn
ORT_DIR := third_party/onnxruntime
ORT_VER := 1.23.2
ORT_TGZ := onnxruntime-linux-x64-$(ORT_VER).tgz
ORT_URL := https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VER)/$(ORT_TGZ)

# CGO include/lib paths for the ONNX Runtime C API.
export CGO_CFLAGS  := -I$(CURDIR)/$(ORT_DIR)/include
export CGO_LDFLAGS := -L$(CURDIR)/$(ORT_DIR)/lib -lonnxruntime -Wl,-rpath,$(CURDIR)/$(ORT_DIR)/lib

.PHONY: all build test vet enroll serve clean models ort dataset-test

all: build

# The CGO backend needs the ONNX Runtime C library present first.
build: ort
	go build -o $(BINARY) .

test: ort
	go test ./...

vet:
	go vet ./...

# Fetch the ONNX Runtime C library + headers (for the CGO backend).
ort:
	@if [ ! -f $(ORT_DIR)/lib/libonnxruntime.so ]; then \
	  echo "Downloading ONNX Runtime C $(ORT_VER)..."; \
	  mkdir -p $(ORT_DIR) tmp; \
	  curl -fSL -o tmp/$(ORT_TGZ) $(ORT_URL); \
	  tar xzf tmp/$(ORT_TGZ) -C tmp; \
	  mv tmp/onnxruntime-linux-x64-$(ORT_VER)/include $(ORT_DIR)/include; \
	  mv tmp/onnxruntime-linux-x64-$(ORT_VER)/lib $(ORT_DIR)/lib; \
	  rm -rf tmp; \
	  echo "ORT C ready in $(ORT_DIR)"; \
	fi

# Dataset regression gate: CGO pipeline correctness over people/.
dataset-test: ort
	./scripts/dataset-test.sh

# Build, then enroll the people/ dataset into data/embeddings.json.
enroll: build
	./$(BINARY) enroll

# Build, then start the API + web UI on :8080.
serve: build
	./$(BINARY) serve --addr :8080

# Download the SCRFD + ArcFace ONNX models into ./models (first run only).
models:
	@mkdir -p models tmp
	@echo "Downloading insightface buffalo_l pack (~289 MB)..."
	@curl -L -o tmp/buffalo_l.zip \
	  https://github.com/deepinsight/insightface/releases/download/v0.7/buffalo_l.zip
	@unzip -o tmp/buffalo_l.zip det_10g.onnx w600k_r50.onnx -d models
	@rm -rf tmp
	@echo "Models ready in ./models"

clean:
	rm -f $(BINARY)
	rm -rf .gocache
