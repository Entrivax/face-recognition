package api

// The access matrix: which actor — anonymous, reader, writer, admin, valid
// share token, wrong-repo share token, disabled share, authenticated user
// without a grant (and with a share header) — may call which scoped route,
// and with which status. These tests pin access.go's resolver contract end
// to end; the per-handler behaviour tests live in the other test files.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recogn/internal/db"
	"recogn/internal/engine"
)

// matrixEnv is the fixture every matrix case runs against:
//
//   - repo "Team"  (share ENABLED, token e.shareToken): person Alice,
//   - repo "Other" (share disabled):                    person Carol,
//   - users: admin, writer (Team: writer), reader (Team: reader), outsider
//     (reader on Other only), plus a share token valid for Team.
type matrixEnv struct {
	e        *env
	other    string
	outsider *http.Cookie
	alice    string // Alice's photo path (also usable for Carol)
	img      []byte
}

func newMatrixEnv(t *testing.T) *matrixEnv {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{10, 10, 40, 40}}}}
	m := &matrixEnv{e: newEnv(t, eng, "Team", true, true)}
	m.other = seedRepo(t, m.e.s, m.e.admin, "Other")
	m.img = pngBytes(t, 100, 100, 0)
	if rec := enrollPerson(t, m.e.s, m.e.writer, m.e.repoID, "Alice", map[string][]byte{"a.png": m.img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll alice: got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := enrollPerson(t, m.e.s, m.e.admin, m.other, "Carol", map[string][]byte{"c.png": m.img}); rec.Code != http.StatusOK {
		t.Fatalf("enroll carol: got %d (%s)", rec.Code, rec.Body.String())
	}
	m.alice = db.HashBytes(m.img)[:12] + ".png"
	// An ungranted-on-Team user.
	m.outsider = seedRoleUser(t, m.e.s, m.e.admin, "noah", "outsider-password", m.other, "reader")
	return m
}

// actor headers for the matrix columns.
func (m *matrixEnv) shareHdr(e *env) map[string]string { return shareHdr(e.repoID, e.shareToken) }
func (m *matrixEnv) otherShareHdr() map[string]string  { return shareHdr(m.other, m.e.shareToken) }

// matrixRow is one route × the status each actor must get.
type matrixRow struct {
	name string
	// call runs the probe for the given environment.
	call func(t *testing.T, m *matrixEnv, cookie *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder

	anonWant     int // anonymous (no credential)
	shareWant    int // valid share token for the route's repo
	otherShare   int // share token for ANOTHER repo
	readerWant   int // reader on the repo
	writerWant   int // writer on the repo
	outsiderWant int // authenticated, no grant on the repo
	adminWant    int // admin
}

// runMatrix walks the table; every cell is asserted.
func runMatrix(t *testing.T, rows []matrixRow) {
	t.Helper()
	m := newMatrixEnv(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cells := []struct {
				name   string
				cookie *http.Cookie
				hdr    map[string]string
				want   int
			}{
				{"anonymous", nil, nil, row.anonWant},
				{"share", nil, m.shareHdr(m.e), row.shareWant},
				{"wrong-token", nil, shareHdr(m.e.repoID, "not-the-token"), row.otherShare},
				{"share-other-repo", nil, m.otherShareHdr(), row.otherShare},
				{"reader", m.e.reader, nil, row.readerWant},
				{"writer", m.e.writer, nil, row.writerWant},
				{"outsider", m.outsider, nil, row.outsiderWant},
				{"admin", m.e.admin, nil, row.adminWant},
			}
			for _, cell := range cells {
				if cell.want < 0 {
					continue // not defined for this route
				}
				rec := row.call(t, m, cell.cookie, cell.hdr)
				if rec.Code != cell.want {
					t.Errorf("%s: got %d (%s), want %d", cell.name, rec.Code, rec.Body.String(), cell.want)
				}
			}
		})
	}
}

