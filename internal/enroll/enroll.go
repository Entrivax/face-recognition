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
	"sync"

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
	// RepoID scopes the scan to one repository. When empty the legacy
	// flat layout applies: the dataset root is PeopleDir itself, folders
	// are people/<Person>/ and every DB write goes through the legacy
	// mutators. When set, the root is PeopleDir/<RepoID> (the repository's
	// dataset folder; a missing directory fails the scan with the
	// ReadDir error), folders are its direct subdirectories, and every DB
	// write uses the repo-scoped mutators, so persons of other
	// repositories are never touched.
	RepoID string
	Force  bool // re-embed everything, ignoring content hashes
	// Prune drops DB photo entries whose file no longer exists in the
	// person's folder (and, when a person's folder disappeared entirely,
	// all of their photo entries). Opt-in: datasets may be temporarily
	// unmounted, so staleness is never cleaned up automatically. Repo-
	// scoped, like every other write: a repo scan prunes only that repo's
	// persons.
	Prune bool
	// Workers is how many photos are detected+embedded in parallel. Values
	// <= 1 (the default) run exactly like the original serial scan; larger
	// values fan the per-photo work out over a bounded worker pool. The
	// engine's own inference gate stays the authoritative bound, so Workers
	// above it just queue on the gate.
	Workers int
	// Progress is called once per processed file with the person, the file
	// name, and 1-based/total counters within that person's folder. With
	// Workers > 1 files complete out of order, so idx counts completed
	// files of the person rather than scan order. It is always called
	// sequentially (never concurrently) from the aggregation step.
	Progress func(person, file string, idx, total int)
}

// FaceEngine is the subset of the recognition engine that enrollment needs.
// *engine.Engine satisfies it; tests provide stubs.
type FaceEngine interface {
	Detect(imgBytes []byte) ([]engine.Face, error)
	EmbedFace(imgBytes []byte, f engine.Face) ([]float32, error)
}

// scanJob is one photo to process, flattened out of the folder walk so
// parallel workers can pick jobs from any person's folder.
type scanJob struct {
	person string // person (folder) name
	repoID string // repository scope ("" = legacy flat layout)
	folder string // absolute folder path
	file   string // image file name inside the folder
	force  bool
}

// scanOutcome is the per-file result, keyed back to its job by index so
// aggregation can replay the serial scan order exactly.
type scanOutcome struct {
	added  bool
	reason string
	err    error
}

