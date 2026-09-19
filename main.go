// Command recogn is a face-recognition application with a CLI, a REST API and
// a minimal web UI. It enrolls known people from a people/ folder and then
// detects and identifies every face in new photos.
//
// Usage:
//
//	recogn enroll [--force]            build/rebuild the face DB from people/
//	recogn recognize <image...>        identify faces in photos
//	recogn people                      list enrolled identities
//	recogn export [--out path]         write the face database as JSON
//	recogn serve [--addr :8080]        start the REST API + web UI
//	recogn hash-password               generate an argon2id admin password hash
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"recogn/internal/api"
	"recogn/internal/auth"
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
	case "export":
		fs := newFlagSet("export")
		out := fs.String("out", "", "output path for the JSON export (default: embeddings.json next to the database)")
		cfg, _ := parseFlags(fs, rest)
		must(runExport(cfg, *out))
	case "serve":
		fs := newFlagSet("serve")
		cfg, set := parseFlags(fs, rest)
		must(runServe(cfg, set["threshold"]))
	case "hash-password":
		fs := newFlagSet("hash-password")
		pw := fs.String("password", "", "hash this password non-interactively instead of prompting (exposed in shell history and process lists — prefer the prompt or a stdin pipe)")
		_ = fs.Parse(rest)
		must(runHashPassword(*pw))
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
  recogn export [--out path]           write the face database as JSON (embeddings.json)
  recogn serve [--addr :8080]          start the REST API and web UI
  recogn hash-password [--password X]  print an argon2id hash for the admin password
                                       (set RECOGN_ADMIN_PASSWORD_HASH to it; the plaintext
                                       password itself is never stored)

Shared flags (per subcommand):
  -addr string        listen address for serve (default ":8080")
  -threshold float    cosine-similarity threshold 0..1 (default 0.45)
  -people string      people dataset directory (default ./people)
  -db string          face database path (default ./data/faces.db)

