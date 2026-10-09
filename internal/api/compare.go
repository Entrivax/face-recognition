package api

import (
	"net/http"
	"strconv"

	"recogn/internal/engine"
)

// compareFace is one detected face of a compared photo: detection metadata
// plus whether the comparison used it.
type compareFace struct {
	Index int        `json:"index"` // 1-based detection order
	BBox  [4]float64 `json:"bbox"`
	Score float64    `json:"score"` // detector confidence 0..1
	Used  bool       `json:"used"`
}

// compareImage summarises the faces found in one of the two compared photos.
type compareImage struct {
	Count int           `json:"count"`
	Faces []compareFace `json:"faces"`
}

// handleCompare compares the largest face of two uploaded photos and returns
// their cosine similarity — a pure face-to-face check that never matches
// against (or writes to) the identity database. Body: multipart files under
// "image1" and "image2". The threshold in the response is the recognizer's
// config value, returned only as a reference for the "same person?" verdict.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	// M1: compare runs two detections and two embeddings per request, so it
	// shares the recognize in-flight budget (rate limit not needed: admin-only).
	if !s.inflight.tryAcquire() {
		w.Header().Set("Retry-After", strconv.Itoa(admitRetryAfter))
		writeErr(w, http.StatusServiceUnavailable, "server busy; try again shortly")
		return
	}
	defer s.inflight.release()
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	imgA, err := readMultipartFile(r, []string{"image1", "a"})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "first photo: "+err.Error())
		return
	}
	imgB, err := readMultipartFile(r, []string{"image2", "b"})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "second photo: "+err.Error())
		return
	}
	facesA, err := s.eng.Detect(imgA)
	if err != nil {
		writeErr(w, engineErrStatus(err), "detect failed (first photo): "+err.Error())
		return
	}
	facesB, err := s.eng.Detect(imgB)
	if err != nil {
		writeErr(w, engineErrStatus(err), "detect failed (second photo): "+err.Error())
		return
	}
	if len(facesA) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "no face detected in the first photo")
		return
	}
	if len(facesB) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "no face detected in the second photo")
		return
	}
	// Compare the largest face of each photo — the face enrollment and
	// thumbnails also use — so multi-face photos have a deterministic choice.
	bestA, _ := engine.LargestFace(facesA)
	bestB, _ := engine.LargestFace(facesB)
	embA, err := s.eng.EmbedFace(imgA, bestA)
	if err != nil {
		writeErr(w, engineErrStatus(err), "embed failed (first photo): "+err.Error())
		return
	}
	embB, err := s.eng.EmbedFace(imgB, bestB)
	if err != nil {
		writeErr(w, engineErrStatus(err), "embed failed (second photo): "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"similarity": engine.Cosine(embA, embB),
		"threshold":  s.eng.Threshold(),
		"image1":     compareImage{Count: len(facesA), Faces: compareFaceList(facesA, bestA)},
		"image2":     compareImage{Count: len(facesB), Faces: compareFaceList(facesB, bestB)},
	})
}

// compareFaceList renders a detection list with the compared face flagged.
// The used face is identified by its bounding box, which came from this very
// slice (NMS removes overlapping boxes, so duplicates cannot occur).
func compareFaceList(faces []engine.Face, used engine.Face) []compareFace {
	out := make([]compareFace, 0, len(faces))
	for i, f := range faces {
		out = append(out, compareFace{Index: i + 1, BBox: f.BBox, Score: f.Score, Used: f.BBox == used.BBox})
	}
	return out
}