// Scan walks peopleDir, embeds each new/changed image, and stores results in
// the database. The engine must already have its identity set loaded if you
// want matching afterwards; this only writes the DB.
//
// The per-photo work runs with Options.Workers parallel workers (<=1 = the
// serial path); the Result is aggregated from per-file outcomes in the
// original walk order, so counts and Skipped entries are identical to a
// serial run regardless of completion order. DB writes are safe to
// parallelise: every db.DB method takes its own mutex and bbolt serialises
// writers internally.
func Scan(eng FaceEngine, database *db.DB, opts Options) (Result, error) {
	var res Result
	// The dataset root is the people dir itself (legacy flat layout) or
	// the repository's own folder under it.
	root := opts.PeopleDir
	if opts.RepoID != "" {
		root = filepath.Join(opts.PeopleDir, opts.RepoID)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return res, fmt.Errorf("read people dir %s: %w", root, err)
	}

	// Collect person folders (directories only).
	var personDirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			personDirs = append(personDirs, e)
		}
	}
	sort.Slice(personDirs, func(i, j int) bool { return personDirs[i].Name() < personDirs[j].Name() })

	// Flatten the walk into an ordered job list (scan order = the serial
	// loop's order: person dirs sorted, files sorted within each folder).
	var jobs []scanJob
	filesPerPerson := make(map[string]int, len(personDirs))
	for _, pd := range personDirs {
		name := pd.Name()
		folder := filepath.Join(root, name)
		files, err := listImages(folder)
		if err != nil {
			res.Skipped = append(res.Skipped, SkippedPhoto{name, folder, err.Error()})
			continue
		}
		res.PeopleSeen++
		if opts.Prune {
			res.PhotosPruned += pruneMissing(database, opts.RepoID, name, files)
		}
		filesPerPerson[name] = len(files)
		for _, f := range files {
			jobs = append(jobs, scanJob{person: name, repoID: opts.RepoID, folder: folder, file: f, force: opts.Force})
		}
	}

	// Run the jobs with a bounded worker pool; results are written to the
	// outcome slot of their job index. Serial fast-path for Workers <= 1.
	outcomes := make([]scanOutcome, len(jobs))
	if opts.Workers <= 1 {
		for i, job := range jobs {
			outcomes[i] = runScanJob(eng, database, job)
		}
	} else {
		scanParallel(eng, database, jobs, opts.Workers, outcomes)
	}

	// Aggregate in scan order and fire Progress sequentially. Progress idx
	// counts completed files within the person's folder (1-based), so with
	// parallel workers the counter tracks completions, not scan order.
	done := make(map[string]int, len(filesPerPerson))
	for i, job := range jobs {
		if opts.Progress != nil {
			done[job.person]++
			opts.Progress(job.person, job.file, done[job.person], filesPerPerson[job.person])
		}
		o := outcomes[i]
		switch {
		case o.err != nil:
			res.PhotosFailed++
			res.Skipped = append(res.Skipped, SkippedPhoto{job.person, job.file, o.err.Error()})
		case o.added:
			res.PhotosAdded++
		default:
			if o.reason != "" {
				res.PhotosFailed++
				res.Skipped = append(res.Skipped, SkippedPhoto{job.person, job.file, o.reason})
			} else {
				res.PhotosKept++ // unchanged, skipped by hash
			}
		}
	}
	if opts.Prune {
		// People whose dataset folder disappeared entirely never appear in
		// the loop above; drop their photo entries too (the person stays
		// enrolled, with zero photos). Repo-scoped: a repo scan only
		// prunes the repo's own persons.
		res.PhotosPruned += pruneMissingPeople(database, opts.PeopleDir, opts.RepoID)
	}
	return res, nil
}

// runScanJob processes one flattened photo job, returning its outcome.
func runScanJob(eng FaceEngine, database *db.DB, job scanJob) scanOutcome {
	added, reason, err := enrollOne(eng, database, job.repoID, job.person, job.folder, job.file, job.force)
	return scanOutcome{added: added, reason: reason, err: err}
}

// scanParallel runs jobs over `workers` goroutines, each calling the same
// enrollOne the serial path uses. Outcomes land in the job-indexed slice, so
// the caller never needs to know about completion order.
func scanParallel(eng FaceEngine, database *db.DB, jobs []scanJob, workers int, outcomes []scanOutcome) {
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers < 1 {
		workers = 1
	}
	next := make(chan int, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range next {
				outcomes[i] = runScanJob(eng, database, jobs[i])
			}
		}()
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()
}

// pruneMissing removes DB photo entries of the named person whose file is not
// among the folder's listed images. Returns how many entries were removed.
// Legacy (repoID "") persons are matched through the legacy accessors; a set
// repoID scopes every lookup and write to that repository's persons.
func pruneMissing(database *db.DB, repoID, name string, files []string) int {
	p := lookupPerson(database, repoID, name)
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
		if present[path] {
			continue
		}
		if repoID == "" {
			if removed, err := database.RemovePhoto(name, path); err == nil && removed {
				n++
			}
			continue
		}
		if removed, err := database.RemovePhotoInRepo(repoID, name, path); err == nil && removed {
			n++
		}
	}
	return n
}

// pruneMissingPeople removes every DB photo entry for people whose folder is
// entirely gone from the dataset root. Returns the number of removed entries.
// A set repoID scopes this to that repository's persons (folders under
// PeopleDir/<RepoID>); the legacy layout prunes only legacy persons.
func pruneMissingPeople(database *db.DB, peopleDir, repoID string) int {
	root := peopleDir
	if repoID != "" {
		root = filepath.Join(peopleDir, repoID)
	}
	var people []db.Person
	if repoID == "" {
		// The legacy flat layout: prune only persons without a repo.
		for _, p := range database.People() {
			if p.RepoID == "" {
				people = append(people, p)
			}
		}
	} else {
		people = database.PeopleByRepo(repoID)
	}
	n := 0
	for _, p := range people {
		folder := filepath.Join(root, p.Name)
		if _, err := os.Stat(folder); !errors.Is(err, os.ErrNotExist) {
			continue // folder exists (or stat failed) — leave it alone
		}
		for _, ph := range p.Photos {
			if repoID == "" {
				if removed, err := database.RemovePhoto(p.Name, ph.Path); err == nil && removed {
					n++
				}
				continue
			}
			if removed, err := database.RemovePhotoInRepo(repoID, p.Name, ph.Path); err == nil && removed {
				n++
			}
		}
	}
	return n
}

