// Command recogn is a face-recognition application with a CLI, a REST API and
// a minimal web UI. It enrolls known people from a people/ folder and then
// detects and identifies every face in new photos.
//
// Usage:
//
//	recogn enroll [--force]            build/rebuild the face DB from people/
//	recogn recognize <image...>        identify faces in photos
//	recogn people                      list enrolled identities
//	recogn serve [--addr :8080]        start the REST API + web UI
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"recogn/internal/api"
	"recogn/internal/config"
	"recogn/internal/db"
	"recogn/internal/engine"
	"recogn/internal/enroll"
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
		prune := fs.Bool("prune", false, "drop DB photo entries whose files are missing from the people folder")
		cfg, set := parseFlags(fs, rest)
		must(runEnroll(cfg, *force, *prune, set["threshold"]))
	case "recognize":
		fs := newFlagSet("recognize")
		jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
		drawOut := fs.String("draw", "", "also write an annotated copy (boxes + labels): a .jpg/.jpeg path for a single image, or a directory for one copy per image")
		cfg, set := parseFlags(fs, rest)
		if fs.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "recognize: provide at least one image path")
			os.Exit(2)
		}
		must(runRecognize(cfg, fs.Args(), *jsonOut, *drawOut, set["threshold"]))
	case "people":
		fs := newFlagSet("people")
		jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
		cfg, _ := parseFlags(fs, rest)
		must(runPeople(cfg, *jsonOut))
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
	fs.StringVar(&cfg.PeopleDir, "people", cfg.PeopleDir, "people dataset directory")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "face database path")
	_ = fs.Parse(args)
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return cfg, set
}

func usage() {
	fmt.Fprintf(os.Stderr, `recogn — face recognition (CLI + API + web UI)

Usage:
  recogn enroll [--force] [--prune]      build or update the face DB from the people folder
  recogn recognize [--json] [--draw out] <image...>
                                       detect & identify every face in the given photos
                                       (--draw: write annotated copies — a .jpg path for a
                                       single image, or a directory for one copy per image)
  recogn people [--json]               list enrolled identities
  recogn serve [--addr :8080]          start the REST API and web UI

Shared flags (per subcommand):
  -addr string        listen address for serve (default ":8080")
  -threshold float    cosine-similarity threshold 0..1 (default 0.45)
  -people string      people dataset directory (default ./people)
  -db string          face database path (default ./data/embeddings.json)

Dataset layout:
  people/<Person Name>/*.{jpg,jpeg,png,webp,bmp}

To add someone new, drop a folder with their photos into people/ and run
'recogn enroll' again (incremental), or use POST /api/people/{name}/enroll.
`)
}

