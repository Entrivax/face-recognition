package api

// Tests for the writer-gated person management flows (repo-scoped): rename
// (folder + ID + thumbnail sidecar move together), delete (record + folder
// + resurrection guard), photo serving/deletion and the thumbnail chooser.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recogn/internal/db"
	"recogn/internal/engine"
)

// TestPersonRename covers POST /api/people/{name}/rename?repo=: the DB
// record, the people/<repoID>/<Name> folder and the thumbnail sidecar all
// move together, and the old name/id stop resolving.
func TestPersonRename(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	img := pngBytes(t, 120, 120, 0)
	photo := db.HashBytes(img)[:12] + ".png"
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}
	// Bob is enrolled too (DB-only), so renaming Alice→Bob must conflict.
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Bob", "b.png", db.HashBytes(img), []float32{1}); err != nil {
		t.Fatal(err)
	}
	reloadCount := 0
	e.s.refresh = func(Engine, *db.DB) { reloadCount++ }

	renamePath := "/api/people/Alice/rename?repo=" + e.repoID
	r := sendJSON(t, e.s, e.writer, renamePath, map[string]string{"name": "Alicia"})
	if r.Code != http.StatusOK {
		t.Fatalf("rename: got %d (%s)", r.Code, r.Body.String())
	}
	var resp struct {
		Renamed bool   `json:"renamed"`
		OldName string `json:"old_name"`
		Name    string `json:"name"`
		ID      string `json:"id"`
	}
	decodeBody(t, r, &resp)
	if !resp.Renamed || resp.OldName != "Alice" || resp.Name != "Alicia" || resp.ID != e.db.NewPersonID(e.repoID, "Alicia") {
		t.Errorf("unexpected rename response: %+v", resp)
	}
	if reloadCount != 1 {
		t.Errorf("engine should be reloaded after a rename, got %d reloads", reloadCount)
	}

	// DB: old name gone, new name resolves, photos intact.
	if e.db.GetInRepo(e.repoID, "Alice") != nil {
		t.Errorf("old name should be gone from the DB")
	}
	p := e.db.GetInRepo(e.repoID, "Alicia")
	if p == nil || p.Name != "Alicia" || len(p.Photos) != 1 || p.Photos[0].Path != photo {
		t.Fatalf("unexpected DB person: %+v", p)
	}

	// Disk: folder renamed inside the repo folder, sidecar renamed.
	repoDir := filepath.Join(e.s.cfg.PeopleDir, e.repoID)
	if _, err := os.Stat(filepath.Join(repoDir, "Alice")); !os.IsNotExist(err) {
		t.Errorf("old folder should be gone, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "Alicia", photo)); err != nil {
		t.Errorf("photo file should be under the new folder: %v", err)
	}
	oldThumb := filepath.Join(e.db.ThumbDir(), p.Photos[0].Path, "..", "alice.jpg") // thumbs/<oldID>.jpg
	_ = oldThumb
	oldID := e.db.NewPersonID(e.repoID, "Alice")
	newID := e.db.NewPersonID(e.repoID, "Alicia")
	if _, err := os.Stat(filepath.Join(e.db.ThumbDir(), oldID+".jpg")); !os.IsNotExist(err) {
		t.Errorf("old sidecar should be gone, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(e.db.ThumbDir(), newID+".jpg")); err != nil {
		t.Errorf("renamed sidecar missing: %v", err)
	}

	// Routes follow the new identity; the old ones 404.
	if code := getJSON(t, e.s, e.writer, "/api/people/Alicia?repo="+e.repoID, nil).Code; code != http.StatusOK {
		t.Errorf("GET new name: got %d, want 200", code)
	}
	if code := getJSON(t, e.s, e.writer, "/api/people/Alice?repo="+e.repoID, nil).Code; code != http.StatusNotFound {
		t.Errorf("GET old name: got %d, want 404", code)
	}
	if code := getJSON(t, e.s, e.writer, "/api/thumbs/"+newID+".jpg", nil).Code; code != http.StatusOK {
		t.Errorf("GET new thumb: got %d, want 200", code)
	}
	if code := getJSON(t, e.s, e.writer, "/api/thumbs/"+oldID+".jpg", nil).Code; code != http.StatusNotFound {
		t.Errorf("GET old thumb: got %d, want 404", code)
	}
	if code := getJSON(t, e.s, e.writer, "/api/people/Alicia/photos/"+photo+"?repo="+e.repoID, nil).Code; code != http.StatusOK {
		t.Errorf("photo under new name: got %d, want 200", code)
	}

	// Errors: collision with an enrolled person, invalid/empty/unchanged
	// name, unknown person, method.
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alicia/rename?repo="+e.repoID, map[string]string{"name": "Bob"}).Code; code != http.StatusConflict {
		t.Errorf("name collision: got %d, want 409", code)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alicia/rename?repo="+e.repoID, map[string]string{"name": `a\b`}).Code; code != http.StatusBadRequest {
		t.Errorf("path-like name: got %d, want 400", code)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alicia/rename?repo="+e.repoID, map[string]string{"name": "   "}).Code; code != http.StatusBadRequest {
		t.Errorf("empty name: got %d, want 400", code)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alicia/rename?repo="+e.repoID, map[string]string{"name": "Alicia"}).Code; code != http.StatusBadRequest {
		t.Errorf("unchanged name: got %d, want 400", code)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Nobody/rename?repo="+e.repoID, map[string]string{"name": "X"}).Code; code != http.StatusNotFound {
		t.Errorf("unknown person: got %d, want 404", code)
	}
	if code := do(t, e.s, http.MethodGet, "/api/people/Alicia/rename?repo="+e.repoID, nil, "", e.writer, nil).Code; code != http.StatusMethodNotAllowed {
		t.Errorf("GET rename: got %d, want 405", code)
	}

	// A folder on disk without a DB entry blocks the rename (409), and a
	// failed rename leaves the person untouched.
	if err := os.MkdirAll(filepath.Join(repoDir, "Carol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alicia/rename?repo="+e.repoID, map[string]string{"name": "Carol"}).Code; code != http.StatusConflict {
		t.Errorf("folder collision: got %d, want 409", code)
	}
	if e.db.GetInRepo(e.repoID, "Carol") != nil || e.db.GetInRepo(e.repoID, "Alicia") == nil {
		t.Errorf("rejected rename must not change anything")
	}

	// Case-only rename works and keeps the ID.
	if r = sendJSON(t, e.s, e.writer, "/api/people/Alicia/rename?repo="+e.repoID, map[string]string{"name": "ALICIA"}); r.Code != http.StatusOK {
		t.Fatalf("case-only rename: got %d (%s), want 200", r.Code, r.Body.String())
	}
	if p := e.db.GetInRepo(e.repoID, "alicia"); p == nil || p.Name != "ALICIA" || p.ID != newID {
		t.Errorf("unexpected person after case-only rename: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(e.db.ThumbDir(), newID+".jpg")); err != nil {
		t.Errorf("sidecar should be untouched by a case-only rename: %v", err)
	}
}

// TestPersonRenameWithoutFolder covers a DB-only person (no dataset
// folder): the rename still succeeds, touching just the DB and thumbnail
// bookkeeping.
func TestPersonRenameWithoutFolder(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, false)
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Ghost", "g.png", db.HashBytes([]byte("img")), []float32{1}); err != nil {
		t.Fatal(err)
	}
	r := sendJSON(t, e.s, e.writer, "/api/people/Ghost/rename?repo="+e.repoID, map[string]string{"name": "Spirit"})
	if r.Code != http.StatusOK {
		t.Fatalf("rename: got %d (%s)", r.Code, r.Body.String())
	}
	if p := e.db.GetInRepo(e.repoID, "Ghost"); p != nil {
		t.Errorf("old name should be gone")
	}
	p := e.db.GetInRepo(e.repoID, "Spirit")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("unexpected renamed person: %+v", p)
	}
}