Dataset layout:
  people/<Person Name>/*.{jpg,jpeg,png,webp,bmp}

To add someone new, drop a folder with their photos into people/ and run
'recogn enroll' again (incremental), or use POST /api/people/{name}/enroll.
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

// openEngine builds the engine + DB and loads identities into the engine.
// Inference runs in-process via CGO + the ONNX Runtime C API, with at most
// cfg.Concurrency (default engine.DefaultConcurrency) model Runs in parallel.
//
// Threshold precedence: an explicit --threshold flag wins; otherwise the value
// persisted in the DB (set via POST /api/config or the UI slider); otherwise
// the env/default in cfg.
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

func runEnroll(cfg config.Config, force, prune, thresholdSet bool) error {
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()
	defer database.Close()

	fmt.Printf("Enrolling from %s (model warmup may take a moment)...\n", cfg.PeopleDir)
	if err := eng.Ping(); err != nil {
		return fmt.Errorf("inference backend failed to start: %w", err)
	}
	res, err := enroll.Scan(eng, database, enroll.Options{
		PeopleDir: cfg.PeopleDir,
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
	fmt.Printf("Database: %s (%d people, %d embeddings)\n",
		cfg.DBPath, len(database.People()), countEmbeddings(database))
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

func runRecognize(cfg config.Config, images []string, jsonOut bool, drawArg string, thresholdSet bool) error {
	eng, database, err := openEngine(cfg, thresholdSet)
	if err != nil {
		return err
	}
	defer eng.Close()
	defer database.Close()
	if len(database.People()) == 0 {
		fmt.Fprintln(os.Stderr, "warning: face database is empty — run 'recogn enroll' first")
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
			results[i] = recognizeOne(eng, imgPath)
		}
	} else {
		next := make(chan int, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := range next {
					results[i] = recognizeOne(eng, images[i])
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
// per-image CLI/JSON record (embeddings stripped from the faces).
func recognizeOne(eng *engine.Engine, imgPath string) fileResult {
	fr := fileResult{Image: imgPath}
	b, err := os.ReadFile(imgPath)
	if err != nil {
		fr.Error = err.Error()
		return fr
	}
	faces, err := eng.Recognize(b)
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

func runPeople(cfg config.Config, jsonOut bool) error {
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()
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
		len(database.People()), countEmbeddings(database), path)
	return nil
}

// runHashPassword prints an argon2id PHC hash of an admin password for
// RECOGN_ADMIN_PASSWORD_HASH. The password comes from --password (scripts;
// exposed in shell history and process lists), from a stdin pipe (e.g.
// `openssl rand -base64 18 | recogn hash-password`), or from a hidden-input
// prompt when run on a terminal. Only the hash goes to stdout so the output
// can be captured directly (`RECOGN_ADMIN_PASSWORD_HASH=$(./recogn
// hash-password ...)`); prompts and hints go to stderr. The command never
// touches the engine, database or models.
func runHashPassword(flagPassword string) error {
	var password []byte // zeroed after hashing
	if flagPassword != "" {
		password = []byte(flagPassword)
	} else if stdinIsTerminal() {
		fmt.Fprintln(os.Stderr, "Enter admin password (input hidden):")
		first, err := readPassword()
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Confirm password:")
		second, err := readPassword()
		if err != nil {
			return err
		}
		if !bytes.Equal(first, second) {
			return fmt.Errorf("passwords do not match")
		}
		password = first
	} else {
		b, err := readPasswordLine()
		if err != nil {
			return err
		}
		password = b
	}
	if len(password) == 0 {
		return fmt.Errorf("password is empty")
	}
	hash, err := auth.HashPassword(password)
	for i := range password {
		password[i] = 0 // best-effort scrub of the plaintext buffer
	}
	if err != nil {
		return err
	}
	fmt.Println(hash)
	fmt.Fprintln(os.Stderr, "\nSet the hash as the admin password credential, e.g.:")
	fmt.Fprintf(os.Stderr, "  RECOGN_ADMIN_PASSWORD_HASH=%s\n", hash)
	return nil
}

// readPasswordLine reads one line from stdin without touching terminal
// settings, trimming the trailing newline. It is the pipe path and the
// fallback used when the terminal echo cannot be disabled.
func readPasswordLine() ([]byte, error) {
	b, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b, nil
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

	// Auto-enroll from people/ if the DB is empty so the server is useful on
	// first run without a separate enroll step.
	if len(database.People()) == 0 {
		if _, statErr := os.Stat(cfg.PeopleDir); statErr == nil {
			fmt.Println("Face DB is empty; enrolling from people/ first...")
			if err := eng.Ping(); err != nil {
				return fmt.Errorf("inference backend failed to start: %w", err)
			}
			res, err := enroll.Scan(eng, database, enroll.Options{
				PeopleDir: cfg.PeopleDir,
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
			fmt.Printf("Enrolled %d people (%d photos).\n", res.PeopleSeen, res.PhotosAdded)
		}
	}

	server, err := api.New(cfg, eng, database, func(e api.Engine, d *db.DB) {
		if ce, ok := e.(*engine.Engine); ok {
			refreshEngine(ce, d)
		}
	})
	if err != nil {
		return err // e.g. corrupt passkeys.json must fail startup loudly
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
	fmt.Printf("Admin auth: %s\n", server.Mode())
	if server.Mode() == "open" {
		// Open mode is backward-compatible but worth one loud line: every
		// endpoint, including enrollment and deletion, is public — and
		// passkey registration is refused there, so the only way to secure
		// the server is to set a password hash (SECURITY-REVIEW.md H2/H3).
		slog.Warn("admin authentication is disabled — all endpoints are public and passkey registration is refused; set RECOGN_ADMIN_PASSWORD_HASH (generate one with 'recogn hash-password') to secure the admin surface")
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