// openEngine builds the engine + DB and loads identities into the engine.
// Inference runs in-process via CGO + the ONNX Runtime C API.
//
// Threshold precedence: an explicit --threshold flag wins; otherwise the value
// persisted in the DB (set via POST /api/config or the UI slider); otherwise
// the env/default in cfg.
func openEngine(cfg config.Config, thresholdSet bool) (*engine.Engine, *db.DB, error) {
	if err := engine.CheckModels(cfg.DetModelPath(), cfg.EmbModelPath()); err != nil {
		return nil, nil, err
	}
	eng, err := engine.New(cfg.DetModelPath(), cfg.EmbModelPath(), cfg.Threshold)
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

func runEnroll(cfg config.Config, force, prune, thresholdSet bool) error {
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()

	fmt.Printf("Enrolling from %s (model warmup may take a moment)...\n", cfg.PeopleDir)
	if err := eng.Ping(); err != nil {
		return fmt.Errorf("inference sidecar failed to start: %w", err)
	}
	res, err := enroll.Scan(eng, database, enroll.Options{
		PeopleDir: cfg.PeopleDir,
		Force:     force,
		Prune:     prune,
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
	fmt.Printf("Database: %s (%d people, %d embeddings)\n",
		cfg.DBPath, len(database.People()), countEmbeddings(database))
	return nil
}

// fileResult is the per-image recognition outcome (also the --json record).
// raw keeps the original bytes for --draw; encoding/json skips it.
type fileResultT struct {
	Image string        `json:"image"`
	Faces []engine.Face `json:"faces"`
	Error string        `json:"error,omitempty"`
	raw   []byte
}

func runRecognize(cfg config.Config, images []string, jsonOut bool, drawArg string, thresholdSet bool) error {
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()
	if len(database.People()) == 0 {
		fmt.Fprintln(os.Stderr, "warning: face database is empty — run 'recogn enroll' first")
	}
	if err := eng.Ping(); err != nil {
		return fmt.Errorf("inference sidecar failed to start: %w", err)
	}

	var all []fileResultT
	hadErr := false

	for _, imgPath := range images {
		fr := fileResultT{Image: imgPath}
		b, err := os.ReadFile(imgPath)
		if err != nil {
			fr.Error = err.Error()
			hadErr = true
			all = append(all, fr)
			continue
		}
		faces, err := eng.Recognize(b)
		if err != nil {
			fr.Error = err.Error()
			hadErr = true
			all = append(all, fr)
			continue
		}
		// Strip embeddings from CLI/JSON output.
		for i := range faces {
			faces[i].Embedding = nil
		}
		fr.Faces = faces
		fr.raw = b
		all = append(all, fr)
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
			if label == "unknown" {
				label = "unknown"
			}
			fmt.Printf("  face %d: %-22s confidence=%.2f  bbox=(%.0f,%.0f %.0fx%.0f)  det=%.2f\n",
				i+1, label, f.Confidence, f.BBox[0], f.BBox[1], f.BBox[2], f.BBox[3], f.Score)
		}
	}
	if hadErr {
		return fmt.Errorf("one or more images failed")
	}
	return nil
}

// writeAnnotated writes annotated copies (boxes + labels) of the successfully
// recognized images. drawArg is either an image path (single input, written
// verbatim) or a directory (one <base>.annotated.jpg per input). Progress
// lines go to stdout in normal mode, stderr in --json mode so the JSON on
// stdout stays machine-readable.
func writeAnnotated(drawArg string, results []fileResultT, jsonOut bool) error {
	var ok []fileResultT
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

func runPeople(cfg config.Config, jsonOut bool) error {
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	people := database.People()
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(people)
	}
	if len(people) == 0 {
		fmt.Println("No people enrolled. Run 'recogn enroll'.")
		return nil
	}
	fmt.Printf("%d people enrolled:\n", len(people))
	for _, p := range people {
		fmt.Printf("  %-24s %2d photo(s)  id=%s\n", p.Name, len(p.Photos), p.ID)
	}
	return nil
}

func runServe(cfg config.Config, thresholdSet bool) error {
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()

	// Auto-enroll from people/ if the DB is empty so the server is useful on
	// first run without a separate enroll step.
	if len(database.People()) == 0 {
		if _, statErr := os.Stat(cfg.PeopleDir); statErr == nil {
			fmt.Println("Face DB is empty; enrolling from people/ first...")
			if err := eng.Ping(); err != nil {
				return fmt.Errorf("inference sidecar failed to start: %w", err)
			}
			res, err := enroll.Scan(eng, database, enroll.Options{
				PeopleDir: cfg.PeopleDir,
				Progress: func(person, file string, idx, total int) {
					fmt.Printf("\r  %-24s [%d/%d] %-40s", person, idx, total, truncate(file, 40))
				},
			})
			fmt.Println()
			if err != nil {
				return err
			}
			refreshEngine(eng, database)
			fmt.Printf("Enrolled %d people (%d photos).\n", res.PeopleSeen, res.PhotosAdded)
		}
	}

	handler := api.New(cfg, eng, database, func(e api.Engine, d *db.DB) {
		if ce, ok := e.(*engine.Engine); ok {
			refreshEngine(ce, d)
		}
	}).Handler()

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// Uploads are multipart images and inference is CPU-bound; keep the
		// limits generous but bounded.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Printf("recogn serving on http://localhost%s  (people=%d, threshold=%.2f)\n",
		normalizeAddr(cfg.Addr), len(database.People()), eng.Threshold())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

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

// helpers

func countEmbeddings(database *db.DB) int {
	n := 0
	for _, p := range database.People() {
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

func normalizeAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	return ":" + addr
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
