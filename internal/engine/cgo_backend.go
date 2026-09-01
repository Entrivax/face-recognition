package engine

import (
	"fmt"
	"image"
	"math"
	"sync"

	"recogn/internal/onnxrt"
)

// cgoInferencer runs SCRFD + ArcFace in-process via the ONNX Runtime C API
// (see internal/onnxrt): detection pre/post-processing and face embedding are
// done here in Go, with only the raw model execution delegated to
// libonnxruntime through CGO.
//
// Inference is serialized with a mutex — CPU inference here is single-stream
// and ORT sessions are not run concurrently.
type cgoInferencer struct {
	det *onnxrt.Session
	emb *onnxrt.Session
	mu  sync.Mutex
}

// NewCGOInferencer opens both ONNX models in-process and returns an
// inferencer. detModel/embModel are paths to the .onnx files.
func NewCGOInferencer(detModel, embModel string) (inferencer, error) {
	det, err := onnxrt.Open(detModel)
	if err != nil {
		return nil, fmt.Errorf("detector: %w", err)
	}
	emb, err := onnxrt.Open(embModel)
	if err != nil {
		det.Close()
		return nil, fmt.Errorf("embedder: %w", err)
	}
	return &cgoInferencer{det: det, emb: emb}, nil
}

func (c *cgoInferencer) ping() error {
	// Models are opened at construction, so a trivial detector run on a blank
	// tensor confirms the runtime is functional.
	c.mu.Lock()
	defer c.mu.Unlock()
	blank := make([]float32, 3*detInputSize*detInputSize)
	_, err := c.det.Run(blank, []int64{1, 3, detInputSize, detInputSize})
	return err
}

func (c *cgoInferencer) detect(imgBytes []byte) ([]Face, error) {
	lb, err := preprocessDetect(imgBytes)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	outs, err := c.det.Run(lb.tensor, []int64{1, 3, detInputSize, detInputSize})
	c.mu.Unlock()
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
	c.mu.Lock()
	outs, err := c.emb.Run(tensor, []int64{1, 3, 112, 112})
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("embed run: %w", err)
	}
	if len(outs) != 1 {
		return nil, fmt.Errorf("embedder returned %d outputs, want 1", len(outs))
	}
	emb := outs[0].Data
	// L2-normalise (ArcFace output is unit L2 norm).
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
	return emb, nil
}

func (c *cgoInferencer) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.det != nil {
		c.det.Close()
		c.det = nil
	}
	if c.emb != nil {
		c.emb.Close()
		c.emb = nil
	}
}
