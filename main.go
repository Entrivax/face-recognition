// Command recogn is a face-recognition application with a CLI, a REST API and
// a minimal web UI. It organises known people into repositories — each a
// people/<repo-uuid>/<Person>/ photo folder — enrolls them from those folders
// and then detects and identifies every face in new photos.
//
// Usage:
//
//	recogn repo import --name "Name"      wrap the legacy flat people/ folders into a new repository
//	recogn enroll --repo NAME|ID          build/rebuild a repository's face DB
//	recogn recognize --repo NAME|ID img   identify faces in photos, scoped to the repository
//	recogn people --repo NAME|ID          list a repository's enrolled identities
//	recogn export [--out path]            write the face database as JSON
//	recogn serve [--addr :8080]           start the REST API + web UI
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"recogn/internal/api"
	"recogn/internal/config"
	"recogn/internal/db"
	"recogn/internal/engine"
	"recogn/internal/enroll"
	"recogn/internal/models"
	"recogn/internal/netutil"
	"recogn/internal/onnxrt"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, rest := os.Args[1], os.Args[2:]

	switch cmd {
	case "enroll":
		fs := newFlagSet("enroll")
		force := fs.Bool("force", false, "re-embed all photos, ignoring cached hashes")
		prune := fs.Bool("prune", false, "drop DB photo entries whose files are missing from the person folder")
		repoSpec := fs.String("repo", "", "repository to enroll (NAME or ID); required")
		cfg, set := parseFlags(fs, rest)
		must(runEnroll(cfg, *repoSpec, *force, *prune, set["threshold"]))
	case "recognize":
		fs := newFlagSet("recognize")
		repoSpec := fs.String("repo", "", "repository whose identities may match (NAME or ID); required")
		jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
		drawOut := fs.String("draw", "", "also write an annotated copy (boxes + labels): a .jpg/.jpeg path for a single image, or a directory for one copy per image")
		cfg, set := parseFlags(fs, rest)
		if fs.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "recognize: provide at least one image path")
			os.Exit(2)
		}
		must(runRecognize(cfg, *repoSpec, fs.Args(), *jsonOut, *drawOut, set["threshold"]))
	case "people":
		fs := newFlagSet("people")
		repoSpec := fs.String("repo", "", "repository whose people to list (NAME or ID); required")
		jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
		cfg, _ := parseFlags(fs, rest)
		must(runPeople(cfg, *repoSpec, *jsonOut))
	case "repo":
		repoCommand(rest)
	case "export":
		fs := newFlagSet("export")
		out := fs.String("out", "", "output path for the JSON export (default: embeddings.json next to the database)")
		cfg, _ := parseFlags(fs, rest)
		must(runExport(cfg, *out))
	case "serve":
		fs := newFlagSet("serve")
		cfg, set := parseFlags(fs, rest)
		must(runServe(cfg, set["threshold"]))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

// repoCommand dispatches the `recogn repo` subcommands. Only `import` exists
// so far: it wraps the legacy flat people/<Person>/ folders into one new
// repository.
func repoCommand(args []string) {
	if len(args) == 0 || args[0] != "import" {
		if len(args) > 0 {
			fmt.Fprintf(os.Stderr, "unknown repo command %q\n\n", args[0])
		} else {
			fmt.Fprintln(os.Stderr, "repo: a subcommand is required (recogn repo import)")
		}
		usage()
		os.Exit(2)
	}
	fs := newFlagSet("repo import")
	name := fs.String("name", "", `display name of the repository to create (required, e.g. --name "My people")`)
	// parseFlags binds the shared flags (-addr/-threshold/-people/-db);
	// -people here selects the legacy people root to import from (default
	// cfg.PeopleDir).
	cfg, _ := parseFlags(fs, args[1:])
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "repo import: unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}
	if strings.TrimSpace(*name) == "" {
		fmt.Fprintln(os.Stderr, `repo import: a repository name is required (use --name "Name")`)
		usage()
		os.Exit(2)
	}
	must(runRepoImport(cfg, strings.TrimSpace(*name)))
}

// newFlagSet creates a FlagSet for one subcommand.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return fs
}