// lookupPerson resolves a person within the scan's scope: db.Get for the
// legacy layout, db.GetInRepo for a repository scan. Returns a deep copy
// (or nil).
func lookupPerson(database *db.DB, repoID, name string) *db.Person {
	if repoID == "" {
		return database.Get(name)
	}
	return database.GetInRepo(repoID, name)
}

// ThumbSize is the pixel size (square) of the generated face thumbnails.
const ThumbSize = 160

// saveThumb stores a face thumbnail for the person if they do not have one
// yet (first enrollment wins). Best-effort: thumbnail problems must never
// fail an enrollment. The person is resolved within repoID's scope (the
// same name can exist in two repositories), and the sidecar write is
// ID-keyed, so it always lands on the right person's thumbnail.
func saveThumb(database *db.DB, repoID, name, photoPath string, imgBytes []byte, f engine.Face) {
	p := lookupPerson(database, repoID, name)
	if p == nil || p.Thumb != "" {
		return // person unknown, or thumbnail already set
	}
	jpg, err := engine.FaceThumb(imgBytes, f, ThumbSize)
	if err != nil {
		return // best-effort
	}
	_ = database.SetThumbnail(p.ID, jpg, photoPath)
}

// thumbPending reports whether the named person exists (within repoID's
// scope) but has no face thumbnail yet (drives the one-time backfill during
// folder scans).
func thumbPending(database *db.DB, repoID, name string) bool {
	p := lookupPerson(database, repoID, name)
	return p != nil && p.Thumb == ""
}

// enrollOne processes a single image. It returns (added, skipReason, err).
// added=false with empty reason means the file was unchanged (hash match).
// Note: DB photo paths are basenames relative to the person's folder, so
// PhotoHash is keyed by the bare file name. A set repoID routes every DB
// access through the repo-scoped mutators.
func enrollOne(eng FaceEngine, database *db.DB, repoID, name, folder, file string, force bool) (bool, string, error) {
	full := filepath.Join(folder, file)
	b, err := os.ReadFile(full)
	if err != nil {
		return false, "", err
	}
	h := db.HashBytes(b)
	photoHash := database.PhotoHash(name, file)
	if repoID != "" {
		photoHash = database.PhotoHashInRepo(repoID, name, file)
	}
	unchanged := !force && photoHash == h
	if unchanged && !thumbPending(database, repoID, name) {
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
		saveThumb(database, repoID, name, file, b, best)
		return false, "", nil
	}
	emb, err := eng.EmbedFace(b, best)
	if err != nil {
		return false, "", fmt.Errorf("embed: %w", err)
	}
	if repoID == "" {
		if err := database.AddPhotoHashed(name, file, h, emb); err != nil {
			return false, "", err
		}
	} else if err := database.AddPhotoHashedForRepo(repoID, name, file, h, emb); err != nil {
		return false, "", err
	}
	saveThumb(database, repoID, name, file, b, best)
	return true, "", nil
}

// Sentinel errors for enrollment outcomes callers distinguish on (the API
// maps them to specific HTTP statuses). Test with errors.Is.
var (
	// ErrNoFace is returned when no face is detected in an uploaded image.
	ErrNoFace = errors.New("no face detected")
	// ErrFaceIndex is returned when a requested face index is outside the
	// detection order of the uploaded image.
	ErrFaceIndex = errors.New("face index out of range")
)

