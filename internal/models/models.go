// Package models fetches the ONNX face models (insightface buffalo_l pack)
// into the configured models directory when they are missing from disk, so a
// freshly deployed executable can bootstrap itself on first run.
//
// The pack contains det_10g.onnx (SCRFD detector) and w600k_r50.onnx (ArcFace
// embedder) among other models; only the entries recogn needs are extracted.
// Download is best-effort and opt-out-able via RECOGN_AUTO_DOWNLOAD.
package models

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// DefaultURL is the insightface buffalo_l release pack (~289 MB).
const DefaultURL = "https://github.com/deepinsight/insightface/releases/download/v0.7/buffalo_l.zip"

// Zip entry names inside the buffalo_l pack.
const (
	detEntry = "det_10g.onnx"
	embEntry = "w600k_r50.onnx"
)

// Options controls Ensure.
type Options struct {
	// Auto enables downloading when a model file is missing. When false,
	// Ensure does nothing and the caller's CheckModels reports the missing
	// files.
	Auto bool
	// URL overrides the download source (RECOGN_MODELS_URL).
	URL string
	// Progress, when non-nil, receives periodic (done, total int64) byte counts
	// while the download runs. total may be -1 when the server does not
	// declare a Content-Length.
	Progress func(done, total int64)
	Done     func(done, total int64)
}

// Ensure makes sure the two model files exist in dir. Returns nil when they
// are already present or when opt.Auto is false (the caller's CheckModels
// reports the missing files).
func Ensure(dir, detName, embName string, opt Options) error {
	need := make(map[string]string) // target filename → zip entry name
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create models dir %s: %w", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, detName)); err != nil {
		need[detName] = detEntry
	}
	if _, err := os.Stat(filepath.Join(dir, embName)); err != nil {
		need[embName] = embEntry
	}
	if len(need) == 0 {
		return nil
	}
	if !opt.Auto {
		return nil
	}
	url := opt.URL
	if url == "" {
		url = DefaultURL
	}
	return download(dir, need, url, opt.Progress, opt.Done)
}

// download fetches the pack into a temp file inside dir and extracts the
// missing models under their configured target names.
func download(dir string, need map[string]string, url string, progress, done func(done, total int64)) error {
	tmp := filepath.Join(dir, ".pack-download.tmp")
	defer os.Remove(tmp)

	if err := fetch(tmp, url, progress, done); err != nil {
		return err
	}
	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return fmt.Errorf("open downloaded pack: %w", err)
	}
	defer zr.Close()

	byName := make(map[string]*zip.File)
	for _, f := range zr.File {
		byName[filepath.Base(f.Name)] = f
	}
	for target, entry := range need {
		f, ok := byName[entry]
		if !ok {
			return fmt.Errorf("downloaded pack does not contain %s (%d entries); "+
				"set RECOGN_MODELS_URL to a pack containing the two models", entry, len(byName))
		}
		if _, err := os.Stat(filepath.Join(dir, target)); err == nil {
			continue // appeared meanwhile; never clobber
		}
		if err := extractEntry(f, filepath.Join(dir, target)); err != nil {
			return err
		}
	}
	return nil
}

// fetch streams the pack URL into dst on disk. Content-Length may be -1 when
// the server does not declare a Content-Length.
func fetch(dst, url string, progress, done func(done, total int64)) error {
	resp, err := http.Get(url) //nolint:gosec // URL comes from config/env set by the operator
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected HTTP status %s", url, resp.Status)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	var pw io.Writer = out
	if progress != nil {
		pw = &progressWriter{total: resp.ContentLength, fn: progress, writer: out}
	}
	var written int64
	if written, err = io.Copy(pw, resp.Body); err != nil {
		out.Close()
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	if done != nil {
		done(written, written)
	}
	return nil
}

// extractEntry writes one zip entry to target via a temp file + rename.
func extractEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()
	tmp := target + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copy %s: %w", target, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// progressWriter counts bytes copied and fires the callback at a coarse
// granularity so long downloads show liveness without flooding output.
type progressWriter struct {
	total   int64
	n       int64
	printed int64
	writer  io.Writer
	fn      func(done, total int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.writer.Write(p)
	w.n += int64(len(p))
	if w.fn != nil && w.n-w.printed >= 32<<20 {
		w.printed = w.n
		w.fn(w.n, w.total)
	}
	return len(p), nil
}
