// Package engine owns the face-recognition pipeline: detect faces in an
// image, align each to 112x112, compute embeddings, and match them against a
// set of known identities. Inference runs in-process via CGO + the ONNX
// Runtime C API (see internal/onnxrt); this file holds the pure-Go logic
// around it.
package engine

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // register decoders
	"image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Face is a single detected face with its detection metadata and, after
// embedding+matching, the recognition outcome.
type Face struct {
	BBox      [4]float64   `json:"bbox"`      // x, y, width, height
	Score     float64      `json:"score"`     // detector confidence 0..1
	Landmarks [][2]float64 `json:"landmarks"` // 5-point landmarks (eyes, nose, mouth corners)
	Embedding []float32    `json:"-"`         // 512-d, not serialised to clients by default

	// Recognition result (populated by Engine.Recognize).
	Name       string  `json:"name"`       // matched identity, or "unknown"
	PersonID   string  `json:"person_id"`  // matched person's ID, or ""
	Confidence float64 `json:"confidence"` // best cosine similarity 0..1
	// Matches lists every enrolled person whose best similarity cleared the
	// threshold, ranked best-first. Useful for spotting near-tied identities
	// (possible duplicate people). Empty when nothing matched.
	Matches []Match `json:"matches,omitempty"`
}

// Match describes one candidate identity for a face.
type Match struct {
	PersonID string  `json:"person_id"`
	Name     string  `json:"name"`
	Score    float64 `json:"score"`
}

// KnownPerson is the engine-facing view of an enrolled identity.
type KnownPerson struct {
	ID         string
	Name       string
	Embeddings [][]float32
}

// ArcFace reference 5-point landmarks for a 112x112 aligned crop.
var arcfaceTemplate = [5][2]float32{
	{38.2946, 51.6963},
	{73.5318, 51.5014},
	{56.0252, 71.7366},
	{41.5493, 92.3655},
	{70.7299, 92.2041},
}

// inferencer abstracts how faces are detected and embedded. The production
// implementation is the in-process CGO backend (onnxrt); the interface exists
// so the engine can be exercised with a stub in tests.
type inferencer interface {
	// detect finds all faces in a raw image (jpeg/png/webp/bmp/gif bytes),
	// decoding it internally.
	detect(imgBytes []byte) ([]Face, error)
	// detectFromImage finds all faces in an already-decoded image, so callers
	// that also align faces from the same image (Recognize) decode only once.
	detectFromImage(src image.Image) ([]Face, error)
	// embedImage computes the 512-d embedding of an aligned 112x112 face.
	embedImage(aligned *image.NRGBA) ([]float32, error)
	// embedBatch computes embeddings for several aligned faces, ideally in
	// one batched model Run. It returns one result per input index: a nil
	// embedding marks a face whose embedding failed (the caller reports it
	// like an embed error). An error return means the whole batch failed.
	embedBatch(aligned []*image.NRGBA) ([][]float32, error)
	// ping warms up the backend and verifies the models load.
	ping() error
	// close releases backend resources.
	close()
}

// Engine runs detection→alignment→embedding→matching. It is safe for
// concurrent use; the underlying inferencer bounds model Runs with a
// concurrency gate (see cgoInferencer) rather than serialising them.
type Engine struct {
	inf inferencer

	// mu guards known and thresh. HTTP traffic writes both while readers run:
	// POST /api/config → SetThreshold and every mutation handler's reload →
	// SetKnown race public recognize traffic. SetKnown is copy-on-write: it
	// replaces the slice wholesale and nothing ever mutates a published
	// backing array, so readers can iterate a snapshot taken under RLock.
	mu     sync.RWMutex
	known  []KnownPerson
	thresh float64
}

// New creates an Engine backed by the in-process CGO/ONNX-Runtime inferencer.
// detModel/embModel are paths to the ONNX models; thresh is the match
// threshold. It returns an error if the models cannot be loaded. The number
// of parallel inference streams defaults to DefaultConcurrency.
func New(detModel, embModel string, thresh float64) (*Engine, error) {
	return NewWithConcurrency(detModel, embModel, thresh, defaultConcurrency)
}

// NewWithConcurrency creates an Engine backed by the in-process
// CGO/ONNX-Runtime inferencer, allowing at most concurrency parallel model
// Runs (<=0 selects the default). Use it to trade CPU cores for request
// throughput; see DefaultConcurrency and config.RECOGN_CONCURRENCY.
func NewWithConcurrency(detModel, embModel string, thresh float64, concurrency int) (*Engine, error) {
	inf, err := newCGOInferencer(detModel, embModel, concurrency)
	if err != nil {
		return nil, err
	}
	return &Engine{inf: inf, thresh: thresh}, nil
}

