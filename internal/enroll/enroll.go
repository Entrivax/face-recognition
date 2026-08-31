// Package enroll scans the people/ dataset folder and populates the face
// database. One sub-folder per person; each image in it contributes one face
// embedding (the largest detected face). Enrollment is incremental: unchanged
// files (matched by content hash) are skipped, so re-running enroll after
// adding photos is cheap.
package enroll

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"recogn/internal/db"
	"recogn/internal/engine"
)

// Result summarises one enrollment run for reporting.
type Result struct {
	PeopleSeen   int
	PhotosAdded  int
	PhotosKept   int
	PhotosFailed int
	// PhotosPruned counts DB photo entries dropped because their file was
	// missing from the people folder (only when Options.Prune is set).
	PhotosPruned int
	Skipped      []SkippedPhoto
}

// SkippedPhoto records an image that produced no usable face.
type SkippedPhoto struct {
	Person string
	Path   string
	Reason string
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true,
	".webp": true, ".bmp": true, ".gif": true,
}

// Options controls an enrollment run.
type Options struct {
	PeopleDir string
	Force     bool // re-embed everything, ignoring content hashes
	// Prune drops DB photo entries whose file no longer exists in the
	// person's folder (and, when a person's folder disappeared entirely,
	// all of their photo entries). Opt-in: datasets may be temporarily
	// unmounted, so staleness is never cleaned up automatically.
	Prune    bool
	Progress func(person, file string, idx, total int)
}

// FaceEngine is the subset of the recognition engine that enrollment needs.
// *engine.Engine satisfies it; tests provide stubs.
type FaceEngine interface {
	Detect(imgBytes []byte) ([]engine.Face, error)
	EmbedFace(imgBytes []byte, f engine.Face) ([]float32, error)
}

// Scan walks peopleDir, embeds each new/changed image, and stores results in
// the database. The engine must already have its identity set loaded if you
// want matching afterwards; this only writes the DB.
func Scan(eng FaceEngine, database *db.DB, opts Options) (Result, error) {
	var res Result
	entries, err := os.ReadDir(opts.PeopleDir)
	if err != nil {
		return res, fmt.Errorf("read people dir %s: %w", opts.PeopleDir, err)
	}

	// Collect person folders (directories only).
	var personDirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			personDirs = append(personDirs, e)
		}
	}
	sort.Slice(personDirs, func(i, j int) bool { return personDirs[i].Name() < personDirs[j].Name() })

	for _, pd := range personDirs {
		name := pd.Name()
		folder := filepath.Join(opts.PeopleDir, name)
		files, err := listImages(folder)
		if err != nil {
			res.Skipped = append(res.Skipped, SkippedPhoto{name, folder, err.Error()})
			continue
		}
		res.PeopleSeen++
		if opts.Prune {
			res.PhotosPruned += pruneMissing(database, name, files)
		}
		for i, f := range files {
			if opts.Progress != nil {
				opts.Progress(name, f, i+1, len(files))
			}
			added, reason, err := enrollOne(eng, database, name, folder, f, opts.Force)
			switch {
			case err != nil:
				res.PhotosFailed++
				res.Skipped = append(res.Skipped, SkippedPhoto{name, f, err.Error()})
			case added:
				res.PhotosAdded++
			default:
				if reason != "" {
					res.PhotosFailed++
					res.Skipped = append(res.Skipped, SkippedPhoto{name, f, reason})
				} else {
					res.PhotosKept++ // unchanged, skipped by hash
				}
			}
		}
	}
	if opts.Prune {
		// People whose dataset folder disappeared entirely never appear in
		// the loop above; drop their photo entries too (the person stays
		// enrolled, with zero photos).
		res.PhotosPruned += pruneMissingPeople(database, opts.PeopleDir)
	}
	return res, nil
}

// pruneMissing removes DB photo entries of the named person whose file is not
// among the folder's listed images. Returns how many entries were removed.
func pruneMissing(database *db.DB, name string, files []string) int {
	p := database.Get(name)
	if p == nil || len(p.Photos) == 0 {
		return 0
	}
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f] = true
	}
	n := 0
	// Snapshot the paths: RemovePhoto mutates the person's photo slice.
	paths := make([]string, 0, len(p.Photos))
	for _, ph := range p.Photos {
		paths = append(paths, ph.Path)
	}
	for _, path := range paths {
		if !present[path] {
			if removed, err := database.RemovePhoto(name, path); err == nil && removed {
				n++
			}
		}
	}
	return n
}

// pruneMissingPeople removes every DB photo entry for people whose folder is
// entirely gone from the people dir. Returns the number of removed entries.
func pruneMissingPeople(database *db.DB, peopleDir string) int {
	n := 0
	for _, p := range database.People() {
		folder := filepath.Join(peopleDir, p.Name)
		if _, err := os.Stat(folder); !errors.Is(err, os.ErrNotExist) {
			continue // folder exists (or stat failed) — leave it alone
		}
		for _, ph := range p.Photos {
			if removed, err := database.RemovePhoto(p.Name, ph.Path); err == nil && removed {
				n++
			}
		}
	}
	return n
}

// ThumbSize is the pixel size (square) of the generated face thumbnails.
const ThumbSize = 160

// saveThumb stores a face thumbnail for the person if they do not have one
// yet (first enrollment wins). Best-effort: thumbnail problems must never
// fail an enrollment.
func saveThumb(database *db.DB, name, photoPath string, imgBytes []byte, f engine.Face) {
	p := database.Get(name)
	if p == nil || p.Thumb != "" {
		return // person unknown, or thumbnail already set
	}
	jpg, err := engine.FaceThumb(imgBytes, f, ThumbSize)
	if err != nil {
		return // best-effort
	}
	_ = database.SetThumbnail(p.ID, jpg, photoPath)
}