// TestAccessMatrixPinsRoles is the route table. Route notes:
//
//   - readers (incl. share tokens): people list, person details, recognize,
//     photo detect, thumbs.
//   - writers (not share tokens): rescan, person mutations, photos manager,
//     full-res photos.
//   - 401 (not 403) for anonymous-without-share everywhere; 403 for
//     authenticated-but-insufficient.
func TestAccessMatrixPinsRoles(t *testing.T) {
	rows := []matrixRow{
		{
			name: "people list",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodGet, "/api/people?repo="+m.e.repoID, nil, "", c, h)
			},
			anonWant: 401, shareWant: 200, otherShare: 401, readerWant: 200, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "person details",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodGet, "/api/people/Alice?repo="+m.e.repoID, nil, "", c, h)
			},
			anonWant: 401, shareWant: 200, otherShare: 401, readerWant: 200, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "recognize",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodPost, "/api/recognize?repo="+m.e.repoID, strings.NewReader("img"), "", c, h)
			},
			anonWant: 401, shareWant: 200, otherShare: 401, readerWant: 200, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "photo detect",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodGet, "/api/people/Alice/photos/"+m.alice+"/detect?repo="+m.e.repoID, nil, "", c, h)
			},
			anonWant: 401, shareWant: 200, otherShare: 401, readerWant: 200, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "thumb",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				p := m.e.db.GetInRepo(m.e.repoID, "Alice")
				return do(t, m.e.s, http.MethodGet, "/api/thumbs/"+p.ID+".jpg", nil, "", c, h)
			},
			anonWant: 401, shareWant: 200, otherShare: 401, readerWant: 200, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "rescan",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodPost, "/api/enroll?repo="+m.e.repoID, nil, "", c, h)
			},
			anonWant: 401, shareWant: 403, otherShare: 401, readerWant: 403, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "full-res photo",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodGet, "/api/people/Alice/photos/"+m.alice+"?repo="+m.e.repoID, nil, "", c, h)
			},
			anonWant: 401, shareWant: 403, otherShare: 401, readerWant: 403, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "photos manager list",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodGet, "/api/people/Alice/photos?repo="+m.e.repoID, nil, "", c, h)
			},
			anonWant: 401, shareWant: 403, otherShare: 401, readerWant: 403, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "meta write",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodPost, "/api/people/Alice/meta?repo="+m.e.repoID,
					strings.NewReader(`{"aliases":["A"]}`), "application/json", c, h)
			},
			anonWant: 401, shareWant: 403, otherShare: 401, readerWant: 403, writerWant: 200, outsiderWant: 403, adminWant: 200,
		},
		{
			name: "person delete",
			call: func(t *testing.T, m *matrixEnv, c *http.Cookie, h map[string]string) *httptest.ResponseRecorder {
				return do(t, m.e.s, http.MethodDelete, "/api/people/Nobody?repo="+m.e.repoID, nil, "", c, h)
			},
			anonWant: 401, shareWant: 403, otherShare: 401, readerWant: 403, writerWant: 404, outsiderWant: 403, adminWant: 404,
		},
	}
	runMatrix(t, rows)
}

// TestAccessErrorsAreUniform pins the refusal wording: anonymous gets the
// 401 with the share-token hint on scoped routes; authenticated callers
// with an insufficient role get the role-specific 403; RequireAdmin routes
// answer "administrator access required".
func TestAccessErrorsAreUniform(t *testing.T) {
	m := newMatrixEnv(t)

	rec := do(t, m.e.s, http.MethodGet, "/api/people?repo="+m.e.repoID, nil, "", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon people list: got %d", rec.Code)
	}
	var body map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if !strings.HasPrefix(body["error"], "authentication required") || !strings.Contains(body["error"], "share token") {
		t.Errorf("401 body must hint at share tokens: %q", body["error"])
	}

	// Outsider (authenticated, no grant) → 403 with the read wording.
	rec = do(t, m.e.s, http.MethodGet, "/api/people?repo="+m.e.repoID, nil, "", m.outsider, nil)
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusForbidden || body["error"] != "read access to this repository is required" {
		t.Fatalf("outsider people list: got %d (%s)", rec.Code, body["error"])
	}

	// Reader on a writer route → 403 with the write wording.
	rec = do(t, m.e.s, http.MethodPost, "/api/enroll?repo="+m.e.repoID, nil, "", m.e.reader, nil)
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusForbidden || body["error"] != "write access to this repository is required" {
		t.Fatalf("reader rescan: got %d (%s)", rec.Code, body["error"])
	}

	// Share token on a writer route → the same 403.
	rec = do(t, m.e.s, http.MethodPost, "/api/enroll?repo="+m.e.repoID, nil, "", nil, m.shareHdr(m.e))
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusForbidden || body["error"] != "write access to this repository is required" {
		t.Fatalf("share rescan: got %d (%s)", rec.Code, body["error"])
	}

	// Admin-only route, authenticated non-admin → "administrator access required".
	rec = do(t, m.e.s, http.MethodPost, "/api/repos", strings.NewReader(`{"name":"X"}`), "application/json", m.e.reader, nil)
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusForbidden || body["error"] != "administrator access required" {
		t.Fatalf("reader create repo: got %d (%s)", rec.Code, body["error"])
	}
}

