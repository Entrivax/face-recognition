// Package enroll scans the people/ dataset folder and populates the face
// database. One sub-folder per person; each image in it contributes one face
// embedding (the largest detected face). Enrollment is incremental: unchanged
// files (matched by content hash) are skipped, so re-running enroll after
// adding photos is cheap.
package enroll

import (
	"fmt"
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
	Progress  func(person, file string, idx, total int)
}

// Scan walks peopleDir, embeds each new/changed image, and stores results in
// the database. The engine must already have its identity set loaded if you
// want matching afterwards; this only writes the DB.
func Scan(eng *engine.Engine, database *db.DB, opts Options) (Result, error) {
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
	return res, nil
}

// enrollOne processes a single image. It returns (added, skipReason, err).
// added=false with empty reason means the file was unchanged (hash match).
func enrollOne(eng *engine.Engine, database *db.DB, name, folder, file string, force bool) (bool, string, error) {
	full := filepath.Join(folder, file)
	b, err := os.ReadFile(full)
	if err != nil {
		return false, "", err
	}
	rel := filepath.Join(filepath.Base(folder), file)
	h := db.HashBytes(b)
	if !force && database.PhotoHash(name, file) == h {
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
	best := faces[0]
	bestArea := area(best.BBox)
	for _, f := range faces[1:] {
		if a := area(f.BBox); a > bestArea {
			best, bestArea = f, a
		}
	}
	emb, err := eng.EmbedFace(b, best)
	if err != nil {
		return false, "", fmt.Errorf("embed: %w", err)
	}
	if err := database.AddPhotoHashed(name, file, h, emb); err != nil {
		return false, "", err
	}
	_ = rel
	return true, "", nil
}

// EnrollBytes embeds a single uploaded image for a person and stores it under
// a synthetic path, used by the API's "add photos to a person" endpoint.
func EnrollBytes(eng *engine.Engine, database *db.DB, name, fileName string, imgBytes []byte) (int, error) {
	faces, err := eng.Detect(imgBytes)
	if err != nil {
		return 0, fmt.Errorf("detect: %w", err)
	}
	if len(faces) == 0 {
		return 0, fmt.Errorf("no face detected in %s", fileName)
	}
	best := faces[0]
	bestArea := area(best.BBox)
	for _, f := range faces[1:] {
		if a := area(f.BBox); a > bestArea {
			best, bestArea = f, a
		}
	}
	emb, err := eng.EmbedFace(imgBytes, best)
	if err != nil {
		return 0, fmt.Errorf("embed: %w", err)
	}
	// Store under an upload/ path with a content-derived name for idempotency.
	rel := "upload/" + db.HashBytes(imgBytes)[:12] + extOr(fileName, ".jpg")
	if err := database.AddPhotoHashed(name, rel, db.HashBytes(imgBytes), emb); err != nil {
		return 0, err
	}
	return len(faces), nil
}

func area(bb [4]float64) float64 { return bb[2] * bb[3] }

func extOr(name, def string) string {
	e := strings.ToLower(filepath.Ext(name))
	if imageExts[e] {
		return e
	}
	return def
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