// thumbPending reports whether the named person exists but has no face
// thumbnail yet (drives the one-time backfill during folder scans).
func thumbPending(database *db.DB, name string) bool {
	p := database.Get(name)
	return p != nil && p.Thumb == ""
}

// enrollOne processes a single image. It returns (added, skipReason, err).
// added=false with empty reason means the file was unchanged (hash match).
// Note: DB photo paths are basenames relative to the person's folder, so
// PhotoHash is keyed by the bare file name.
func enrollOne(eng FaceEngine, database *db.DB, name, folder, file string, force bool) (bool, string, error) {
	full := filepath.Join(folder, file)
	b, err := os.ReadFile(full)
	if err != nil {
		return false, "", err
	}
	h := db.HashBytes(b)
	unchanged := !force && database.PhotoHash(name, file) == h
	if unchanged && !thumbPending(database, name) {
		return false, "", nil // unchanged
	}

	faces, err := eng.Detect(b)
	if err != nil {
		return false, "", fmt.Errorf("detect: %w", err)
	}
	if len(faces) == 0 {
		return false, "no face detected", nil
	}
	// Use the largest face for enrollment (most reliable signal).
	best, _ := engine.LargestFace(faces)
	if unchanged {
		// The photo is unchanged, but the person still lacks a thumbnail:
		// crop only — no re-embedding, no DB write.
		saveThumb(database, name, file, b, best)
		return false, "", nil
	}
	emb, err := eng.EmbedFace(b, best)
	if err != nil {
		return false, "", fmt.Errorf("embed: %w", err)
	}
	if err := database.AddPhotoHashed(name, file, h, emb); err != nil {
		return false, "", err
	}
	saveThumb(database, name, file, b, best)
	return true, "", nil
}

// EnrollBytes embeds a single uploaded image for a person, stores the
// embedding in the database, and writes the original image bytes into the
// people folder under the person's name, so runtime enrollments become part
// of the dataset (a later Scan treats them like any other photo).
//
// The file is saved as peopleDir/<Person>/<sha1[:12]><ext> — content-derived,
// so re-uploading the same image is idempotent — and the DB photo path is the
// same basename a folder rescan would derive for it, keeping rescans
// duplicate-free.
func EnrollBytes(eng FaceEngine, database *db.DB, peopleDir, name, fileName string, imgBytes []byte) (string, error) {
	faces, err := eng.Detect(imgBytes)
	if err != nil {
		return "", fmt.Errorf("detect: %w", err)
	}
	if len(faces) == 0 {
		return "", fmt.Errorf("no face detected in %s", fileName)
	}
	best, _ := engine.LargestFace(faces)
	emb, err := eng.EmbedFace(imgBytes, best)
	if err != nil {
		return "", fmt.Errorf("embed: %w", err)
	}
	name = strings.TrimSpace(name)
	if err := CheckName(name); err != nil {
		return "", err
	}
	if peopleDir == "" {
		return "", fmt.Errorf("people folder not configured; cannot save %s", fileName)
	}
	h := db.HashBytes(imgBytes)
	ext := imageExt(imgBytes) // content-derived, so re-uploads are idempotent
	// Use the DB-canonical person name so enrolling "alice" when "Alice"
	// exists lands in the existing folder instead of a case-duplicate one.
	folder := name
	if p := database.Get(name); p != nil {
		folder = p.Name
	}
	fileName = h[:12] + ext
	full := filepath.Join(peopleDir, folder, fileName)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("create people folder: %w", err)
	}
	if err := os.WriteFile(full, imgBytes, 0o644); err != nil {
		return "", fmt.Errorf("write to people folder: %w", err)
	}
	if err := database.AddPhotoHashed(name, fileName, h, emb); err != nil {
		// The image is already on disk; leave it. The next folder rescan will
		// enroll it, so the dataset self-heals.
		return "", err
	}
	saveThumb(database, name, fileName, imgBytes, best)
	return filepath.Join(folder, fileName), nil
}

// CheckName validates a person name that will also be used as a folder name
// under the people directory. Spaces and unicode are fine (dataset folders
// use them); path-like or control-character names are not.
func CheckName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return fmt.Errorf("person name is empty")
	}
	if n == "." || n == ".." {
		return fmt.Errorf("invalid person name %q", n)
	}
	if strings.ContainsAny(n, `/\`) || strings.ContainsRune(n, os.PathSeparator) {
		return fmt.Errorf("invalid person name %q: path separators not allowed", n)
	}
	if strings.HasPrefix(n, ".") {
		return fmt.Errorf("invalid person name %q: leading dot not allowed", n)
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid person name %q: control characters not allowed", n)
		}
	}
	return nil
}

// imageExt returns the file extension for imgBytes' actual format (decided by
// sniffing the decoded header, not by the upload's original name, so the saved
// name is a pure function of the content). Falls back to .jpg when the format
// cannot be sniffed.
func imageExt(imgBytes []byte) string {
	if _, format, err := image.DecodeConfig(bytes.NewReader(imgBytes)); err == nil {
		switch format {
		case "jpeg":
			return ".jpg"
		case "png":
			return ".png"
		case "gif":
			return ".gif"
		case "bmp":
			return ".bmp"
		case "webp":
			return ".webp"
		case "tiff":
			return ".tiff"
		}
	}
	return ".jpg"
}

// listImages returns image file names (not full paths) directly under dir.
func listImages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if imageExts[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