// parseFlags binds the shared config flags to fs, parses args, and returns the
// resulting Config plus the set of explicitly provided flags (used for the
// threshold precedence: explicit flag > stored DB value > env/default).
func parseFlags(fs *flag.FlagSet, args []string) (config.Config, map[string]bool) {
	cfg := config.Default()
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address (serve)")
	fs.Float64Var(&cfg.Threshold, "threshold", cfg.Threshold,
		"cosine-similarity threshold (0..1) for a positive identity match")
	fs.StringVar(&cfg.PeopleDir, "people", cfg.PeopleDir, "people root (people/<repo-uuid>/<Person>/)")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "face database path")
	_ = fs.Parse(args)
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return cfg, set
}

func usage() {
	fmt.Fprintf(os.Stderr, `recogn — face recognition (CLI + API + web UI)

Usage:
  recogn repo import --name "Name" [--people DIR]
                                       wrap the legacy flat people/ folders into one new repository
  recogn enroll --repo NAME|ID [--force] [--prune]
                                       build or update a repository's face DB from its person folders
  recogn recognize --repo NAME|ID [--json] [--draw OUT] <image...>
                                       detect & identify every face in the given photos, matching
                                       only identities of that repository (--draw: write annotated
                                       copies — a .jpg path for a single image, or a directory for
                                       one copy per image)
  recogn people --repo NAME|ID [--json]
                                       list the identities enrolled in that repository
  recogn export [--out path]           write the face database as JSON (embeddings.json, all repos)
  recogn serve [--addr :8080]          start the REST API and web UI

Shared flags (per subcommand):
  -repo NAME|ID       repository to act on (enroll, recognize, people); required
  -addr string        listen address for serve (default ":8080")
  -threshold float    cosine-similarity threshold 0..1 (default 0.45)
  -people string      people root: people/<repo-uuid>/<Person>/ (default ./people)
  -db string          face database path (default ./data/faces.db)

Dataset layout:
  people/<repo-uuid>/<Person Name>/*.{jpg,jpeg,png,webp,bmp,gif}

Legacy flat folders people/<Person>/ are wrapped into one repository with
'recogn repo import'. To add someone new afterwards, drop a folder under
people/<repo-uuid>/ and run 'recogn enroll --repo X' (incremental), or use
POST /api/people/{name}/enroll.
`)
}

// resolveWorkers returns the batch-worker count for the given config: an
// explicit positive RECOGN_CONCURRENCY wins, otherwise the engine default
// (one stream per core, capped at 4).
func resolveWorkers(cfg config.Config) int {
	if cfg.Concurrency > 0 {
		return cfg.Concurrency
	}
	return engine.DefaultConcurrency()
}

// autoDownloadEnabled reports whether the model auto-download is active.
// Enabled by default; disable with RECOGN_AUTO_DOWNLOAD=0/false/off/no.
func autoDownloadEnabled() bool {
	switch strings.ToLower(os.Getenv("RECOGN_AUTO_DOWNLOAD")) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// openEngine builds the engine + DB and loads identities into the engine.
// Inference runs in-process via CGO + the ONNX Runtime C API, with at most
// cfg.Concurrency (default engine.DefaultConcurrency) model Runs in parallel.
//
// Threshold precedence: an explicit --threshold flag wins; otherwise the value
// persisted in the DB (set via POST /api/config or the UI slider); otherwise
// the env/default in cfg.
func openEngine(cfg config.Config, thresholdSet bool) (*engine.Engine, *db.DB, error) {
	// Register the embedded ONNX Runtime library so the first session can
	// extract and dlopen it (single-file deployment). No-op when nothing is
	// embedded (-tags noembed), in which case the fallback search applies.
	onnxrt.SetRuntimeLibrary(ortLibVersion, ortLibFile, ortLibData)

	// Auto-download the models if they are missing from the models dir.
	if err := models.Ensure(cfg.ModelsDir, cfg.DetModel, cfg.EmbModel, models.Options{
		Auto: autoDownloadEnabled(),
		URL:  os.Getenv("RECOGN_MODELS_URL"),
		Progress: func(done, total int64) {
			if total > 0 {
				fmt.Fprintf(os.Stderr, "\r  downloading models: %3d%% (%d/%d MiB)",
					done*100/total, done>>20, total>>20)
			}
		},
		Done: func(done, total int64) {
			fmt.Fprintf(os.Stderr, "\r  downloading models: %3d%% (%d/%d MiB)\n",
				done*100/total, done>>20, total>>20)
		},
	}); err != nil {
		return nil, nil, err
	}
	if err := engine.CheckModels(cfg.DetModelPath(), cfg.EmbModelPath()); err != nil {
		return nil, nil, err
	}
	eng, err := engine.NewWithConcurrency(cfg.DetModelPath(), cfg.EmbModelPath(), cfg.Threshold, cfg.Concurrency)
	if err != nil {
		return nil, nil, fmt.Errorf("init inference backend: %w", err)
	}
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		eng.Close()
		return nil, nil, err
	}
	if !thresholdSet {
		if t := database.Threshold(); t != nil {
			eng.SetThreshold(*t)
		}
	}
	refreshEngine(eng, database)
	return eng, database, nil
}