// TestShareTokenSemantics pins the share-token resolver: the header format,
// the enabled check, the constant-time hash comparison and the one-repo
// scoping.
func TestShareTokenSemantics(t *testing.T) {
	m := newMatrixEnv(t)
	e := m.e

	// A disabled share never grants (Other is not shared).
	if _, ok := e.s.validShare(shareReq(m.other, "whatever-token"), m.other); ok {
		t.Errorf("disabled share must not validate")
	}

	// A valid token for Team validates, and its repo is the canonical ID.
	gotRepo, ok := e.s.validShare(shareReq(e.repoID, e.shareToken), e.repoID)
	if !ok || gotRepo != e.repoID {
		t.Fatalf("valid share: repo=%q ok=%v", gotRepo, ok)
	}

	// Wrong token → no access.
	if _, ok := e.s.validShare(shareReq(e.repoID, "not-the-token"), e.repoID); ok {
		t.Errorf("wrong token must not validate")
	}

	// A share token for another repo is refused when a repo is required.
	if _, ok := e.s.validShare(shareReq(m.other, e.shareToken), e.repoID); ok {
		t.Errorf("cross-repo token must not validate")
	}

	// Garbage header shapes: no slash, empty parts.
	for _, h := range []string{"", "noseparator", "/", "/token", "repo/", "  "} {
		req := httptest.NewRequest(http.MethodGet, "/api/people", nil)
		req.Header.Set("X-Share-Token", h)
		if _, ok := e.s.validShare(req, e.repoID); ok {
			t.Errorf("garbage share header %q must not validate", h)
		}
	}

	// A share principal gets reader-only — writer routes refuse.
	rec := do(t, e.s, http.MethodPost, "/api/people/Alice/rename?repo="+e.repoID,
		strings.NewReader(`{"name":"X"}`), "application/json", nil, shareHdr(e.repoID, e.shareToken))
	if rec.Code != http.StatusForbidden {
		t.Errorf("share rename: got %d, want 403", rec.Code)
	}

	// An authenticated user does NOT fall back to a share header: no grant
	// on the repo → 403 even with a valid token in the header.
	rec = do(t, e.s, http.MethodGet, "/api/people?repo="+e.repoID, nil, "", m.outsider, shareHdr(e.repoID, e.shareToken))
	if rec.Code != http.StatusForbidden {
		t.Errorf("outsider with valid share header: got %d (%s), want 403", rec.Code, rec.Body.String())
	}
}

// shareReq builds a request with an X-Share-Token header of "repo/token".
func shareReq(repoID, token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/people", nil)
	req.Header.Set("X-Share-Token", repoID+"/"+token)
	return req
}

// TestRepoParamAcceptsNameAndID pins the ?repo= canonicalization: display
// names resolve to the same grant as the ID, unknown values grant nothing.
func TestRepoParamAcceptsNameAndID(t *testing.T) {
	m := newMatrixEnv(t)
	e := m.e

	// By name: the reader is still granted.
	if rec := do(t, e.s, http.MethodGet, "/api/people?repo=Team", nil, "", e.reader, nil); rec.Code != http.StatusOK {
		t.Errorf("repo by name: got %d (%s)", rec.Code, rec.Body.String())
	}
	// By ID (case-insensitive): granted.
	if rec := do(t, e.s, http.MethodGet, "/api/people?repo="+strings.ToUpper(e.repoID), nil, "", e.reader, nil); rec.Code != http.StatusOK {
		t.Errorf("repo by uppercased ID: got %d", rec.Code)
	}
	// Unknown value: no access.
	if rec := do(t, e.s, http.MethodGet, "/api/people?repo=nosuchrepo", nil, "", e.reader, nil); rec.Code != http.StatusForbidden {
		t.Errorf("unknown repo: got %d (%s), want 403", rec.Code, rec.Body.String())
	}

	// Share header repo IDs are matched case-insensitively too.
	rec := do(t, e.s, http.MethodGet, "/api/people?repo="+strings.ToUpper(e.repoID), nil, "", nil, shareHdr(strings.ToUpper(e.repoID), e.shareToken))
	if rec.Code != http.StatusOK {
		t.Errorf("share with uppercased repo: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// TestDisabledShareRefused pins that disabling the share link invalidates
// the token immediately.
func TestDisabledShareRefused(t *testing.T) {
	m := newMatrixEnv(t)
	e := m.e

	rec := do(t, e.s, http.MethodGet, "/api/people?repo="+e.repoID, nil, "", nil, shareHdr(e.repoID, e.shareToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("share access before disable: got %d", rec.Code)
	}
	if rec := sendJSON(t, e.s, e.admin, "/api/repos/"+e.repoID+"/share", map[string]bool{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("disable share: got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, e.s, http.MethodGet, "/api/people?repo="+e.repoID, nil, "", nil, shareHdr(e.repoID, e.shareToken))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("share access after disable: got %d, want 401", rec.Code)
	}
	// And the stored token is cleared.
	repo, _ := e.db.GetRepo(e.repoID)
	if repo.Shared || repo.ShareToken != "" {
		t.Errorf("repo still marked shared: %+v", repo)
	}
}

// TestRecognizeEmptyRepoAllUnknown pins that recognizing against an empty
// repository sends an EMPTY (non-nil) allowed set to the engine — every
// face is unknown, no legacy identity can leak in.
func TestRecognizeEmptyRepoAllUnknown(t *testing.T) {
	eng := &stubEngine{faces: []engine.Face{{BBox: [4]float64{0, 0, 10, 10}}}}
	e := newEnv(t, eng, "Empty", true, false)

	body, ct := multipartBody(t, "image", "photo.png", pngBytes(t, 40, 40, 0))
	if rec := do(t, e.s, http.MethodPost, "/api/recognize?repo="+e.repoID, body, ct, e.reader, nil); rec.Code != http.StatusOK {
		t.Fatalf("recognize: got %d (%s)", rec.Code, rec.Body.String())
	}
	allowed := eng.recordedAllowed()
	if allowed == nil {
		t.Fatalf("allowed set must be non-nil (empty), got nil")
	}
	if len(allowed) != 0 {
		t.Errorf("allowed set = %v, want empty", allowed)
	}
}