// NewWithInferencer creates an Engine with an explicit inference backend
// (used by tests).
func NewWithInferencer(inf inferencer, thresh float64) *Engine {
	return &Engine{inf: inf, thresh: thresh}
}

// Close shuts the engine (and its inference backend) down.
func (e *Engine) Close() { e.inf.close() }

// SetThreshold updates the match threshold. Safe for concurrent use.
func (e *Engine) SetThreshold(t float64) {
	e.mu.Lock()
	e.thresh = t
	e.mu.Unlock()
}

// Threshold returns the current match threshold. Safe for concurrent use.
func (e *Engine) Threshold() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.thresh
}

// SetKnown replaces the in-memory identity set. The caller must not mutate
// people (or its Embeddings) afterwards: the engine publishes the slice
// as-is and relies on its immutability (copy-on-write). Production callers
// build a fresh slice per call. Safe for concurrent use.
func (e *Engine) SetKnown(people []KnownPerson) {
	e.mu.Lock()
	e.known = people
	e.mu.Unlock()
}

// Known returns the current identity set as a shallow copy, so callers may
// mutate the returned slice without touching the engine's set (element
// Embeddings are shared and must stay read-only). Safe for concurrent use.
func (e *Engine) Known() []KnownPerson {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]KnownPerson, len(e.known))
	copy(out, e.known)
	return out
}

// knownSnapshot returns the identity set for matching: the slice header is
// copied under RLock, and iterating it outside the lock is race-free because
// SetKnown only ever replaces the slice wholesale (copy-on-write contract on
// SetKnown), so a published backing array is never written again.
func (e *Engine) knownSnapshot() []KnownPerson {
	e.mu.RLock()
	known := e.known
	e.mu.RUnlock()
	return known
}

// Ping warms up the inference backend and verifies models load.
func (e *Engine) Ping() error { return e.inf.ping() }

// Detect finds all faces in an image (raw bytes of a jpeg/png/webp/bmp/gif).
func (e *Engine) Detect(imgBytes []byte) ([]Face, error) {
	return e.inf.detect(imgBytes)
}

// EmbedFace aligns a detected face and computes its embedding. imgBytes is the
// original full image; the face's landmarks drive the alignment crop.
func (e *Engine) EmbedFace(imgBytes []byte, f Face) ([]float32, error) {
	aligned, err := alignFaceImage(imgBytes, f)
	if err != nil {
		return nil, fmt.Errorf("align: %w", err)
	}
	return e.inf.embedImage(aligned)
}

// MatchEmbedding returns the best identity for an embedding plus whether it
// clears the threshold. The score is the max cosine similarity across every
// enrolled embedding of every person.
func (e *Engine) MatchEmbedding(emb []float32) (Match, bool) {
	best := Match{Score: -1}
	for _, m := range e.bestPerPerson(emb) {
		if m.Score > best.Score {
			best = m
		}
	}
	return best, best.Score >= e.Threshold()
}

// bestPerPerson computes every known person's best cosine similarity to emb
// (one entry per person, regardless of threshold). The query's own norm is
// computed once here (see cosineWithNorm) instead of once per enrolled
// embedding.
func (e *Engine) bestPerPerson(emb []float32) map[string]Match {
	known := e.knownSnapshot()
	best := make(map[string]Match, len(known))
	sqrtNa := sqrtNorm(emb)
	for _, p := range known {
		m := Match{PersonID: p.ID, Name: p.Name, Score: -1}
		for _, pe := range p.Embeddings {
			if s := cosineWithNorm(emb, pe, sqrtNa); s > m.Score {
				m.Score = s
			}
		}
		best[p.ID] = m
	}
	return best
}