// refreshEngine pushes the DB's identities into the engine's matcher.
func refreshEngine(eng *engine.Engine, database *db.DB) {
	people := database.People()
	known := make([]engine.KnownPerson, 0, len(people))
	for _, p := range people {
		kp := engine.KnownPerson{ID: p.ID, Name: p.Name}
		for _, ph := range p.Photos {
			if len(ph.Embedding) > 0 {
				kp.Embeddings = append(kp.Embeddings, ph.Embedding)
			}
		}
		known = append(known, kp)
	}
	eng.SetKnown(known)
}

// repositories

// resolveRepo resolves a --repo spec to a repository: the display name first
// (unique, case-insensitive), then the exact repository ID (also matched
// case-insensitively). A spec that is neither returns an error.
func resolveRepo(database *db.DB, spec string) (db.Repo, error) {
	spec = strings.TrimSpace(spec)
	if repo, ok := database.GetRepoByName(spec); ok {
		return repo, nil
	}
	if repo, ok := database.GetRepo(spec); ok {
		return repo, nil
	}
	return db.Repo{}, fmt.Errorf("repository %q not found", spec)
}

// requireRepoSpec handles a missing --repo flag: it lists the available
// repositories on stderr with a usage hint and exits 2.
func requireRepoSpec(cfg config.Config, hint string) {
	if database, err := db.Open(cfg.DBPath); err == nil {
		printRepoList(database)
		database.Close()
	} else {
		fmt.Fprintln(os.Stderr, "error opening the face database:", err)
	}
	fmt.Fprintf(os.Stderr, "specify a repository with --repo NAME|ID (%s)\n", hint)
	os.Exit(2)
}

// printRepoList writes the available repositories ("<id>  <name>" per line)
// to stderr so the operator can pick a --repo value.
func printRepoList(database *db.DB) {
	repos := database.Repos()
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "available repositories: none — create one with 'recogn repo import --name \"Name\"' or the admin UI")
		return
	}
	fmt.Fprintf(os.Stderr, "available repositories:\n")
	for _, r := range repos {
		fmt.Fprintf(os.Stderr, "  %s  %s\n", r.ID, r.Name)
	}
}

// isRepoUUID reports whether name looks like a repository dataset directory:
// a 36-character lowercase UUID (8-4-4-4-12 hex groups), the shape every
// repo folder is created with. Legacy person folder names never match it, so
// it is what separates repositories from persons under the people root.
func isRepoUUID(name string) bool {
	if len(name) != 36 || name[8] != '-' || name[13] != '-' || name[18] != '-' || name[23] != '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch i {
		case 8, 13, 18, 23:
			continue
		}
		c := name[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isImageName reports whether a file name looks like an enrollable image
// (same extension set the enrollment scanner accepts).
func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif":
		return true
	}
	return false
}

// repoHasImages reports whether the repository's dataset folder exists and at
// least one person folder under it holds at least one image file.
func repoHasImages(repoDir string) bool {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files, err := os.ReadDir(filepath.Join(repoDir, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && isImageName(f.Name()) {
				return true
			}
		}
	}
	return false
}