// TestDeletePersonRemovesFolder checks that deleting a person of a repo
// also removes their dataset folder (people/<repoID>/<Name>), so a rescan
// cannot re-enroll them.
func TestDeletePersonRemovesFolder(t *testing.T) {
	e := newEnv(t, &stubEngine{}, "Team", true, false)
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Alice", "a/1.jpg", db.HashBytes([]byte("x")), []float32{1}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Alice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := del(t, e.s, e.writer, "/api/people/Alice?repo="+e.repoID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		FolderRemoved bool `json:"folder_removed"`
	}
	decodeBody(t, rec, &resp)
	if !resp.FolderRemoved {
		t.Errorf("folder_removed = false, want true")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("people folder still exists after delete")
	}
	if e.db.GetInRepo(e.repoID, "Alice") != nil {
		t.Errorf("Alice should be removed from the DB")
	}

	// A bad name is rejected before anything happens.
	if rec := del(t, e.s, e.writer, "/api/people/.hidden?repo="+e.repoID); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: got %d, want 400", rec.Code)
	}
}

// TestThumbChooser covers serving enrolled photos (writer-only) and
// regenerating the thumbnail from a chosen photo.
func TestThumbChooser(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	imgA, imgB := pngBytes(t, 120, 120, 0), pngBytes(t, 160, 90, 100)
	photoA := db.HashBytes(imgA)[:12] + ".png"
	photoB := db.HashBytes(imgB)[:12] + ".png"

	// One upload per request so the thumbnail source is deterministic.
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a1.png": imgA}); rec.Code != http.StatusOK {
		t.Fatalf("upload a1: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a2.png": imgB}); rec.Code != http.StatusOK {
		t.Fatalf("upload a2: got %d (%s)", rec.Code, rec.Body.String())
	}
	p := e.db.GetInRepo(e.repoID, "Alice")
	if p.ThumbSrc != photoA {
		t.Fatalf("initial ThumbSrc = %q, want %q (first upload wins)", p.ThumbSrc, photoA)
	}
	thumbPath := filepath.Join(e.db.ThumbDir(), p.Thumb)
	thumbBefore, err := os.ReadFile(thumbPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	// Serving an enrolled photo returns the stored bytes as image/png.
	r := getJSON(t, e.s, e.writer, "/api/people/Alice/photos/"+photoB+"?repo="+e.repoID, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("photo serve: got %d", r.Code)
	}
	if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("photo content type %q, want image/png", ct)
	}
	if !bytes.Equal(r.Body.Bytes(), imgB) {
		t.Errorf("served photo bytes differ from the stored upload")
	}

	// Choose photo B as the thumbnail source.
	type thumbResp struct {
		Person string `json:"person"`
		Thumb  string `json:"thumb"`
	}
	r = sendJSON(t, e.s, e.writer, "/api/people/Alice/thumbnail?repo="+e.repoID, map[string]string{"photo": photoB})
	if r.Code != http.StatusOK {
		t.Fatalf("select thumb: got %d (%s)", r.Code, r.Body.String())
	}
	var tr thumbResp
	decodeBody(t, r, &tr)
	wantURL := "/api/thumbs/" + p.ID + ".jpg?v=" + db.HashBytes([]byte(photoB))[:8]
	if tr.Thumb != wantURL {
		t.Errorf("thumb URL = %q, want %q", tr.Thumb, wantURL)
	}
	p = e.db.GetInRepo(e.repoID, "Alice")
	if p.ThumbSrc != photoB {
		t.Errorf("ThumbSrc = %q, want %q", p.ThumbSrc, photoB)
	}
	thumbAfter, _ := os.ReadFile(thumbPath)
	if bytes.Equal(thumbBefore, thumbAfter) {
		t.Errorf("sidecar was not regenerated from the chosen photo")
	}
	// People list carries the updated cache-busted URL.
	var people struct {
		People []struct {
			ID    string `json:"id"`
			Thumb string `json:"thumb"`
		} `json:"people"`
	}
	getJSON(t, e.s, e.writer, "/api/people?repo="+e.repoID, &people)
	for _, pp := range people.People {
		if pp.ID == p.ID && pp.Thumb != wantURL {
			t.Errorf("people list thumb = %q, want %q", pp.Thumb, wantURL)
		}
	}

	// Errors: unknown photo, unenrolled/unsafe path, missing file, unknown
	// person, bad name, wrong methods.
	if code := getJSON(t, e.s, e.writer, "/api/people/Alice/photos/nope.png?repo="+e.repoID, nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown photo: got %d, want 404", code)
	}
	if code := getJSON(t, e.s, e.writer, "/api/people/Nobody/photos/"+photoB+"?repo="+e.repoID, nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown person photo: got %d, want 404", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/people/Alice/photos/"+photoB+"?repo="+e.repoID, nil)
	req.AddCookie(e.writer)
	r3 := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(r3, req)
	if r3.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST photo: got %d, want 405", r3.Code)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alice/thumbnail?repo="+e.repoID, map[string]string{"photo": "nope.png"}).Code; code != http.StatusNotFound {
		t.Errorf("unknown photo select: got %d, want 404", code)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alice/thumbnail?repo="+e.repoID, map[string]string{"photo": "../../etc/passwd"}).Code; code != http.StatusNotFound {
		t.Errorf("unsafe path select: got %d, want 404", code)
	}
	// Enrolled in the DB but the file is gone from disk.
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Alice", "ghost.jpg", db.HashBytes(imgA), []float32{1}); err != nil {
		t.Fatal(err)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Alice/thumbnail?repo="+e.repoID, map[string]string{"photo": "ghost.jpg"}).Code; code != http.StatusNotFound {
		t.Errorf("missing file select: got %d, want 404", code)
	}
	if code := sendJSON(t, e.s, e.writer, "/api/people/Nobody/thumbnail?repo="+e.repoID, map[string]string{"photo": "x.jpg"}).Code; code != http.StatusNotFound {
		t.Errorf("unknown person select: got %d, want 404", code)
	}
	if code := sendJSON(t, e.s, e.writer, `/api/people/a\b/thumbnail?repo=`+e.repoID, map[string]string{"photo": "x.jpg"}).Code; code != http.StatusBadRequest {
		t.Errorf("bad name select: got %d, want 400", code)
	}
	if code := getJSON(t, e.s, e.writer, "/api/people/Alice/thumbnail?repo="+e.repoID, nil).Code; code != http.StatusMethodNotAllowed {
		t.Errorf("GET select: got %d, want 405", code)
	}
}

