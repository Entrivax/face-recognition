// Package config centralises paths, tunables and runtime settings for recogn.
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Config holds all runtime settings. Paths are resolved relative to the
// working directory the binary is launched from, so running `recogn` from the
// repository root keeps people/, models/ and data/ side by side.
type Config struct {
	PeopleDir string  // root of per-person photo folders
	ModelsDir string  // directory holding the ONNX models
	DataDir   string  // directory for the generated face database
	DBPath    string  // path to the bbolt face database file (faces.db)
	DetModel  string  // SCRFD detector ONNX filename (inside ModelsDir)
	EmbModel  string  // ArcFace embedder ONNX filename (inside ModelsDir)
	Threshold float64 // cosine-similarity threshold for a positive match
	Addr      string  // listen address for `serve`
	// Concurrency is how many model Runs may execute in parallel (the
	// engine's inference gate) and the default worker count for batch jobs
	// (enrollment scan, CLI recognize, multi-upload). 0 = auto
	// (DefaultConcurrency in internal/engine).
	Concurrency int
}

// Default returns a Config populated from defaults, environment and flags.
// Flags are only parsed once (main calls this after flag.Parse).
func Default() Config {
	wd, _ := os.Getwd()
	cfg := Config{
		PeopleDir: envOr("RECOGN_PEOPLE_DIR", filepath.Join(wd, "people")),
		ModelsDir: envOr("RECOGN_MODELS_DIR", filepath.Join(wd, "models")),
		DataDir:   envOr("RECOGN_DATA_DIR", filepath.Join(wd, "data")),
		DetModel:  envOr("RECOGN_DET_MODEL", "det_10g.onnx"),
		EmbModel:  envOr("RECOGN_EMB_MODEL", "w600k_r50.onnx"),
		Threshold: envFloat("RECOGN_THRESHOLD", 0.45),
		Addr:      envOr("RECOGN_ADDR", ":8080"),
	}
	cfg.Concurrency = envInt("RECOGN_CONCURRENCY", 0)
	cfg.DBPath = filepath.Join(cfg.DataDir, "faces.db")
	return cfg
}

// DetModelPath returns the absolute path to the detector model.
func (c Config) DetModelPath() string { return filepath.Join(c.ModelsDir, c.DetModel) }

// EmbModelPath returns the absolute path to the embedder model.
func (c Config) EmbModelPath() string { return filepath.Join(c.ModelsDir, c.EmbModel) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// envInt parses an integer environment variable, falling back to def when it
// is unset or unparsable.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
