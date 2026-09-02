package engine

import (
	"bytes"
	"fmt"
	"image"
	"math"
	"runtime"
	"sync"

	"recogn/internal/onnxrt"
)

const (
	// maxConcurrency caps the configured number of parallel model Runs:
	// beyond a handful of streams the shared CPU is oversubscribed and
	// activation memory starts piling up.
	maxConcurrency = 16
)

// defaultConcurrency is one Run stream per core, capped to leave headroom for
// decode/matching on the same cores.
var defaultConcurrency = func() int {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}()

// DefaultConcurrency reports the default cap on parallel model Runs.
func DefaultConcurrency() int { return defaultConcurrency }

// cgoInferencer runs SCRFD + ArcFace in-process via the ONNX Runtime C API
// (see internal/onnxrt): detection pre/post-processing and face embedding are
// done here in Go, with only the raw model execution delegated to
// libonnxruntime through CGO.
//
// Inference is bounded, not serialised: a slots semaphore admits up to
// `concurrency` model Runs in parallel. This is safe because both sessions
// were opened with intra-op threads = 1 (see onnxrt.c), so each Run executes
// sequentially on the calling goroutine, and ORT's CPU execution provider is
// documented thread-safe for concurrent Run calls on one session (guarded by
// TestConcurrentRunParity in internal/onnxrt). Only the Run itself holds a
// slot; Go-side pre/post-processing stays outside the gate.
type cgoInferencer struct {
	det *onnxrt.Session
	emb *onnxrt.Session
	// slots is a buffered channel used as a counting semaphore: send to take
	// a slot, receive to release. Capacity is the max concurrent Runs.
	slots chan struct{}
}

// inferenceConcurrency validates a requested concurrency level: values <= 0
// fall back to the default; anything above maxConcurrency is clamped.
func inferenceConcurrency(n int) int {
	if n <= 0 {
		return defaultConcurrency
	}
	if n > maxConcurrency {
		return maxConcurrency
	}
	return n
}

// newCGOInferencer opens both ONNX models in-process and returns an
// inferencer allowing at most concurrency parallel model Runs (<=0 uses the
// default; values above maxConcurrency are clamped). detModel/embModel are
// paths to the .onnx files.
func newCGOInferencer(detModel, embModel string, concurrency int) (inferencer, error) {
	det, err := onnxrt.Open(detModel)
	if err != nil {
		return nil, fmt.Errorf("detector: %w", err)
	}
	emb, err := onnxrt.Open(embModel)
	if err != nil {
		det.Close()
		return nil, fmt.Errorf("embedder: %w", err)
	}
	n := inferenceConcurrency(concurrency)
	return &cgoInferencer{det: det, emb: emb, slots: make(chan struct{}, n)}, nil
}

// takeSlot admits one Run through the concurrency gate.
func (c *cgoInferencer) takeSlot() { c.slots <- struct{}{} }

// releaseSlot frees the Run slot.
func (c *cgoInferencer) releaseSlot() { <-c.slots }

func (c *cgoInferencer) ping() error {
	// Models are opened at construction, so a trivial detector run on a blank
	// tensor confirms the runtime is functional.
	c.takeSlot()
	defer c.releaseSlot()
	blank := make([]float32, 3*detInputSize*detInputSize)
	_, err := c.det.Run(blank, []int64{1, 3, detInputSize, detInputSize})
	return err
}

func (c *cgoInferencer) detect(imgBytes []byte) ([]Face, error) {
	src, _, err := image.Decode(bytes.NewReader(imgBytes))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return c.detectFromImage(src)
}