// matchesAboveThreshold filters a bestPerPerson result to the entries whose
// score clears thresh, ranked best-first (ties broken by name). Shared by
// MatchAll and Recognize so Recognize needs only one bestPerPerson pass —
// previously the unknown-face path computed the map twice.
func matchesAboveThreshold(all map[string]Match, thresh float64) []Match {
	out := make([]Match, 0, len(all))
	for _, m := range all {
		if m.Score >= thresh {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// MatchAll returns every enrolled person whose best cosine similarity to emb
// clears the threshold, ranked best-first (ties broken by name). One entry
// per person: a person with several enrolled photos contributes only their
// best-scoring embedding.
func (e *Engine) MatchAll(emb []float32) []Match {
	return matchesAboveThreshold(e.bestPerPerson(emb), e.Threshold())
}

// LargestFace returns the face with the largest bounding-box area and whether
// any face was present. Enrollment and thumbnail selection both use the
// largest face (the most reliable signal).
func LargestFace(faces []Face) (Face, bool) {
	if len(faces) == 0 {
		return Face{}, false
	}
	best := faces[0]
	bestArea := best.BBox[2] * best.BBox[3]
	for _, f := range faces[1:] {
		if a := f.BBox[2] * f.BBox[3]; a > bestArea {
			best, bestArea = f, a
		}
	}
	return best, true
}

// Recognize runs the full pipeline on one image: detect every face, embed and
// match each, and annotate the returned faces with identities. Each face also
// carries Matches: every enrolled person above the threshold, ranked — useful
// for spotting near-tied identities (possible duplicate people).
//
// The image is decoded exactly once here and the decoded image is reused for
// both the detector's letterbox and every face's alignment crop (both used to
// decode independently, and per-face alignment before that re-decoded per
// face). The aligned faces go to the inferencer's embedBatch, which fans
// per-face Runs across the concurrency gate, so multi-face photos use
// several cores.
func (e *Engine) Recognize(imgBytes []byte) ([]Face, error) {
	// Decode once for detection and all alignments below. decodeLimited
	// gates the declared pixel count first: a decompression bomb would
	// otherwise allocate its full RGBA size here.
	src, err := decodeLimited(imgBytes)
	if err != nil {
		return nil, fmt.Errorf("decode source image: %w", err)
	}
	faces, err := e.inf.detectFromImage(src)
	if err != nil {
		return nil, err
	}
	if len(faces) == 0 {
		return faces, nil
	}
	// Align every face first (pure Go, no inference) so all faces can be
	// embedded in one batched Run. A face whose alignment fails keeps its nil
	// slot and is marked unknown below, exactly like an embed failure.
	aligned := make([]*image.NRGBA, len(faces))
	for i := range faces {
		a, err := alignFaceFromImage(src, faces[i])
		if err != nil {
			aligned[i] = nil // single bad crop shouldn't sink the whole photo
			continue
		}
		aligned[i] = a
	}
	embs, err := e.inf.embedBatch(aligned)
	if err != nil {
		// Whole-batch failure: fall back to per-face embedding.
		embs = make([][]float32, len(faces))
		for i, a := range aligned {
			if a == nil {
				continue
			}
			emb, ferr := e.inf.embedImage(a)
			if ferr != nil {
				continue
			}
			embs[i] = emb
		}
	}
	for i := range faces {
		emb := embs[i]
		if emb == nil {
			// Alignment or embedding failed for this face.
			faces[i].Name = "unknown"
			continue
		}
		faces[i].Embedding = emb
		// One bestPerPerson pass feeds both the threshold-filtered Matches and
		// (when nothing clears the threshold) the near-miss confidence hint.
		all := e.bestPerPerson(emb)
		matches := matchesAboveThreshold(all, e.Threshold())
		faces[i].Matches = matches
		if len(matches) > 0 {
			faces[i].Confidence = math.Max(0, matches[0].Score)
			faces[i].Name = matches[0].Name
			faces[i].PersonID = matches[0].PersonID
		} else {
			// Nothing cleared the threshold; keep the near-miss score as the
			// displayed confidence hint.
			best := -1.0
			for _, m := range all {
				if m.Score > best {
					best = m.Score
				}
			}
			faces[i].Confidence = math.Max(0, best)
			faces[i].Name = "unknown"
		}
	}
	return faces, nil
}

// Cosine returns the cosine similarity between two embeddings. Both are
// expected L2-normalised (ArcFace output), so this is a dot product, but we
// divide by norms anyway for safety.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// sqrtNorm returns sqrt(sum(a[i]^2)), accumulated in the same index order as
// the na accumulation inside Cosine, so hoisting it out of a per-embedding
// loop keeps the arithmetic bit-identical.
func sqrtNorm(a []float32) float64 {
	var na float64
	for i := range a {
		na += float64(a[i]) * float64(a[i])
	}
	return math.Sqrt(na)
}

// cosineWithNorm is Cosine(a, b) with a's norm precomputed via sqrtNorm.
// bestPerPerson uses it to pay the query norm once per face instead of once
// per enrolled embedding. Bit-identical to Cosine: the dot product and nb are
// accumulated in the same order, and the final division uses the same
// sqrt(na)*sqrt(nb) product — only the sqrt(na) operand is reused.
func cosineWithNorm(a, b []float32, sqrtNa float64) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if sqrtNa == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrtNa * math.Sqrt(nb))
}

// alignFaceImage warps the face described by f's 5-point landmarks into a
// 112x112 crop matching the ArcFace reference template, returning the image.
func alignFaceImage(imgBytes []byte, f Face) (*image.NRGBA, error) {
	src, err := decodeLimited(imgBytes)
	if err != nil {
		return nil, fmt.Errorf("decode source image: %w", err)
	}
	return alignFaceFromImage(src, f)
}

// alignFaceFromImage is alignFaceImage for an already-decoded source image,
// so callers that process many faces of one photo decode it only once.
func alignFaceFromImage(src image.Image, f Face) (*image.NRGBA, error) {
	if len(f.Landmarks) < 5 {
		// Fall back to a plain bbox crop when landmarks are unavailable.
		return cropBBoxImage(src, f.BBox)
	}

	// Estimate the similarity transform mapping the detected landmarks onto
	// the ArcFace template (Umeyama). We then render it with an affine warp.
	M := umeyama(f.Landmarks[:5], arcfaceTemplate)

	// Backward-warp: build the aligned image by sampling the source through
	// the inverse (dst->src) of the forward landmark transform.
	inv := invertAffine(M)
	return affineWarp(src, inv, 112, 112), nil
}

// cropBBoxImage extracts a padded bounding-box crop (fallback when landmarks
// are missing) and resizes to 112x112, returning the image.
func cropBBoxImage(src image.Image, bb [4]float64) (*image.NRGBA, error) {
	b := src.Bounds()
	x, y := int(bb[0]), int(bb[1])
	w, h := int(bb[2]), int(bb[3])
	// pad a little around the box
	padx, pady := w/8, h/8
	x0 := max(0, x-padx)
	y0 := max(0, y-pady)
	x1 := min(b.Max.X, x+w+padx)
	y1 := min(b.Max.Y, y+h+pady)
	if x1 <= x0 || y1 <= y0 {
		return nil, fmt.Errorf("empty crop")
	}
	cropped := cropImage(src, image.Rect(x0, y0, x1, y1))
	return resizeBilinear(cropped, 112, 112), nil
}

// FaceThumb renders a square display thumbnail cropped around the face
// described by f's bounding box: the crop side is 1.6x the larger bbox side
// (≈30% margin around the face), centered on the bbox center (clamped into
// the image), resized to size x size, and JPEG-encoded. It is meant for UI
// avatars — not for recognition, which uses the aligned ArcFace crop.
func FaceThumb(imgBytes []byte, f Face, size int) ([]byte, error) {
	src, err := decodeLimited(imgBytes)
	if err != nil {
		return nil, fmt.Errorf("decode source image: %w", err)
	}
	if size <= 0 {
		size = 160
	}
	if f.BBox[2] <= 0 || f.BBox[3] <= 0 {
		return nil, fmt.Errorf("degenerate face bbox")
	}
	b := src.Bounds()
	w, h := f.BBox[2], f.BBox[3]
	cx, cy := f.BBox[0]+w/2, f.BBox[1]+h/2
	side := 1.6 * math.Max(w, h)
	// Clamp the crop origin so the square stays inside the image where
	// possible; near the edges the crop shrinks (the resize stretches it).
	x0 := math.Max(float64(b.Min.X), math.Min(cx-side/2, float64(b.Max.X)-side))
	y0 := math.Max(float64(b.Min.Y), math.Min(cy-side/2, float64(b.Max.Y)-side))
	r := image.Rect(int(math.Round(x0)), int(math.Round(y0)),
		int(math.Round(x0+side)), int(math.Round(y0+side))).Intersect(b)
	if r.Dx() <= 0 || r.Dy() <= 0 {
		return nil, fmt.Errorf("empty face crop")
	}
	cropped := resizeBilinear(cropImage(src, r), size, size)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, cropped, &jpeg.Options{Quality: 92}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cropImage copies the given rectangle of src into a new image anchored at the
// origin.
func cropImage(src image.Image, r image.Rectangle) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	// Type-checked once per call, not once per pixel. The generic At/Set copy
	// at the bottom is kept for every other source type (e.g. *image.YCbCr
	// from JPEG) and for rects reaching outside the source, where At() yields
	// transparent zeros but direct Pix indexing would be invalid — per-pixel
	// semantics must not change in either case.
	switch s := src.(type) {
	case *image.NRGBA:
		if !r.In(s.Rect) {
			break
		}
		// Read src.Pix directly instead of src.At(...)/dst.Set(...).
		// Exactness: At() returns color.NRGBA and dst.Set runs it through
		// color.NRGBAModel, which identity-returns its own NRGBA type (the
		// stdlib model functions short-circuit their own concrete type) — so
		// Set stores the source bytes verbatim, alpha included. A plain row
		// copy therefore reproduces the generic path byte for byte; no
		// premultiply/unpremultiply round trip may be inserted here (NRGBA is
		// NOT premultiplied, so that would corrupt semi-transparent pixels).
		for y := 0; y < r.Dy(); y++ {
			si := s.PixOffset(r.Min.X, r.Min.Y+y)
			di := y * dst.Stride
			copy(dst.Pix[di:di+4*r.Dx()], s.Pix[si:si+4*r.Dx()])
		}
		return dst
	case *image.RGBA:
		if !r.In(s.Rect) {
			break
		}
		// RGBA stores alpha-premultiplied bytes, so At().RGBA() returns
		// v8*0x101 for every channel (no per-pixel alpha multiply);
		// NRGBAModel has no RGBA fast path, so dst.Set really un-premultiplies
		// through the model — replicated below with the model's exact integer
		// math (same floor divisions): opaque stores the bytes as-is
		// ((v8*0x101)>>8 == v8), fully transparent zeroes them (even a
		// nonzero premultiplied RGB), and anything between un-premultiplies
		// as r' = (R*0x101*0xffff)/(A*0x101) = R*0xffff/A, with alpha stored
		// as uint8((A*0x101)>>8) == A.
		for y := 0; y < r.Dy(); y++ {
			si := s.PixOffset(r.Min.X, r.Min.Y+y)
			di := y * dst.Stride
			for x := 0; x < r.Dx(); x++ {
				R, G, B, A := s.Pix[si], s.Pix[si+1], s.Pix[si+2], s.Pix[si+3]
				switch A {
				case 0xff:
					dst.Pix[di], dst.Pix[di+1], dst.Pix[di+2], dst.Pix[di+3] = R, G, B, 0xff
				case 0:
					dst.Pix[di], dst.Pix[di+1], dst.Pix[di+2], dst.Pix[di+3] = 0, 0, 0, 0
				default:
					a := uint32(A)
					dst.Pix[di] = uint8((uint32(R) * 0xffff / a) >> 8)
					dst.Pix[di+1] = uint8((uint32(G) * 0xffff / a) >> 8)
					dst.Pix[di+2] = uint8((uint32(B) * 0xffff / a) >> 8)
					dst.Pix[di+3] = A
				}
				si += 4
				di += 4
			}
		}
		return dst
	}
	// Generic fallback: the original per-pixel copy. Reached by non-NRGBA/RGBA
	// sources via the switch's default path and by NRGBA/RGBA sources whose
	// rect is not fully inside the source (the fast cases above break); At()
	// returns transparent zeros outside bounds, exactly as before.
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			dst.Set(x, y, src.At(r.Min.X+x, r.Min.Y+y))
		}
	}
	return dst
}