func runEnroll(cfg config.Config, repoSpec string, force, prune, thresholdSet bool) error {
	if strings.TrimSpace(repoSpec) == "" {
		requireRepoSpec(cfg, "see 'recogn enroll -h'")
	}
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()
	defer database.Close()

	repo, err := resolveRepo(database, repoSpec)
	if err != nil {
		return err
	}
	repoDir := filepath.Join(cfg.PeopleDir, repo.ID)
	fmt.Printf("Enrolling repository %q from %s (model warmup may take a moment)...\n", repo.Name, repoDir)
	if err := eng.Ping(); err != nil {
		return fmt.Errorf("inference backend failed to start: %w", err)
	}
	res, err := enroll.Scan(eng, database, enroll.Options{
		PeopleDir: cfg.PeopleDir,
		RepoID:    repo.ID,
		Force:     force,
		Prune:     prune,
		Workers:   resolveWorkers(cfg),
		Progress: func(person, file string, idx, total int) {
			fmt.Printf("\r  %-24s [%d/%d] %-40s", person, idx, total, truncate(file, 40))
		},
	})
	fmt.Println()
	if err != nil {
		return err
	}
	refreshEngine(eng, database)

	fmt.Printf("\nDone. people=%d  added=%d  kept=%d  failed=%d  pruned=%d\n",
		res.PeopleSeen, res.PhotosAdded, res.PhotosKept, res.PhotosFailed, res.PhotosPruned)
	if len(res.Skipped) > 0 {
		fmt.Println("Skipped photos:")
		for _, s := range res.Skipped {
			fmt.Printf("  - %s / %s: %s\n", s.Person, filepath.Base(s.Path), s.Reason)
		}
	}
	repoPeople := database.PeopleByRepo(repo.ID)
	fmt.Printf("Database: %s (repository %q: %d people, %d embeddings)\n",
		cfg.DBPath, repo.Name, len(repoPeople), countEmbeddings(repoPeople))
	return nil
}

// fileResult is the per-image recognition outcome (also the --json record).
// raw keeps the original bytes for --draw; encoding/json skips it.
type fileResult struct {
	Image string        `json:"image"`
	Faces []engine.Face `json:"faces"`
	Error string        `json:"error,omitempty"`
	raw   []byte
}

func runRecognize(cfg config.Config, repoSpec string, images []string, jsonOut bool, drawArg string, thresholdSet bool) error {
	if strings.TrimSpace(repoSpec) == "" {
		requireRepoSpec(cfg, "see 'recogn recognize -h'")
	}
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()
	defer database.Close()

	repo, err := resolveRepo(database, repoSpec)
	if err != nil {
		return err
	}
	// Matching is scoped to the repository: build the allowed identity set
	// from the repo's own people. A repo with no people yields a non-nil
	// empty set, so every detected face stays unknown.
	people := database.PeopleByRepo(repo.ID)
	if len(people) == 0 {
		fmt.Fprintf(os.Stderr, "warning: repository has no enrolled people — run 'recogn enroll --repo %s' first\n", repo.Name)
	}
	allowed := make(map[string]struct{}, len(people))
	for _, p := range people {
		allowed[p.ID] = struct{}{}
	}
	if err := eng.Ping(); err != nil {
		return fmt.Errorf("inference backend failed to start: %w", err)
	}

	var all []fileResult
	hadErr := false

	// Recognize the images with a bounded worker pool, preserving input
	// order: each result lands in its own index slot, so JSON/annotated/
	// text output is identical to the serial loop's.
	results := make([]fileResult, len(images))
	workers := resolveWorkers(cfg)
	if workers > len(images) {
		workers = len(images)
	}
	if workers <= 1 {
		for i, imgPath := range images {
			results[i] = recognizeOne(eng, imgPath, allowed)
		}
	} else {
		next := make(chan int, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := range next {
					results[i] = recognizeOne(eng, images[i], allowed)
				}
			}()
		}
		for i := range images {
			next <- i
		}
		close(next)
		wg.Wait()
	}
	for _, fr := range results {
		all = append(all, fr)
		if fr.Error != "" {
			hadErr = true
		}
	}

	if drawArg != "" {
		if err := writeAnnotated(drawArg, all, jsonOut); err != nil {
			return err
		}
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(all)
	}

	for _, fr := range all {
		fmt.Printf("%s\n", fr.Image)
		if fr.Error != "" {
			fmt.Printf("  error: %s\n", fr.Error)
			continue
		}
		if len(fr.Faces) == 0 {
			fmt.Println("  no faces detected")
			continue
		}
		for i, f := range fr.Faces {
			label := f.Name
			fmt.Printf("  face %d: %-22s confidence=%.2f  bbox=(%.0f,%.0f %.0fx%.0f)  det=%.2f\n",
				i+1, label, f.Confidence, f.BBox[0], f.BBox[1], f.BBox[2], f.BBox[3], f.Score)
		}
	}
	if hadErr {
		return fmt.Errorf("one or more images failed")
	}
	return nil
}