// TestThumbChooserNoFace checks that a photo without a detectable face is
// rejected and leaves the current thumbnail untouched.
func TestThumbChooserNoFace(t *testing.T) {
	e := newEnv(t, &stubEngine{faces: nil}, "Team", true, false)
	img := pngBytes(t, 60, 60, 0)
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Bob", "b.png", db.HashBytes(img), []float32{1}); err != nil {
		t.Fatal(err)
	}
	// The photo file must exist on disk; only detection fails.
	if err := os.MkdirAll(filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Bob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Bob", "b.png"), img, 0o644); err != nil {
		t.Fatal(err)
	}
	r := sendJSON(t, e.s, e.writer, "/api/people/Bob/thumbnail?repo="+e.repoID, map[string]string{"photo": "b.png"})
	if r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no-face select: got %d (%s), want 422", r.Code, r.Body.String())
	}
	if p := e.db.GetInRepo(e.repoID, "Bob"); p.Thumb != "" {
		t.Errorf("failed selection must not create a thumbnail")
	}
}

// TestPhotoDetect covers inspecting an enrolled photo's detected faces via
// the /detect subroute (used by the photos manager modal).
func TestPhotoDetect(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{
		{BBox: [4]float64{10, 10, 40, 40}, Name: "Alice", PersonID: "alice", Confidence: 0.9},
	}}
	e := newEnv(t, eng, "Team", true, false)
	img := pngBytes(t, 120, 120, 0)
	photo := db.HashBytes(img)[:12] + ".png"
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a.png": img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d (%s)", rec.Code, rec.Body.String())
	}

	r := getJSON(t, e.s, e.reader, "/api/people/Alice/photos/"+photo+"/detect?repo="+e.repoID, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("detect: got %d (%s)", r.Code, r.Body.String())
	}
	var resp struct {
		Photo string        `json:"photo"`
		Count int           `json:"count"`
		Faces []engine.Face `json:"faces"`
	}
	decodeBody(t, r, &resp)
	if resp.Photo != photo || resp.Count != 1 || len(resp.Faces) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Faces[0].Name != "Alice" {
		t.Errorf("face identity = %q, want Alice", resp.Faces[0].Name)
	}
	if strings.Contains(r.Body.String(), "embedding") {
		t.Errorf("detect response must not leak embeddings")
	}

	// Unknown photo / missing file / unknown person / bad method.
	if code := getJSON(t, e.s, e.reader, "/api/people/Alice/photos/nope.png/detect?repo="+e.repoID, nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown photo: got %d, want 404", code)
	}
	// Enrolled in the DB but the file is gone from disk.
	if err := e.db.AddPhotoHashedForRepo(e.repoID, "Alice", "ghost.jpg", db.HashBytes(img), []float32{1}); err != nil {
		t.Fatal(err)
	}
	if code := getJSON(t, e.s, e.reader, "/api/people/Alice/photos/ghost.jpg/detect?repo="+e.repoID, nil).Code; code != http.StatusNotFound {
		t.Errorf("missing file: got %d, want 404", code)
	}
	if code := getJSON(t, e.s, e.reader, "/api/people/Nobody/photos/"+photo+"/detect?repo="+e.repoID, nil).Code; code != http.StatusNotFound {
		t.Errorf("unknown person: got %d, want 404", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/people/Alice/photos/"+photo+"/detect?repo="+e.repoID, nil)
	req.AddCookie(e.reader)
	r2 := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(r2, req)
	if r2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST detect: got %d, want 405", r2.Code)
	}
}