// EnrollFace enrolls one specific face of an uploaded photo, chosen by a
// 1-based index into the detection order (the same order /api/recognize
// reports; detection is deterministic for the same bytes). The UI's
// "name this face" action uses it to enroll one unknown face out of a
// multi-person photo. A set repoID scopes the enrollment into that
// repository (dataset folder PeopleDir/<RepoID> and the repo-scoped DB
// mutators); empty keeps the legacy flat layout. See EnrollBytes for the
// file-naming and dataset semantics.
func EnrollFace(eng FaceEngine, database *db.DB, peopleDir, repoID, name, fileName string, imgBytes []byte, faceIndex int) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err // fail before running inference
	}
	faces, err := eng.Detect(imgBytes)
	if err != nil {
		return "", fmt.Errorf("detect: %w", err)
	}
	if len(faces) == 0 {
		return "", fmt.Errorf("%w in %s", ErrNoFace, fileName)
	}
	if faceIndex < 1 || faceIndex > len(faces) {
		return "", fmt.Errorf("%w: %d (photo has %d face(s))", ErrFaceIndex, faceIndex, len(faces))
	}
	return saveEnrollment(eng, database, peopleDir, repoID, name, fileName, imgBytes, faces[faceIndex-1])
}

// EnrollBytes embeds a single uploaded image for a person, stores the
// embedding in the database, and writes the original image bytes into the
// person's people folder, so runtime enrollments become part of the dataset
// (a later Scan treats them like any other photo). A set repoID scopes the
// enrollment into that repository: the file lands under
// PeopleDir/<RepoID>/<Person>/ and the DB write goes through the
// repo-scoped mutator.
//
// The file is saved as <dataset root>/<Person>/<sha1[:12]><ext> —
// content-derived, so re-uploading the same image is idempotent — and the
// DB photo path is the same basename a folder rescan would derive for it,
// keeping rescans duplicate-free. The returned path is relative to
// peopleDir (including the repo folder for a repo-scoped enrollment).
func EnrollBytes(eng FaceEngine, database *db.DB, peopleDir, repoID, name, fileName string, imgBytes []byte) (string, error) {
	faces, err := eng.Detect(imgBytes)
	if err != nil {
		return "", fmt.Errorf("detect: %w", err)
	}
	if len(faces) == 0 {
		return "", fmt.Errorf("%w in %s", ErrNoFace, fileName)
	}
	best, _ := engine.LargestFace(faces)
	return saveEnrollment(eng, database, peopleDir, repoID, name, fileName, imgBytes, best)
}

// saveEnrollment embeds the chosen face, writes the original image bytes into
// the person's people folder under a content-derived name, and stores the
// embedding in the database.
func saveEnrollment(eng FaceEngine, database *db.DB, peopleDir, repoID, name, fileName string, imgBytes []byte, face engine.Face) (string, error) {
	emb, err := eng.EmbedFace(imgBytes, face)
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
	if p := lookupPerson(database, repoID, name); p != nil {
		folder = p.Name
	}
	fileName = h[:12] + ext
	// The dataset folder: people/<Name> (legacy) or people/<RepoID>/<Name>.
	dir := peopleDir
	if repoID != "" {
		dir = filepath.Join(peopleDir, repoID)
	}
	full := filepath.Join(dir, folder, fileName)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("create people folder: %w", err)
	}
	if err := os.WriteFile(full, imgBytes, 0o644); err != nil {
		return "", fmt.Errorf("write to people folder: %w", err)
	}
	if repoID == "" {
		if err := database.AddPhotoHashed(name, fileName, h, emb); err != nil {
			// The image is already on disk; leave it. The next folder rescan will
			// enroll it, so the dataset self-heals.
			return "", err
		}
	} else if err := database.AddPhotoHashedForRepo(repoID, name, fileName, h, emb); err != nil {
		// The image is already on disk; leave it. The next folder rescan will
		// enroll it, so the dataset self-heals.
		return "", err
	}
	saveThumb(database, repoID, name, fileName, imgBytes, face)
	// The returned path is relative to peopleDir: folder-relative for the
	// legacy layout, including the repo folder for a repo-scoped one.
	rel := filepath.Join(folder, fileName)
	if repoID != "" {
		rel = filepath.Join(repoID, rel)
	}
	return rel, nil
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