// recognizeOne runs the full pipeline on one image file, producing the
// per-image CLI/JSON record (embeddings stripped from the faces). allowed
// scopes matching to one repository's identities (nil = unrestricted).
func recognizeOne(eng *engine.Engine, imgPath string, allowed map[string]struct{}) fileResult {
	fr := fileResult{Image: imgPath}
	b, err := os.ReadFile(imgPath)
	if err != nil {
		fr.Error = err.Error()
		return fr
	}
	faces, err := eng.RecognizeScoped(b, allowed)
	if err != nil {
		fr.Error = err.Error()
		return fr
	}
	// Strip embeddings from CLI/JSON output.
	for i := range faces {
		faces[i].Embedding = nil
	}
	fr.Faces = faces
	fr.raw = b
	return fr
}

// writeAnnotated writes annotated copies (boxes + labels) of the successfully
// recognized images. drawArg is either an image path (single input, written
// verbatim) or a directory (one <base>.annotated.jpg per input). Progress
// lines go to stdout in normal mode, stderr in --json mode so the JSON on
// stdout stays machine-readable.
func writeAnnotated(drawArg string, results []fileResult, jsonOut bool) error {
	var ok []fileResult
	for _, fr := range results {
		if fr.Error == "" && fr.raw != nil {
			ok = append(ok, fr)
		}
	}
	if len(ok) == 0 {
		return fmt.Errorf("no successfully recognized image to annotate")
	}
	var outPath func(base string) string
	if len(ok) == 1 && isJPGExt(filepath.Ext(drawArg)) {
		outPath = func(string) string { return drawArg }
	} else {
		if err := os.MkdirAll(drawArg, 0o755); err != nil {
			return fmt.Errorf("create draw directory: %w", err)
		}
		outPath = func(base string) string {
			return filepath.Join(drawArg, strings.TrimSuffix(base, filepath.Ext(base))+".annotated.jpg")
		}
	}
	out := os.Stdout
	if jsonOut {
		out = os.Stderr
	}
	for _, fr := range ok {
		annotated, err := engine.Annotate(fr.raw, fr.Faces)
		if err != nil {
			return fmt.Errorf("annotate %s: %w", fr.Image, err)
		}
		dst := outPath(filepath.Base(fr.Image))
		if err := os.WriteFile(dst, annotated, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		fmt.Fprintf(out, "annotated → %s\n", dst)
	}
	return nil
}

func isJPGExt(ext string) bool {
	e := strings.ToLower(ext)
	return e == ".jpg" || e == ".jpeg"
}

// repoPeopleJSON is the --json shape of `recogn people`: the repository (id +
// name only — the share-token hash is not a CLI concern) plus its people.
type repoPeopleJSON struct {
	Repo   repoInfoJSON `json:"repo"`
	People []db.Person  `json:"people"`
}

type repoInfoJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func runPeople(cfg config.Config, repoSpec string, jsonOut bool) error {
	if strings.TrimSpace(repoSpec) == "" {
		requireRepoSpec(cfg, "see 'recogn people -h'")
	}
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()
	repo, err := resolveRepo(database, repoSpec)
	if err != nil {
		return err
	}
	people := database.PeopleByRepo(repo.ID)

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(repoPeopleJSON{
			Repo:   repoInfoJSON{ID: repo.ID, Name: repo.Name},
			People: people,
		})
	}
	fmt.Printf("Repository %q (%s): %d people\n", repo.Name, repo.ID, len(people))
	if len(people) == 0 {
		fmt.Printf("No people enrolled in this repository — run 'recogn enroll --repo %s'.\n", repo.Name)
		return nil
	}
	fmt.Printf("%d photo embedding(s) in total\n", countEmbeddings(people))
	for _, p := range people {
		fmt.Printf("  %-24s %2d photo(s)  id=%s\n", p.Name, len(p.Photos), p.ID)
	}
	return nil
}

