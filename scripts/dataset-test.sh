#!/usr/bin/env bash
# Dataset regression gate for the CGO inference pipeline.
#
# Runs the CGO-only dataset correctness test: every enrolled photo must detect
# an in-bounds face with a usable (unit-norm) embedding, and same-person
# embeddings must cluster tighter than across people. Requires the ONNX models
# and the people/ dataset.
set -euo pipefail
cd "$(dirname "$0")/.."

export GOPATH="$PWD/.gopath" GOMODCACHE="$PWD/.gomodcache" GOCACHE="$PWD/.gocache"
export GOFLAGS=-mod=mod GOPROXY=off CGO_ENABLED=1
export RECOGN_DATASET=1

echo "== CGO dataset pipeline correctness =="
go test ./internal/engine/ -run TestCGODatasetPipeline -v
