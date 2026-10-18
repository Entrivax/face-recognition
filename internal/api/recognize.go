package api

import (
	"net/http"
	"strconv"

	"recogn/internal/engine"
)

// repoAllowedSet builds the person-ID set of one repository: the allowed
// set RecognizeScoped restricts matching to, so a scoped request can only
// ever name identities of the repository it acts on. An empty repository
// yields an empty (non-nil) set — every face comes back unknown, which is
// the correct answer for a repo with no enrolled people.
func (s *Server) repoAllowedSet(repoID string) map[string]struct{} {
	people := s.db.PeopleByRepo(repoID)
	allowed := make(map[string]struct{}, len(people))
	for _, p := range people {
		allowed[p.ID] = struct{}{}
	}
	return allowed
}

// handleRecognize accepts a multipart image under the field "image" (or a
// raw body) and returns every detected face with its identity, matched
// against the repository named by ?repo= (required). The caller needs at
// least the reader role on that repository (share tokens included).
func (s *Server) handleRecognize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	// The repository is part of the request's identity (which identity set
	// to match against); refuse before spending any admission budget.
	repoID, ok := s.requireRepo(w, r)
	if !ok {
		return
	}
	// Role check before admission control: a 401/403 must not consume a
	// rate-limit token or an in-flight slot.
	if !s.requireRepoRole(w, r, repoID, roleReader) {
		return
	}
	// M1 admission control (scoped route): rate-limit the client first so
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
	faces, err := s.eng.RecognizeScoped(img, s.repoAllowedSet(repoID))
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