func runExport(cfg config.Config, out string) error {
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()
	path, err := database.ExportTo(out)
	if err != nil {
		return err
	}
	fmt.Printf("Exported %d people (%d embeddings) → %s\n",
		len(database.People()), countEmbeddings(database.People()), path)
	return nil
}

// runRepoImport wraps the legacy flat people/<Person>/ folders into one new
// repository: every non-repo folder under the people root moves under
// people/<repo-uuid>/, each already-enrolled person's DB record is rewritten
// into the repo (fresh <repoUUID>-<slug> ID with photos, metadata and
// thumbnail carried over), and the legacy record is removed. Folders that
// were never enrolled stay for a later 'recogn enroll --repo X'. The command
// is pure file + DB surgery: no engine, no models, no inference, no model
// downloads.
func runRepoImport(cfg config.Config, name string) error {
	peopleRoot := cfg.PeopleDir
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()

	// Find the legacy person folders: every non-hidden directory directly
	// under the people root that is NOT a repository UUID. Repository
	// folders (people/<uuid>/) are left alone.
	repos := database.Repos()
	entries, err := os.ReadDir(peopleRoot)
	if err != nil {
		return fmt.Errorf("read people dir %s: %w", peopleRoot, err)
	}
	var legacy []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if isRepoUUID(e.Name()) {
			continue
		}
		legacy = append(legacy, e.Name())
	}
	sort.Strings(legacy)
	if len(legacy) == 0 {
		if len(repos) == 0 {
			fmt.Printf("Nothing to import: no legacy person folders under %s.\n", peopleRoot)
		} else {
			fmt.Printf("Nothing to import: %s holds only repository folders (%d repositories).\n", peopleRoot, len(repos))
		}
		return nil
	}

	// The name must be free. (Re-running a completed or partially failed
	// import therefore fails here — the leftover legacy folders, if any,
	// are reported in the message.)
	if _, ok := database.GetRepoByName(name); ok {
		return fmt.Errorf("repository name %q is already in use; %d legacy person folder(s) remain under %s",
			name, len(legacy), peopleRoot)
	}

	repo, err := database.CreateRepo(name)
	if err != nil {
		return err
	}
	repoDir := filepath.Join(peopleRoot, repo.ID)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return fmt.Errorf("create repository folder %s (repository %q was created — remove it before retrying): %w",
			repoDir, repo.Name, err)
	}

	// Move the person folders into the repository first. A mid-way failure
	// stops the moves but does not abort outright: the folders already in
	// place are migrated into the repository (their files live there now —
	// leaving their DB records legacy would strand them), the summary is
	// printed, and the recorded move error is returned at the end.
	moved := make([]string, 0, len(legacy))
	var moveErr error
	for _, folder := range legacy {
		dst := filepath.Join(repoDir, folder)
		if err := os.Rename(filepath.Join(peopleRoot, folder), dst); err != nil {
			moveErr = fmt.Errorf("move person folder %s: %w", folder, err)
			if len(moved) > 0 {
				fmt.Fprintf(os.Stderr, "repo import: aborted; %d of %d folder(s) were already moved into %s:\n  %s\n",
					len(moved), len(legacy), repo.ID, strings.Join(moved, "\n  "))
			}
			break
		}
		moved = append(moved, folder)
	}

	// Rewrite the DB records of the already-enrolled people into the repo.
	allPeople := database.People()
	migrated, photosCopied := 0, 0
	var failed []string
	for _, folder := range moved {
		// Match the DB record among LEGACY persons only (RepoID "") —
		// db.Get is a global view and could hand back another repository's
		// person when the name is shared.
		var person *db.Person
		for i := range allPeople {
			if allPeople[i].RepoID == "" && strings.EqualFold(allPeople[i].Name, folder) {
				p := allPeople[i]
				person = &p
				break
			}
		}
		if person == nil {
			continue // the folder was never enrolled; enroll handles it later
		}
		n, why := migrateLegacyPerson(database, repo, person)
		if why == "" {
			migrated++
			photosCopied += n
		} else {
			failed = append(failed, fmt.Sprintf("%s: %s", person.Name, why))
		}
	}

	// Summary.
	fmt.Printf("Imported %d people (%d photos) into repository %q (%s)\n",
		len(moved), photosCopied, repo.Name, repo.ID)
	fmt.Printf("Enrolled records migrated to the database: %d of %d folder(s).\n", migrated, len(moved))
	if notEnrolled := len(moved) - migrated - len(failed); notEnrolled > 0 {
		fmt.Printf("%d folder(s) were not in the database yet — run 'recogn enroll --repo %s' to embed them.\n",
			notEnrolled, repo.Name)
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d enrolled person(s) could not be migrated (their folders moved, but the legacy DB record was kept):\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
	}
	// The recorded move failure (nil on a clean run) — reported only now,
	// after what did move was migrated and summarised.
	return moveErr
}