// TestPhotoDelete covers removing a single photo: DB entry and file are
// gone, the other photo survives, and errors match the rest of the photo
// routes.
func TestPhotoDelete(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	imgA, imgB := pngBytes(t, 120, 120, 0), pngBytes(t, 120, 120, 7)
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a1.png": imgA}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a1: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a2.png": imgB}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a2: got %d (%s)", rec.Code, rec.Body.String())
	}
	photoA := db.HashBytes(imgA)[:12] + ".png"

	rec := del(t, e.s, e.writer, "/api/people/Alice/photos/"+photoA+"?repo="+e.repoID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Removed string `json:"removed"`
		Photos  int    `json:"photos"`
	}
	decodeBody(t, rec, &resp)
	if resp.Removed != photoA || resp.Photos != 1 {
		t.Errorf("unexpected delete response: %+v", resp)
	}
	p := e.db.GetInRepo(e.repoID, "Alice")
	if p == nil || len(p.Photos) != 1 {
		t.Fatalf("expected Alice with 1 remaining photo, got %+v", p)
	}
	if _, err := os.Stat(filepath.Join(e.s.cfg.PeopleDir, e.repoID, "Alice", photoA)); !os.IsNotExist(err) {
		t.Errorf("photo file should be gone from the people folder, err=%v", err)
	}

	if code := del(t, e.s, e.writer, "/api/people/Alice/photos/nope.png?repo="+e.repoID).Code; code != http.StatusNotFound {
		t.Errorf("unknown photo delete: got %d, want 404", code)
	}
	if code := do(t, e.s, http.MethodPost, "/api/people/Alice/photos/"+photoA+"?repo="+e.repoID, nil, "", e.writer, nil).Code; code != http.StatusMethodNotAllowed {
		t.Errorf("POST to delete route: got %d, want 405", code)
	}
}

