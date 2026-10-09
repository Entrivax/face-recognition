package api

import (
	"net/http"
	"strconv"

	"recogn/internal/engine"
)

// handleRecognize accepts a multipart image under the field "image" (or a raw
// body) and returns every detected face with its identity.
func (s *Server) handleRecognize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	// M1 admission control (public route): rate-limit the client first so
	// 429s never consume an in-flight slot, then take a decode/inference
	// slot — full gate sheds with 503 instead of queueing memory up.
	if ok, retry := s.recognLimiter.allow(s.auth.ClientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded; retry later")
		return
	}
	if !s.inflight.tryAcquire() {
		w.Header().Set("Retry-After", strconv.Itoa(admitRetryAfter))
		writeErr(w, http.StatusServiceUnavailable, "server busy; try again shortly")
		return
	}
	defer s.inflight.release()
	img, err := readImage(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	faces, err := s.eng.Recognize(img)
	if err != nil {
		writeErr(w, engineErrStatus(err), "recognition failed: "+err.Error())
		return
	}
	for i := range faces {
		faces[i].Embedding = nil
	}
	// ?draw=1: respond with the annotated image (boxes + labels) instead of
	// the JSON report.
	if r.URL.Query().Get("draw") == "1" {
		out, err := engine.Annotate(img, faces)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "annotate failed: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(faces),
		"faces": faces,
	})
}
