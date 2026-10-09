package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"recogn/internal/engine"
	"recogn/internal/enroll"
)

// handleEnrollPerson adds one or more uploaded photos to a (new or existing)
// person. Accepts multipart files under "images" (or "image"). On success each
// photo's embedding goes into the DB and the image file itself is written into
// the person's folder under the people directory, so runtime uploads become
// part of the dataset.
func (s *Server) handleEnrollPerson(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	var files []struct {
		name string
		data []byte
	}
	for _, field := range []string{"images", "image", "files"} {
		if r.MultipartForm == nil {
			break
		}
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
			files = append(files, struct {
				name string
				data []byte
			}{fh.Filename, b})
		}
		if len(files) > 0 {
			break
		}
	}
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "no image files provided (field 'images')")
		return
	}
	added := 0
	var failures []string
	saved := make([]string, 0, len(files))
	// Embed the uploads with a bounded worker pool, preserving input order:
	// each result lands in its index slot, so `saved` and `failures` list
	// files in the order they were submitted regardless of completion order.
	type enrollResult struct {
		rel string
		err error
	}
	results := make([]enrollResult, len(files))
	workers := s.workers
	if workers > len(files) {
		workers = len(files)
	}
	if workers <= 1 {
		for i, f := range files {
			rel, err := enroll.EnrollBytes(s.eng, s.db, s.cfg.PeopleDir, name, f.name, f.data)
			results[i] = enrollResult{rel: rel, err: err}
		}
	} else {
		next := make(chan int, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := range next {
					rel, err := enroll.EnrollBytes(s.eng, s.db, s.cfg.PeopleDir, name, files[i].name, files[i].data)
					results[i] = enrollResult{rel: rel, err: err}
				}
			}()
		}
		for i := range files {
			next <- i
		}
		close(next)
		wg.Wait()
	}
	for i, r := range results {
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", files[i].name, r.err))
			continue
		}
		added++
		saved = append(saved, r.rel)
	}
	s.reload()
	resp := map[string]any{"person": name, "added": added, "total": len(files), "saved": saved}
	if len(failures) > 0 {
		resp["failures"] = failures
	}
	status := http.StatusOK
	if added == 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, resp)
}

// handleEnrollFace enrolls one specific face of an uploaded photo as (or
// into) the named person. Body: multipart "image" plus "face_index" — a
// 1-based index into the detection order /api/recognize reported for the same
// bytes (detection is deterministic). The UI's "name this face" action on
// unknown recognition results uses it.
func (s *Server) handleEnrollFace(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	idx, err := strconv.Atoi(strings.TrimSpace(r.FormValue("face_index")))
	if err != nil || idx < 1 {
		writeErr(w, http.StatusBadRequest, "face_index must be a positive integer")
		return
	}
	img, err := readMultipartFile(r, []string{"image", "file", "photo"})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := enroll.EnrollFace(s.eng, s.db, s.cfg.PeopleDir, name, "upload", img, idx)
	if err != nil {
		switch {
		case errors.Is(err, enroll.ErrFaceIndex):
			writeErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, enroll.ErrNoFace):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, engine.ErrImageTooLarge):
			// The photo's declared dimensions exceed the decode pixel cap.
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{"person": name, "saved": saved})
}

// handleEnrollFolder rescans the people/ directory (incremental).
func (s *Server) handleEnrollFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	force := r.URL.Query().Get("force") == "true"
	prune := r.URL.Query().Get("prune") == "true"
	res, err := enroll.Scan(s.eng, s.db, enroll.Options{
		PeopleDir: s.cfg.PeopleDir,
		Force:     force,
		Prune:     prune,
		Workers:   s.workers,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{
		"people":        res.PeopleSeen,
		"added":         res.PhotosAdded,
		"kept":          res.PhotosKept,
		"failed":        res.PhotosFailed,
		"pruned":        res.PhotosPruned,
		"skipped_count": len(res.Skipped),
	})
}
