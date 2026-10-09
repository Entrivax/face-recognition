package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"recogn/internal/db"
	"recogn/internal/enroll"
)

// Limits for the metadata fields (POST /api/people/{name}/meta). The JSON
// body is capped at 1 MiB like the other JSON routes; these bounds keep the
// individual fields sane on top of that.
const (
	maxAliases    = 20
	maxAliasChars = 120
	maxURLs       = 50
	maxURLChars   = 2048
	maxDescrChars = 20_000
	minYear       = 1
	maxYear       = 2100
)

// metaRequest mirrors the POST /api/people/{name}/meta body. All fields are
// optional; the handler has full-replace semantics, so an omitted or empty
// field clears the stored value.
type metaRequest struct {
	Aliases     []string  `json:"aliases"`
	Birth       *birthReq `json:"birthdate"`
	URLs        []string  `json:"urls"`
	Description string    `json:"description"`
}

type birthReq struct {
	Year  *int `json:"year"`
	Month *int `json:"month"`
	Day   *int `json:"day"`
}

// normalize validates and trims the request into a stored meta value:
// aliases are deduped case-insensitively (first spelling wins), URLs must be
// absolute http(s) links (so the UI can link them safely), the birthdate
// components are range-checked (a day without a month is refused; a complete
// date must exist on the calendar). Returns nil when nothing remains — a
// fully-empty request clears the metadata.
func (b *metaRequest) normalize() (*db.PersonMeta, error) {
	aliases := make([]string, 0, len(b.Aliases))
	seenAlias := make(map[string]bool, len(b.Aliases))
	for i, a := range b.Aliases {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if utf8.RuneCountInString(a) > maxAliasChars {
			return nil, fmt.Errorf("alias %d exceeds %d characters", i+1, maxAliasChars)
		}
		key := strings.ToLower(a)
		if seenAlias[key] {
			continue
		}
		seenAlias[key] = true
		aliases = append(aliases, a)
	}
	if len(aliases) > maxAliases {
		return nil, fmt.Errorf("too many aliases (max %d)", maxAliases)
	}

	urls := make([]string, 0, len(b.URLs))
	seenURL := make(map[string]bool, len(b.URLs))
	for i, raw := range b.URLs {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if utf8.RuneCountInString(u) > maxURLChars {
			return nil, fmt.Errorf("URL %d exceeds %d characters", i+1, maxURLChars)
		}
		parsed, err := url.Parse(u)
		if err != nil {
			return nil, fmt.Errorf("URL %d is not a valid URL", i+1)
		}
		scheme := strings.ToLower(parsed.Scheme)
		if (scheme != "http" && scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("URL %d must be an absolute http(s) link", i+1)
		}
		if seenURL[u] {
			continue
		}
		seenURL[u] = true
		urls = append(urls, u)
	}
	if len(urls) > maxURLs {
		return nil, fmt.Errorf("too many URLs (max %d)", maxURLs)
	}

	birth, err := b.Birth.normalize()
	if err != nil {
		return nil, err
	}

	if utf8.RuneCountInString(b.Description) > maxDescrChars {
		return nil, fmt.Errorf("description exceeds %d characters", maxDescrChars)
	}
	// Whitespace-only descriptions count as empty (so an otherwise-empty
	// request clears the whole record); real text is stored verbatim.
	description := b.Description
	if strings.TrimSpace(description) == "" {
		description = ""
	}

	meta := db.PersonMeta{Aliases: aliases, Birth: birth, URLs: urls, Description: description}
	if meta.IsEmpty() {
		return nil, nil
	}
	return &meta, nil
}

// normalize validates the partial birthdate: year 1–2100, month 1–12, day
// 1–31; a day requires a month or a year (the UI's dashed template cannot
// express a bare day — YYYY--DD is year+day), and a complete date must exist
// on the calendar (catches Feb 30). Partial info stays lenient — Feb 29 with
// a known month but unknown year is accepted as-is.
func (b *birthReq) normalize() (*db.BirthDate, error) {
	if b == nil || (b.Year == nil && b.Month == nil && b.Day == nil) {
		return nil, nil
	}
	bd := db.BirthDate{Year: b.Year, Month: b.Month, Day: b.Day}
	if b.Year != nil && (*b.Year < minYear || *b.Year > maxYear) {
		return nil, fmt.Errorf("birthdate year must be between %d and %d", minYear, maxYear)
	}
	if b.Month != nil && (*b.Month < 1 || *b.Month > 12) {
		return nil, fmt.Errorf("birthdate month must be between 1 and 12")
	}
	if b.Day != nil {
		if b.Month == nil && b.Year == nil {
			return nil, fmt.Errorf("birthdate day requires a month or a year")
		}
		if *b.Day < 1 || *b.Day > 31 {
			return nil, fmt.Errorf("birthdate day must be between 1 and 31")
		}
	}
	if b.Year != nil && b.Month != nil && b.Day != nil {
		date := time.Date(*b.Year, time.Month(*b.Month), *b.Day, 0, 0, 0, 0, time.UTC)
		if date.Year() != *b.Year || int(date.Month()) != *b.Month || date.Day() != *b.Day {
			return nil, fmt.Errorf("birthdate is not a real calendar date")
		}
	}
	return &bd, nil
}

// handleSetMeta replaces a person's metadata (full-replace semantics): body
// {"aliases": [...], "birthdate": {"year","month","day"}, "urls": [...],
// "description": "..."} — omitted or empty fields clear the stored value.
// The reply is the refreshed public details document. Metadata does not
// affect recognition, so no engine reload happens.
func (s *Server) handleSetMeta(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := enroll.CheckName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body metaRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	meta, err := body.normalize()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := s.db.Get(name)
	if p == nil {
		writeErr(w, http.StatusNotFound, "person not found")
		return
	}
	upd, err := s.db.SetMeta(p.Name, meta)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, personDetailDoc(upd))
}
