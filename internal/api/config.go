package api

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"threshold": s.eng.Threshold()})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var body struct {
			Threshold float64 `json:"threshold"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if body.Threshold < 0 || body.Threshold > 1 {
			writeErr(w, http.StatusBadRequest, "threshold must be between 0 and 1")
			return
		}
		s.eng.SetThreshold(body.Threshold)
		// Persist so the value survives restarts (restored in openEngine
		// unless an explicit --threshold flag overrides it).
		if err := s.db.SetThreshold(body.Threshold); err != nil {
			writeErr(w, http.StatusInternalServerError, "persist threshold: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"threshold": s.eng.Threshold()})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "unsupported method")
	}
}
