package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"recogn/internal/db"
	"recogn/internal/enroll"
)

func (s *Server) handleDeletePerson(w http.ResponseWriter, r *http.Request, name string) {
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	// Capture the canonical name before the record is gone.
	dir := filepath.Join(s.cfg.PeopleDir, p.Name)
	removed, err := s.db.RemovePerson(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	// Best-effort: drop the dataset folder too, otherwise the next rescan
	// silently re-enrolls the deleted person ("resurrection"). CheckName
	// guarantees no separators, so only the direct folder is removed.
	folderRemoved := true
	if err := os.RemoveAll(dir); err != nil {
		folderRemoved = false
		slog.Warn("remove people folder", "dir", dir, "err", err)
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{
		"removed":        name,
		"folder_removed": folderRemoved,
	})
}

// handleRenamePerson renames a person end-to-end: the people/<Name> dataset
// folder, the derived person ID, the thumbnail sidecar file, and the DB
// record. Body: {"name": "<new name>"}. The folder is moved first; a DB
// failure rolls it back so the dataset never drifts from the database.
func (s *Server) handleRenamePerson(w http.ResponseWriter, r *http.Request, oldName string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(oldName); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	newName := strings.TrimSpace(body.Name)
	if err := enroll.CheckName(newName); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(oldName)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	oldCanonical := p.Name // db.Get returns a copy; use the canonical name for the folder
	if newName == oldCanonical {
		writeErr(w, http.StatusBadRequest, "new name is unchanged")
		return
	}
	if other := s.db.Get(newName); other != nil && other.ID != p.ID {
		writeErr(w, http.StatusConflict, fmt.Sprintf("a person named %q is already enrolled", newName))
		return
	}
	// Folder: refuse to clobber a different existing folder (this also
	// blocks case-collision siblings like Serra/serra on case-sensitive
	// filesystems), while os.SameFile keeps case-only renames working on
	// case-insensitive ones. A missing old folder (person without a dataset
	// folder) is fine — only the DB changes then.
	oldDir := filepath.Join(s.cfg.PeopleDir, oldCanonical)
	newDir := filepath.Join(s.cfg.PeopleDir, newName)
	folderMoved := false
	if oldFi, err := os.Stat(oldDir); err == nil {
		if newFi, err2 := os.Stat(newDir); err2 == nil && !os.SameFile(oldFi, newFi) {
			writeErr(w, http.StatusConflict, "a people folder with that name already exists")
			return
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			writeErr(w, http.StatusInternalServerError, "rename people folder: "+err.Error())
			return
		}
		folderMoved = true
	}
	upd, err := s.db.RenamePerson(oldCanonical, newName)
	if err != nil {
		if folderMoved { // keep dataset and DB consistent
			_ = os.Rename(newDir, oldDir)
		}
		switch {
		case errors.Is(err, db.ErrPersonNotFound):
			writeErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, db.ErrNameTaken):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.reload()
	writeJSON(w, http.StatusOK, map[string]any{
		"renamed":  true,
		"old_name": oldCanonical,
		"name":     upd.Name,
		"id":       upd.ID,
	})
}
