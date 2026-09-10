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

# CGO include path for the ONNX Runtime C API. The shared library is NOT
# linked at build time anymore — it is embedded into the executable and
# loaded at runtime via dlopen/LoadLibrary (see internal/onnxrt/embed.go).
# -ldl covers dlopen on glibc < 2.34.
export CGO_CFLAGS  := -I$(CURDIR)/$(ORT_DIR)/include
export CGO_LDFLAGS := -ldl

.PHONY: all build build-windows test test-race vet enroll serve clean models ort ort-win ui dataset-test

all: build

# Build the web UI (Preact + TypeScript via Vite) into internal/web/dist,
# which the Go binary embeds. Needs Node >= 20; skipped when node_modules
# is already populated the same way `ort` is skipped once fetched.
ui:
	@if [ ! -d web/node_modules ]; then \
	  echo "Installing web UI dependencies..."; \
	  npm --prefix web ci; \
	fi
	npm --prefix web run build

# The CGO backend needs the ONNX Runtime C library present first.
build: ort ui
	go build -o $(BINARY) .

# Cross-compile a Windows binary (recogn.exe). Requires mingw-w64:
#   Debian/Ubuntu : sudo apt install gcc-mingw-w64-x86-64
#   Fedora        : sudo dnf install mingw64-gcc
#   macOS (brew)  : brew install mingw-w64
# The resulting recogn.exe needs onnxruntime.dll next to it (or on PATH).
# `make dist-windows` bundles everything into a zip.
build-windows: ort-win ui
	GOOS=windows GOARCH=amd64 \
	CC=$(WIN_CC) \
	CGO_CFLAGS="-I$(CURDIR)/$(ORT_WIN_DIR)/include" \
	CGO_LDFLAGS="" \
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

# Bundle the self-contained Windows binary into a distributable zip.
# ORT is embedded, so no DLL or models folder is needed.
dist-windows: build-windows
	@mkdir -p dist/recogn-windows
	cp $(WIN_BINARY) dist/recogn-windows/
	cd dist && zip -r recogn-windows-x64.zip recogn-windows
	@echo "Distributable: dist/recogn-windows-x64.zip"

test: ort
	go test ./...

# Test suite under the race detector — the concurrency gate's thread-safety
# gate (TestConcurrentRunParity) and the parallel scans only mean anything
# with the race detector on, so run this after changing anything parallel.
test-race: ort
	go test -race ./...

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

# Build, then enroll the people/ dataset into data/faces.db.
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
	rm -rf .gocache dist web/node_modules
