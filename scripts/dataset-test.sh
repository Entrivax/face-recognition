#!/bin/sh
# Dataset regression gate: CGO pipeline correctness over the people/ dataset
# (detection + embedding on every photo, batched-embed parity). Needs the ONNX
# models in ./models and the ORT C header in third_party/onnxruntime —
# `make dataset-test` fetches that first.
set -e
cd "$(dirname "$0")/.."

# Same cache/CGO environment the Makefile exports for every go invocation.
export GOPATH="$PWD/.gopath"
export GOMODCACHE="$PWD/.gomodcache"
export GOCACHE="$PWD/.gocache"
export GOFLAGS=-mod=mod
export GOPROXY=off
export CGO_ENABLED=1
export CGO_CFLAGS="-I$PWD/third_party/onnxruntime/include"
export CGO_LDFLAGS="-ldl"

RECOGN_DATASET=1 go test ./internal/engine/ \
  -run 'TestCGODatasetPipeline|TestCGOBatchedEmbedParity' -count=1 -v