// resizeBilinear scales src to w x h with bilinear interpolation.
func resizeBilinear(src image.Image, w, h int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	if sw == 0 || sh == 0 {
		return dst
	}
	// The source type is checked once per call; the generic fallback keeps the
	// original per-pixel At/Set path for every other source type (e.g.
	// *image.YCbCr from JPEG), whose semantics must not change.
	switch s := src.(type) {
	case *image.NRGBA:
		// Identical fx/fy arithmetic to the generic loop; only the per-sample
		// read (Pix instead of At().RGBA()) and the write (setNRGBA8 instead
		// of dst.Set) differ, both proven byte-exact (see their comments).
		for y := 0; y < h; y++ {
			fy := (float64(y)+0.5)*float64(sh)/float64(h) - 0.5 + float64(sb.Min.Y)
			di := y * dst.Stride
			for x := 0; x < w; x++ {
				fx := (float64(x)+0.5)*float64(sw)/float64(w) - 0.5 + float64(sb.Min.X)
				c := bilinearNRGBA(s, sb, fx, fy)
				setNRGBA8(dst.Pix[di:di+4:di+4], c)
				di += 4
			}
		}
	case *image.RGBA:
		for y := 0; y < h; y++ {
			fy := (float64(y)+0.5)*float64(sh)/float64(h) - 0.5 + float64(sb.Min.Y)
			di := y * dst.Stride
			for x := 0; x < w; x++ {
				fx := (float64(x)+0.5)*float64(sw)/float64(w) - 0.5 + float64(sb.Min.X)
				c := bilinearRGBA(s, sb, fx, fy)
				setNRGBA8(dst.Pix[di:di+4:di+4], c)
				di += 4
			}
		}
	default:
		for y := 0; y < h; y++ {
			fy := (float64(y)+0.5)*float64(sh)/float64(h) - 0.5 + float64(sb.Min.Y)
			for x := 0; x < w; x++ {
				fx := (float64(x)+0.5)*float64(sw)/float64(w) - 0.5 + float64(sb.Min.X)
				dst.Set(x, y, bilinear(src, sb, fx, fy))
			}
		}
	}
	return dst
}