// detectFromImage runs the detector on an already-decoded image, so callers
// that also align faces from the same image (Recognize) decode only once.
func (c *cgoInferencer) detectFromImage(src image.Image) ([]Face, error) {
	lb, err := letterboxDetect(src)
	if err != nil {
		return nil, err
	}
	c.takeSlot()
	outs, err := c.det.Run(lb.tensor, []int64{1, 3, detInputSize, detInputSize})
	c.releaseSlot()
	if err != nil {
		return nil, fmt.Errorf("detect run: %w", err)
	}
	if len(outs) != 9 {
		return nil, fmt.Errorf("detector returned %d outputs, want 9", len(outs))
	}
	flat := make([][]float32, 9)
	for i := range outs {
		flat[i] = outs[i].Data
	}
	return decodeSCRFD(flat, lb.scale, detInputSize), nil
}

func (c *cgoInferencer) embedImage(aligned *image.NRGBA) ([]float32, error) {
	tensor := preprocessFace(aligned)
	c.takeSlot()
	outs, err := c.emb.Run(tensor, []int64{1, 3, 112, 112})
	c.releaseSlot()
	if err != nil {
		return nil, fmt.Errorf("embed run: %w", err)
	}
	return normalizeEmbedding(outs, "embed")
}

// embedBatch computes embeddings for several aligned faces. NOTE: it does
// NOT batch them into one [N,3,112,112] Run. Although the ArcFace graph
// declares a dynamic input batch dim, this export keeps a fixed {1,512}
// output shape and its BatchNormalization nodes normalise over the batch
// axis at Run time, so batched outputs diverge from per-face outputs
// (measured cosine 0.007–0.59; see TestCGOBatchedEmbedParity and
// TestRunBatchEmbedder). N faces are therefore embedded as N parallel
// per-face Runs through the concurrency gate — same numbers, multiple cores.
// A face whose Run fails yields nil at its index; the caller reports it like
// an embed error. Empty input returns an empty result without inference.
func (c *cgoInferencer) embedBatch(aligned []*image.NRGBA) ([][]float32, error) {
	out := make([][]float32, len(aligned))
	// Collect runnable face indices (nils are preserved as nil results).
	idx := make([]int, 0, len(aligned))
	for i, a := range aligned {
		if a != nil {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return out, nil
	}
	workers := cap(c.slots)
	if workers > len(idx) {
		workers = len(idx)
	}
	if workers <= 1 {
		for _, i := range idx {
			if e, err := c.embedImage(aligned[i]); err == nil {
				out[i] = e
			}
		}
		return out, nil
	}
	next := make(chan int, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range next {
				if e, err := c.embedImage(aligned[i]); err == nil {
					out[i] = e // failed Runs leave nil, reported by the caller
				}
			}
		}()
	}
	for _, i := range idx {
		next <- i
	}
	close(next)
	wg.Wait()
	return out, nil
}

// normalizeEmbedding L2-normalises a single ArcFace output tensor
// (the output is unit L2 norm) and validates its shape.
func normalizeEmbedding(outs []onnxrt.Tensor, what string) ([]float32, error) {
	if len(outs) != 1 {
		return nil, fmt.Errorf("%s returned %d outputs, want 1", what, len(outs))
	}
	emb := outs[0].Data
	normalizeInPlace(emb)
	return emb, nil
}

// normalizeInPlace scales a slice to unit L2 norm (no-op for zero vectors).
func normalizeInPlace(emb []float32) {
	var norm float64
	for _, v := range emb {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		inv := 1.0 / math.Sqrt(norm)
		for i := range emb {
			emb[i] = float32(float64(emb[i]) * inv)
		}
	}
}

func (c *cgoInferencer) close() {
	// Acquire every slot so concurrent Runs finish before sessions close,
	// then release them again for good measure (close is terminal).
	for i := 0; i < cap(c.slots); i++ {
		c.slots <- struct{}{}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if c.det != nil {
			c.det.Close()
			c.det = nil
		}
	}()
	go func() {
		defer wg.Done()
		if c.emb != nil {
			c.emb.Close()
			c.emb = nil
		}
	}()
	wg.Wait()
	for i := 0; i < cap(c.slots); i++ {
		<-c.slots
	}
}