// migrateLegacyPerson rewrites one legacy person record into the repository:
// every photo (hash + embedding) is re-created through the repo-scoped
// mutator under a fresh <repoUUID>-<slug> ID, the metadata and the thumbnail
// sidecar carry over, and the legacy record (with its photo bucket and old
// sidecar) is removed at the end. Returns the number of photos migrated and
// an empty reason on success; on failure the legacy record is left untouched
// so nothing is lost (the already-copied photos, if any, stay in the repo —
// a retry replaces them by path).
func migrateLegacyPerson(database *db.DB, repo db.Repo, person *db.Person) (int, string) {
	newID := database.NewPersonID(repo.ID, person.Name)

	// 1. Photos: re-create each through the repo-scoped mutator, which
	// creates the person under the new ID on the first call.
	for _, ph := range person.Photos {
		if err := database.AddPhotoHashedForRepo(repo.ID, person.Name, ph.Path, ph.Hash, ph.Embedding); err != nil {
			return 0, fmt.Sprintf("copy photo %s: %v", ph.Path, err)
		}
	}

	// 2. Metadata: a freshly created record has no meta, so copy the legacy
	// metadata explicitly. person.Meta may be nil (IsEmpty reports it);
	// SetMetaInRepo stores an empty meta as none.
	if !person.Meta.IsEmpty() {
		if _, err := database.SetMetaInRepo(repo.ID, person.Name, person.Meta); err != nil {
			return len(person.Photos), fmt.Sprintf("copy metadata: %v", err)
		}
	}

	// 3. Thumbnail sidecar (best-effort, must never fail the import): the
	// legacy sidecar is data/thumbs/<oldID>.jpg; re-write its bytes through
	// SetThumbnail so the new record both gains the file data/thumbs/<newID>.jpg
	// and references it (Thumb + ThumbSrc). If that fails, the sidecar is
	// simply not migrated: the legacy one is removed with the legacy record
	// and the next enroll rescan regenerates a thumbnail for the new ID.
	if old := database.ThumbFile(person.ID); old != "" {
		jpg, err := os.ReadFile(old)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: thumbnail for %s not migrated (%v) — the next enroll rescan regenerates it\n", person.Name, err)
		} else if err := database.SetThumbnail(newID, jpg, person.ThumbSrc); err != nil {
			fmt.Fprintf(os.Stderr, "warning: thumbnail for %s not migrated (%v) — the next enroll rescan regenerates it\n", person.Name, err)
		}
	}

	// 4. Drop the legacy record: its people entry, photo bucket and old
	// thumbnail sidecar.
	if _, err := database.RemovePerson(person.Name); err != nil {
		return len(person.Photos), fmt.Sprintf("remove legacy record: %v", err)
	}
	return len(person.Photos), ""
}