// umeyama estimates a similarity transform (scale, rotation, translation)
// mapping src points onto dst points, returned as a 2x3 affine matrix that
// maps src -> dst. src must have 5 points; dst is the 5-point template.
func umeyama(src [][2]float64, dst [5][2]float32) [2][3]float64 {
	n := 5
	// means
	var msx, msy, mdx, mdy float64
	for i := 0; i < n; i++ {
		msx += src[i][0]
		msy += src[i][1]
		mdx += float64(dst[i][0])
		mdy += float64(dst[i][1])
	}
	msx, msy, mdx, mdy = msx/float64(n), msy/float64(n), mdx/float64(n), mdy/float64(n)

	// centre
	sx := make([]float64, n)
	sy := make([]float64, n)
	dx := make([]float64, n)
	dy := make([]float64, n)
	var srcVar float64
	for i := 0; i < n; i++ {
		sx[i] = src[i][0] - msx
		sy[i] = src[i][1] - msy
		dx[i] = float64(dst[i][0]) - mdx
		dy[i] = float64(dst[i][1]) - mdy
		srcVar += sx[i]*sx[i] + sy[i]*sy[i]
	}
	srcVar /= float64(n)

	// cross-covariance
	var sxx, sxy, syx, syy float64
	for i := 0; i < n; i++ {
		sxx += dx[i] * sx[i]
		sxy += dx[i] * sy[i]
		syx += dy[i] * sx[i]
		syy += dy[i] * sy[i]
	}
	sxx, sxy, syx, syy = sxx/float64(n), sxy/float64(n), syx/float64(n), syy/float64(n)

	// For the 2-D similarity case the optimal rotation+scale reduces to:
	//   a = (sxx + syy) / srcVar
	//   b = (syx - sxy) / srcVar
	// giving R*scale = [[a, -b],[b, a]].
	a := (sxx + syy) / srcVar
	b := (syx - sxy) / srcVar

	tx := mdx - a*msx + b*msy
	ty := mdy - b*msx - a*msy

	return [2][3]float64{
		{a, -b, tx},
		{b, a, ty},
	}
}

