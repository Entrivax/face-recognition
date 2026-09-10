// Runtime-library loading for the onnxrt shim: this file owns getting the
// ONNX Runtime shared library loaded exactly once per process.
//
// Single-file deployment: the main package embeds the ORT shared library
// (go:embed) and registers it here via SetRuntimeLibrary. The first Open then
// extracts it to a per-version user-cache dir and loads it with
// dlopen (POSIX) / LoadLibrary (Windows) — the C side only ever resolves the
// single OrtGetApiBase symbol, so no link-time dependency on the library
// remains.
//
// Builds/tests that never register an embedded copy (plain `go test`, or
// `-tags noembed` builds) fall back to, in order: the RECOGN_ORT_LIBRARY env
// override, a bare soname resolved through the normal loader search, the
// repository's third_party layout relative to the current directory, and
// relative to this package's source tree.
package onnxrt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var (
	rtMu       sync.Mutex
	rtLoaded   bool   // set once ort_runtime_load has succeeded
	rtVersion  string // ORT release the embedded library was built from
	rtFilename string // canonical file name of the embedded library
	rtData     []byte // embedded library bytes (nil = not embedded)
)

// SetRuntimeLibrary registers the ORT shared library compiled into the
// executable, so the first Open can extract and load it. Call it once before
// any Open (main does, right before building the engine); calls after the
// runtime is already loaded are no-ops. With data == nil it does nothing,
// which activates the fallback search in ensureRuntime.
func SetRuntimeLibrary(version, filename string, data []byte) {
	if filename == "" || len(data) == 0 {
		return
	}
	rtMu.Lock()
	defer rtMu.Unlock()
	rtVersion, rtFilename, rtData = version, filename, data
}

// ensureRuntime loads the ORT shared library at most once, trying candidates
// in order until one loads. Open calls it before every session creation; the
// work happens only on the first call.
func ensureRuntime() error {
	rtMu.Lock()
	defer rtMu.Unlock()
	if rtLoaded {
		return nil
	}

	var tried []string
	try := func(source, path string) bool {
		tried = append(tried, source+"="+path)
		return loadLibrary(path) == nil
	}

	// 1. Explicit override wins; a failing explicit path is a hard error.
	if p := os.Getenv("RECOGN_ORT_LIBRARY"); p != "" {
		if !try("RECOGN_ORT_LIBRARY", p) {
			return fmt.Errorf("load onnxruntime library from RECOGN_ORT_LIBRARY=%s: %w", p, loadErr())
		}
		rtLoaded = true
		return nil
	}

	// 2. The library embedded in this executable: extract to a per-version
	// cache dir, then dlopen it from there. On a noexec cache mount the
	// extraction falls back to a temp dir.
	if rtFilename != "" && len(rtData) > 0 {
		path, err := extractLibrary(rtVersion, rtFilename, rtData)
		if err != nil {
			return fmt.Errorf("extract embedded %s: %w", rtFilename, err)
		}
		if !try("embedded", path) {
			return fmt.Errorf("load extracted onnxruntime library %s: %w "+
				"(is the cache directory mounted noexec? set RECOGN_ORT_LIBRARY to a loadable copy)",
				path, loadErr())
		}
		rtLoaded = true
		return nil
	}

	// 3-5. Builds/tests without an embedded library.
	for _, c := range fallbackCandidates() {
		if try(c.source, c.path) {
			rtLoaded = true
			return nil
		}
	}

	return fmt.Errorf("cannot load the onnxruntime shared library (tried: %s) — "+
		"point RECOGN_ORT_LIBRARY at libonnxruntime.so.1 / onnxruntime.dll, "+
		"or install it on the system loader path", strings.Join(tried, ", "))
}

// libCandidate is one place to look for the ORT shared library.
type libCandidate struct {
	source string // human-readable origin, used in error messages
	path   string
}

// fallbackCandidates lists where to find the library when nothing is embedded
// (tests, or builds with the `noembed` tag). Ordered most-explicit first.
func fallbackCandidates() []libCandidate {
	var out []libCandidate
	// Bare soname → normal loader search (LD_LIBRARY_PATH, ldconfig, PATH).
	out = append(out, libCandidate{source: "system soname", path: defaultSoname()})
	// Repository layout relative to the current directory (make test).
	out = append(out, libCandidate{source: "cwd-relative",
		path: filepath.Join(repoLibDir(), repoLibFile())})
	// Repository layout relative to this package's source tree, so
	// `go test ./internal/onnxrt` works from any working directory. Under
	// -trimpath the recorded path is not absolute and the candidate harmlessly
	// fails to load.
	if _, file, _, ok := runtime.Caller(0); ok {
		out = append(out, libCandidate{source: "source-relative",
			path: filepath.Join(filepath.Dir(file), "..", "..", repoLibDir(), repoLibFile())})
	}
	return out
}

func defaultSoname() string {
	if runtime.GOOS == "windows" {
		return "onnxruntime.dll"
	}
	return "libonnxruntime.so.1"
}

// repoLibDir is the third_party location `make ort` / `make ort-win` populate.
func repoLibDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("third_party", "onnxruntime-win", "lib")
	}
	return filepath.Join("third_party", "onnxruntime", "lib")
}

func repoLibFile() string {
	if runtime.GOOS == "windows" {
		return "onnxruntime.dll"
	}
	return "libonnxruntime.so.1.23.2"
}

// extractLibrary makes the ORT shared library available as a real file and
// returns its path. The first writable candidate dir wins:
//
//	<UserCacheDir>/recogn/ort/<version>/<file>   (~/.cache on Linux,
//	                                              %LocalAppData% on Windows)
//	<TempDir>/recogn-ort/<version>/<file>        (read-only $HOME, noexec mounts)
func extractLibrary(version, filename string, data []byte) (string, error) {
	var dirs []string
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		dirs = append(dirs, filepath.Join(cache, "recogn", "ort", version))
	}
	dirs = append(dirs, filepath.Join(os.TempDir(), "recogn-ort", version))

	var errs []error
	for _, dir := range dirs {
		path, err := extractTo(dir, filename, data)
		if err == nil {
			return path, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", dir, err))
	}
	return "", errors.Join(errs...)
}

// extractTo writes data into dir/filename if it is not already there with the
// right size. The write is atomic (temp file + rename), so concurrent
// processes starting on the same cache dir are safe; a copy already loaded by
// another process stays valid on POSIX (rename replaces the name, not the
// inode).
func extractTo(dir, filename string, data []byte) (string, error) {
	target := filepath.Join(dir, filename)
	if fi, err := os.Stat(target); err == nil && fi.Size() == int64(len(data)) {
		return target, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d", filename, os.Getpid()))
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil { // WriteFile's mode goes through umask
		os.Remove(tmp)
		return "", err
	}
	if runtime.GOOS == "windows" {
		// os.Rename does not replace an existing file on Windows.
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			os.Remove(tmp)
			return "", err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return target, nil
}
