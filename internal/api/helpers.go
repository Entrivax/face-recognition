package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"recogn/internal/engine"
)

// readImage extracts image bytes from a multipart "image" field or raw body.
func readImage(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		// multipartMemory (not maxUpload): big parts spill to OS temp files
		// instead of being buffered RAM-resident for the whole request (M1).
		if err := r.ParseMultipartForm(multipartMemory); err != nil {
			return nil, fmt.Errorf("parse multipart: %w", err)
		}
		for _, field := range []string{"image", "file", "photo"} {
			f, _, err := r.FormFile(field)
			if err == nil {
				defer f.Close()
				return io.ReadAll(io.LimitReader(f, maxUpload))
			}
		}
		return nil, errors.New("multipart field 'image' not found")
	}
	defer r.Body.Close()
	// The body is MaxBytesReader-capped above, so ReadAll stops at maxUpload
	// with an error instead of buffering an unbounded stream.
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("empty body")
	}
	return b, nil
}

// readMultipartFile returns the bytes of the first uploaded file under one of
// the given multipart fields, capped at maxUpload.
func readMultipartFile(r *http.Request, fields []string) ([]byte, error) {
	if r.MultipartForm == nil {
		return nil, errors.New("no file provided")
	}
	for _, field := range fields {
		for _, fh := range r.MultipartForm.File[field] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(f, maxUpload))
			f.Close()
			if err != nil {
				continue
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("no image file provided (field '%s')", fields[0])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// engineErrStatus picks the HTTP status for an engine error: images whose
// declared dimensions exceed the decode pixel cap (decompression-bomb guard,
// engine.ErrImageTooLarge) are the client's fault → 400; everything else is a
// backend failure → 502.
func engineErrStatus(err error) int {
	if errors.Is(err, engine.ErrImageTooLarge) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}
