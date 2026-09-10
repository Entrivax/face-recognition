//go:build noembed || !(linux || windows)

package main

// Built with `-tags noembed` (or on an unsupported platform): nothing is
// embedded. The ORT shared library is then located via the fallback chain in
// internal/onnxrt/embed.go — the RECOGN_ORT_LIBRARY env var, the system
// loader search path, or the repository's third_party layout.
var ortLibData []byte

const (
	ortLibVersion = ""
	ortLibFile    = ""
)