// invertAffine inverts a 2x3 affine matrix that maps src->dst, producing the
// matrix mapping dst->src used for backward warping.
func invertAffine(m [2][3]float64) [2][3]float64 {
	a, b, tx := m[0][0], m[0][1], m[0][2]
	c, d, ty := m[1][0], m[1][1], m[1][2]
	det := a*d - b*c
	if det == 0 {
		// identity fallback
		return [2][3]float64{{1, 0, 0}, {0, 1, 0}}
	}
	ia := d / det
	ib := -b / det
	ic := -c / det
	id := a / det
	itx := -(ia*tx + ib*ty)
	ity := -(ic*tx + id*ty)
	return [2][3]float64{
		{ia, ib, itx},
		{ic, id, ity},
	}
}

// affineWarp renders a w x h image by sampling src through the dst->src affine
// matrix m using bilinear interpolation.
func affineWarp(src image.Image, m [2][3]float64, w, h int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	// Source type checked once per call; the generic fallback keeps the
	// original per-pixel At/Set path for other source types (e.g. *image.YCbCr
	// from JPEG), whose semantics must not change.
	switch s := src.(type) {
	case *image.NRGBA:
		for y := 0; y < h; y++ {
			di := y * dst.Stride
			for x := 0; x < w; x++ {
				fx := m[0][0]*float64(x) + m[0][1]*float64(y) + m[0][2]
				fy := m[1][0]*float64(x) + m[1][1]*float64(y) + m[1][2]
				c := bilinearNRGBA(s, sb, fx, fy)
				setNRGBA8(dst.Pix[di:di+4:di+4], c)
				di += 4
			}
		}
	case *image.RGBA:
		for y := 0; y < h; y++ {
			di := y * dst.Stride
			for x := 0; x < w; x++ {
				fx := m[0][0]*float64(x) + m[0][1]*float64(y) + m[0][2]
				fy := m[1][0]*float64(x) + m[1][1]*float64(y) + m[1][2]
				c := bilinearRGBA(s, sb, fx, fy)
				setNRGBA8(dst.Pix[di:di+4:di+4], c)
				di += 4
			}
		}
	default:
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				fx := m[0][0]*float64(x) + m[0][1]*float64(y) + m[0][2]
				fy := m[1][0]*float64(x) + m[1][1]*float64(y) + m[1][2]
				c := bilinear(src, sb, fx, fy)
				dst.Set(x, y, c)
			}
		}
	}
	return dst
}

