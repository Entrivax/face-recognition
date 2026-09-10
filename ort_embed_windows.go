//go:build windows && !noembed

package main

import _ "embed"

// The ONNX Runtime shared library (Windows build), embedded into the
// executable so the .exe deploys as a single file. At startup the first Open
// call extracts it to a per-version cache dir and loads it via LoadLibrary
// (see internal/onnxrt/embed.go).
//
// Keep ortLibVersion in sync with ORT_VER in the Makefile.
//
//go:embed third_party/onnxruntime-win/lib/onnxruntime.dll
var ortLibData []byte

const (
	ortLibVersion = "1.23.2"
	ortLibFile    = "onnxruntime.dll"
)
