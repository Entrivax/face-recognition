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

# --- Linux (host) ORT -------------------------------------------------------
ORT_TGZ := onnxruntime-linux-x64-$(ORT_VER).tgz
ORT_URL := https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VER)/$(ORT_TGZ)

# --- Windows (cross-compile) ORT --------------------------------------------
ORT_WIN_ZIP := onnxruntime-win-x64-$(ORT_VER).zip
ORT_WIN_URL := https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VER)/$(ORT_WIN_ZIP)
ORT_WIN_DIR := third_party/onnxruntime-win
WIN_BINARY  := recogn.exe
WIN_CC      := x86_64-w64-mingw32-gcc

# CGO include/lib paths for the ONNX Runtime C API (Linux host).
export CGO_CFLAGS  := -I$(CURDIR)/$(ORT_DIR)/include
export CGO_LDFLAGS := -L$(CURDIR)/$(ORT_DIR)/lib -lonnxruntime -Wl,-rpath,$(CURDIR)/$(ORT_DIR)/lib

.PHONY: all build build-windows test vet enroll serve clean models ort ort-win dataset-test

all: build

# The CGO backend needs the ONNX Runtime C library present first.
build: ort
	go build -o $(BINARY) .

# Cross-compile a Windows binary (recogn.exe). Requires mingw-w64:
#   Debian/Ubuntu : sudo apt install gcc-mingw-w64-x86-64
#   Fedora        : sudo dnf install mingw64-gcc
#   macOS (brew)  : brew install mingw-w64
# The resulting recogn.exe needs onnxruntime.dll next to it (or on PATH).
# `make dist-windows` bundles everything into a zip.
build-windows: ort-win
	GOOS=windows GOARCH=amd64 \
	CC=$(WIN_CC) \
	CGO_CFLAGS="-I$(CURDIR)/$(ORT_WIN_DIR)/include" \
	CGO_LDFLAGS="-L$(CURDIR)/$(ORT_WIN_DIR)/lib -lonnxruntime" \
	go build -o $(WIN_BINARY) .

# Fetch the Windows ONNX Runtime C library + headers (for cross-compilation).
ort-win:
	@if [ ! -f $(ORT_WIN_DIR)/lib/onnxruntime.dll ]; then \
	  echo "Downloading ONNX Runtime C $(ORT_VER) for Windows..."; \
	  mkdir -p $(ORT_WIN_DIR) tmp; \
	  curl -fSL -o tmp/$(ORT_WIN_ZIP) $(ORT_WIN_URL); \
	  unzip -o tmp/$(ORT_WIN_ZIP) -d tmp; \
	  mv tmp/onnxruntime-win-x64-$(ORT_VER)/include $(ORT_WIN_DIR)/include; \
	  mv tmp/onnxruntime-win-x64-$(ORT_VER)/lib $(ORT_WIN_DIR)/lib; \
	  rm -rf tmp; \
	  echo "Windows ORT C ready in $(ORT_WIN_DIR)"; \
	fi

# Bundle the Windows binary + DLL + models into a distributable zip.
dist-windows: build-windows models
	@mkdir -p dist/recogn-windows
	cp $(WIN_BINARY) dist/recogn-windows/
	cp $(ORT_WIN_DIR)/lib/onnxruntime.dll dist/recogn-windows/
	cp -r models dist/recogn-windows/models
	cd dist && zip -r recogn-windows-x64.zip recogn-windows
	@echo "Distributable: dist/recogn-windows-x64.zip"

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
	rm -f $(BINARY) $(WIN_BINARY)
	rm -rf .gocache dist