func bilinear(src image.Image, b image.Rectangle, fx, fy float64) (nrgba color8) {
	x0 := int(math.Floor(fx))
	y0 := int(math.Floor(fy))
	x1 := x0 + 1
	y1 := y0 + 1
	dx := fx - float64(x0)
	dy := fy - float64(y0)

	sample := func(x, y int) (r, g, bl, a float64) {
		if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
			return 0, 0, 0, 0
		}
		cr, cg, cb, ca := src.At(x, y).RGBA()
		return float64(cr) / 257, float64(cg) / 257, float64(cb) / 257, float64(ca) / 257
	}
	r00, g00, b00, a00 := sample(x0, y0)
	r10, g10, b10, a10 := sample(x1, y0)
	r01, g01, b01, a01 := sample(x0, y1)
	r11, g11, b11, a11 := sample(x1, y1)

	lerp := func(v00, v10, v01, v11 float64) float64 {
		top := v00*(1-dx) + v10*dx
		bot := v01*(1-dx) + v11*dx
		return top*(1-dy) + bot*dy
	}
	return color8{
		r: uint8(clamp(lerp(r00, r10, r01, r11))),
		g: uint8(clamp(lerp(g00, g10, g01, g11))),
		b: uint8(clamp(lerp(b00, b10, b01, b11))),
		a: uint8(clamp(lerp(a00, a10, a01, a11))),
	}
}

// sampleNRGBA returns the four float64 samples bilinear() would derive from
// src.At(x, y).RGBA()/257 for an *image.NRGBA source. Exact: for opaque pixels
// (A==0xff) At().RGBA() returns v8*0x101 and v8*0x101/257 == v8 exactly in
// float64, so the raw Pix byte IS the sample; alpha is A*0x101/257 == A. For
// semi-transparent pixels NRGBA.RGBA() premultiplies (r16 = R*0x101*A/0xff,
// integer floor) — replicated here with the same integer math before the /257,
// and the alpha sample stays float64(A).
func sampleNRGBA(s *image.NRGBA, b image.Rectangle, x, y int) [4]float64 {
	if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
		return [4]float64{}
	}
	i := s.PixOffset(x, y)
	r8, g8, b8, a8 := s.Pix[i], s.Pix[i+1], s.Pix[i+2], s.Pix[i+3]
	if a8 == 0xff {
		return [4]float64{float64(r8), float64(g8), float64(b8), float64(a8)}
	}
	a := uint32(a8)
	return [4]float64{
		float64(uint32(r8)*0x101*a/0xff) / 257,
		float64(uint32(g8)*0x101*a/0xff) / 257,
		float64(uint32(b8)*0x101*a/0xff) / 257,
		float64(a8),
	}
}

// sampleRGBA is sampleNRGBA for *image.RGBA sources. RGBA stores
// alpha-premultiplied bytes, so At().RGBA() returns v8*0x101 for every pixel
// and v8*0x101/257 == v8 exactly — the raw Pix byte IS the sample, for alpha
// included.
func sampleRGBA(s *image.RGBA, b image.Rectangle, x, y int) [4]float64 {
	if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
		return [4]float64{}
	}
	i := s.PixOffset(x, y)
	return [4]float64{
		float64(s.Pix[i]), float64(s.Pix[i+1]),
		float64(s.Pix[i+2]), float64(s.Pix[i+3]),
	}
}

