//go:build linux && !noembed

package main

import _ "embed"

// The ONNX Runtime shared library, embedded into the executable so the binary
// deploys as a single file. At startup the first Open call extracts it to a
// per-version cache dir and loads it via dlopen (see internal/onnxrt/embed.go).
//
// Keep ortLibVersion in sync with ORT_VER in the Makefile and the Dockerfile's
// ORT_VER build arg — the embedded file name below carries the version.
//
//go:embed third_party/onnxruntime/lib/libonnxruntime.so.1.23.2
var ortLibData []byte

const (
	ortLibVersion = "1.23.2"
	ortLibFile    = "libonnxruntime.so.1.23.2"
)
