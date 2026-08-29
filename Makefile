# recogn — build helpers.
# The sandbox/environment needs the Go caches to live inside the workspace,
# so we export them for every go invocation.

export GOPATH     := $(CURDIR)/.gopath
export GOMODCACHE := $(CURDIR)/.gomodcache
export GOCACHE    := $(CURDIR)/.gocache
export GOFLAGS    := -mod=mod
export GOPROXY    := off

BINARY := recogn

.PHONY: all build test vet enroll serve clean models

all: build

build:
	go build -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

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