func runServe(cfg config.Config, thresholdSet bool) error {
	// Soft memory limit as a backstop against unbounded allocation (e.g. a
	// regression reintroducing a decode bomb): the GC runs harder as the Go
	// heap approaches the limit instead of letting RSS balloon until the
	// kernel OOM-kills the process. Legit workloads peak far below it. An
	// operator-set GOMEMLIMIT wins.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(4 << 30) // 4 GiB
	}
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()
	defer database.Close()

	// Auto-enroll repositories whose dataset folders hold photos but whose
	// DB records are empty (a fresh store without repositories gets none).
	autoEnrollRepos(eng, database, cfg)

	server, err := api.New(cfg, eng, database, func(e api.Engine, d *db.DB) {
		if ce, ok := e.(*engine.Engine); ok {
			refreshEngine(ce, d)
		}
	})
	if err != nil {
		return err
	}
	handler := server.Handler()

	// Bind before announcing so the printed URLs match the real listener
	// (resolves hostnames and fills in the port for a bare/zero port).
	ln, err := netutil.Listen(cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	srv := &http.Server{
		Handler: handler,
		// Uploads are multipart images and inference is CPU-bound; keep the
		// limits generous but bounded.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	printServing(ln.Addr().String(), len(database.People()), eng.Threshold())
	mode := server.Mode()
	fmt.Printf("Admin auth: %s\n", mode)
	slog.Info("admin authentication: " + mode + " mode")
	if mode == "setup" {
		// First run: no users exist yet, so the auth service holds a one-time
		// setup key that completes POST /api/setup. It lives in memory only —
		// regenerated on every start until an admin exists — so it must be
		// handed to the operator right here.
		if key, ok := server.SetupKey(); ok {
			fmt.Printf("setup key: %s\n", key)
			slog.Info("setup key: " + key)
		}
		fmt.Fprintln(os.Stderr, "First-run setup required: open the web UI's setup view and enter this key together with an admin username and password.")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	// Graceful shutdown on Ctrl+C / SIGTERM: finish in-flight requests, then exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// autoEnrollRepos runs an enrollment scan for every repository that has no
// people yet but already holds person folders with images on disk (e.g. a
// repository created or wrapped before anything was enrolled through it, or
// one whose face DB was deleted). Repositories that already have people are
// untouched. A store without any repository is left alone — there is no
// default repository; the operator creates one via the admin UI or
// 'recogn repo import'.
func autoEnrollRepos(eng *engine.Engine, database *db.DB, cfg config.Config) {
	repos := database.Repos()
	if len(repos) == 0 {
		slog.Info("no repositories exist yet — create one via the admin UI or 'recogn repo import'")
		return
	}
	var empty []db.Repo
	for _, r := range repos {
		if database.PeopleCount(r.ID) == 0 && repoHasImages(filepath.Join(cfg.PeopleDir, r.ID)) {
			empty = append(empty, r)
		}
	}
	if len(empty) == 0 {
		return
	}
	if err := eng.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: skipping auto-enroll — inference backend failed to start: %v\n", err)
		return
	}
	for _, repo := range empty {
		fmt.Printf("Repository %q has no enrolled people; enrolling from %s...\n",
			repo.Name, filepath.Join(cfg.PeopleDir, repo.ID))
		res, err := enroll.Scan(eng, database, enroll.Options{
			PeopleDir: cfg.PeopleDir,
			RepoID:    repo.ID,
			Workers:   resolveWorkers(cfg),
			Progress: func(person, file string, idx, total int) {
				fmt.Printf("\r  %-24s [%d/%d] %-40s", person, idx, total, truncate(file, 40))
			},
		})
		fmt.Println()
		if err != nil {
			// One repository's failure must not keep the server down.
			fmt.Fprintf(os.Stderr, "warning: auto-enroll of repository %q failed: %v\n", repo.Name, err)
			continue
		}
		refreshEngine(eng, database)
		fmt.Printf("Enrolled %d people (%d photos) into repository %q.\n", res.PeopleSeen, res.PhotosAdded, repo.Name)
	}
}

// helpers

// countEmbeddings counts the photo records (one embedding each) in the given
// people list.
func countEmbeddings(people []db.Person) int {
	n := 0
	for _, p := range people {
		n += len(p.Photos)
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// printServing announces where the server is reachable. A specific listen
// host yields a single URL; a wildcard listener lists every matching
// interface (see netutil.URLs).
func printServing(listen string, people int, threshold float64) {
	meta := fmt.Sprintf("(people=%d, threshold=%.2f)", people, threshold)
	urls := netutil.URLs(listen)
	if len(urls) == 1 {
		fmt.Printf("recogn serving on %s  %s\n", urls[0], meta)
		return
	}
	fmt.Printf("recogn serving %s on:\n", meta)
	for _, u := range urls {
		fmt.Printf("  %s\n", u)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