// blendBilinear lerps the four bilinear corner samples and clamps to 8 bits.
// The specialised bilinearNRGBA/bilinearRGBA share it; the operations and their
// order are identical to the lerp closure inside bilinear().
func blendBilinear(s00, s10, s01, s11 [4]float64, dx, dy float64) color8 {
	lerp := func(v00, v10, v01, v11 float64) float64 {
		top := v00*(1-dx) + v10*dx
		bot := v01*(1-dx) + v11*dx
		return top*(1-dy) + bot*dy
	}
	return color8{
		r: uint8(clamp(lerp(s00[0], s10[0], s01[0], s11[0]))),
		g: uint8(clamp(lerp(s00[1], s10[1], s01[1], s11[1]))),
		b: uint8(clamp(lerp(s00[2], s10[2], s01[2], s11[2]))),
		a: uint8(clamp(lerp(s00[3], s10[3], s01[3], s11[3]))),
	}
}

// bilinearNRGBA is bilinear specialised for *image.NRGBA sources: samples come
// straight from Pix (see sampleNRGBA for the exactness argument) with the same
// floor/lerp/clamp arithmetic as bilinear.
func bilinearNRGBA(s *image.NRGBA, b image.Rectangle, fx, fy float64) color8 {
	x0 := int(math.Floor(fx))
	y0 := int(math.Floor(fy))
	x1 := x0 + 1
	y1 := y0 + 1
	dx := fx - float64(x0)
	dy := fy - float64(y0)
	return blendBilinear(
		sampleNRGBA(s, b, x0, y0), sampleNRGBA(s, b, x1, y0),
		sampleNRGBA(s, b, x0, y1), sampleNRGBA(s, b, x1, y1),
		dx, dy,
	)
}

// bilinearRGBA is bilinearNRGBA for *image.RGBA sources.
func bilinearRGBA(s *image.RGBA, b image.Rectangle, fx, fy float64) color8 {
	x0 := int(math.Floor(fx))
	y0 := int(math.Floor(fy))
	x1 := x0 + 1
	y1 := y0 + 1
	dx := fx - float64(x0)
	dy := fy - float64(y0)
	return blendBilinear(
		sampleRGBA(s, b, x0, y0), sampleRGBA(s, b, x1, y0),
		sampleRGBA(s, b, x0, y1), sampleRGBA(s, b, x1, y1),
		dx, dy,
	)
}

// setNRGBA8 writes c into a 4-byte NRGBA Pix slice with exactly the semantics
// of (*image.NRGBA).Set(x, y, c), i.e. color.NRGBAModel.Convert(c): opaque
// (a==0xff) stores the channels as-is (At().RGBA() = v8*0x101, (v8*0x101)>>8 ==
// v8), fully transparent zeroes them, and anything between is un-premultiplied
// with the same integer floor division the model performs
// (r' = (r16*0xffff)/a16 = (c.r*0xffff)/c.a after cancelling 0x101).
func setNRGBA8(pix []uint8, c color8) {
	switch c.a {
	case 0xff:
		pix[0], pix[1], pix[2], pix[3] = c.r, c.g, c.b, 0xff
	case 0:
		pix[0], pix[1], pix[2], pix[3] = 0, 0, 0, 0
	default:
		a := uint32(c.a)
		pix[0] = uint8((uint32(c.r) * 0xffff / a) >> 8)
		pix[1] = uint8((uint32(c.g) * 0xffff / a) >> 8)
		pix[2] = uint8((uint32(c.b) * 0xffff / a) >> 8)
		pix[3] = c.a
	}
}

type color8 struct{ r, g, b, a uint8 }

func (c color8) RGBA() (r, g, b, a uint32) {
	r = uint32(c.r) * 0x101
	g = uint32(c.g) * 0x101
	b = uint32(c.b) * 0x101
	a = uint32(c.a) * 0x101
	return
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Ensure the models referenced exist before trying to serve requests.
func CheckModels(detPath, embPath string) error {
	for _, p := range []string{detPath, embPath} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("model missing: %s (run `make models`, download the buffalo_l pack manually, "+
				"or re-enable auto-download via RECOGN_AUTO_DOWNLOAD; override the pack URL with RECOGN_MODELS_URL)", filepath.Base(p))
		}
	}
	return nil
}