// TestPhotoDeleteThumbnail checks that deleting the thumbnail source photo
// regenerates the avatar from another photo, and clearing the last photo
// removes the thumbnail entirely while the person survives.
func TestPhotoDeleteThumbnail(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	e := newEnv(t, eng, "Team", true, false)
	imgA, imgB := pngBytes(t, 120, 120, 0), pngBytes(t, 160, 90, 100)

	// One upload per request so the thumbnail source is deterministic.
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a1.png": imgA}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a1: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"a2.png": imgB}); rec.Code != http.StatusOK {
		t.Fatalf("enroll a2: got %d (%s)", rec.Code, rec.Body.String())
	}
	photoA := db.HashBytes(imgA)[:12] + ".png"
	photoB := db.HashBytes(imgB)[:12] + ".png"
	p := e.db.GetInRepo(e.repoID, "Alice")
	if p.ThumbSrc != photoA {
		t.Fatalf("ThumbSrc = %q, want %q", p.ThumbSrc, photoA)
	}

	// Delete the thumbnail source: avatar regenerates from the other photo.
	if code := del(t, e.s, e.writer, "/api/people/Alice/photos/"+photoA+"?repo="+e.repoID).Code; code != http.StatusOK {
		t.Fatalf("delete thumb-src photo: got %d", code)
	}
	p = e.db.GetInRepo(e.repoID, "Alice")
	if p.ThumbSrc != photoB {
		t.Errorf("ThumbSrc = %q, want %q (regenerated from the remaining photo)", p.ThumbSrc, photoB)
	}
	if p.Thumb == "" {
		t.Errorf("thumbnail sidecar should have been regenerated, not cleared")
	}
	if _, err := os.Stat(filepath.Join(e.db.ThumbDir(), p.Thumb)); err != nil {
		t.Errorf("regenerated sidecar missing: %v", err)
	}

	// Delete the last photo: thumbnail cleared, person stays enrolled.
	if code := del(t, e.s, e.writer, "/api/people/Alice/photos/"+photoB+"?repo="+e.repoID).Code; code != http.StatusOK {
		t.Fatalf("delete last photo: got %d", code)
	}
	p = e.db.GetInRepo(e.repoID, "Alice")
	if p == nil {
		t.Fatalf("person should remain after removing their last photo")
	}
	if p.Thumb != "" || p.ThumbSrc != "" {
		t.Errorf("thumbnail should be cleared, got thumb=%q src=%q", p.Thumb, p.ThumbSrc)
	}
	if _, err := os.Stat(filepath.Join(e.db.ThumbDir(), p.ID+".jpg")); !os.IsNotExist(err) {
		t.Errorf("sidecar file should be removed, err=%v", err)
	}
}

// TestEnrollPersonWriteFailure: a read-only people dir answers 422 and
// leaves no DB entry.
func TestEnrollPersonWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	e := newEnv(t, &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}, "Team", true, false)
	if err := os.MkdirAll(filepath.Join(e.s.cfg.PeopleDir, e.repoID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(filepath.Join(e.s.cfg.PeopleDir, e.repoID), 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(e.s.cfg.PeopleDir, e.repoID), 0o755) })

	rec := enrollPerson(t, e.s, e.writer, e.repoID, "Alice", map[string][]byte{"x.jpg": []byte("img")})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when the people dir is read-only, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "people folder") {
		t.Errorf("failure should mention the people folder, got %s", rec.Body.String())
	}
	if e.db.GetInRepo(e.repoID, "Alice") != nil {
		t.Errorf("failed persistence must not leave a DB entry")
	}
}
