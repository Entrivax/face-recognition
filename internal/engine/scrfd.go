package engine

import (
	"sort"
	"sync"
)

// SCRFD post-processing, ported from python/infer.py (which mirrors the
// insightface SCRFD implementation). It decodes the detector's raw output
// tensors into bounding boxes, confidence scores and 5-point landmarks, then
// applies non-maximum suppression.
//
// The detector emits 9 tensors across 3 FPN strides {8,16,32}; for stride
// index idx the tensors are:
//   outputs[idx]   -> scores  [N,1]
//   outputs[idx+3] -> bbox    [N,4]  (distances, multiplied by stride)
//   outputs[idx+6] -> kps     [N,10] (landmark offsets, multiplied by stride)
// where N = (640/stride)^2 * 2 anchors.

var (
	scrfdStrides    = []int{8, 16, 32}
	scrfdNumAnchors = 2
	scrfdNMSThresh  = 0.4
	scrfdDetThresh  = 0.5
)

// rawDetection is one decoded candidate before NMS.
type rawDetection struct {
	x1, y1, x2, y2 float32
	score          float32
	kps            [5][2]float32
}

// anchor-center grids are pure constants for a given feature-map geometry, but
// decodeSCRFD used to rebuild them on every detection. They are cached by
// (height, width, stride, numAnchors); the cached slice is shared between
// calls, so callers must treat it as read-only (decodeSCRFD does).
var (
	anchorCentersMu    sync.Mutex
	anchorCentersCache = map[[4]int][][2]float32{}
)

// anchorCenters returns the (N,2) anchor-center grid for a feature map of
// height x width at the given stride, with numAnchors per location, cached by
// geometry. The values are identical to building the grid fresh every call.
func anchorCenters(height, width, stride, numAnchors int) [][2]float32 {
	key := [4]int{height, width, stride, numAnchors}
	anchorCentersMu.Lock()
	c, ok := anchorCentersCache[key]
	anchorCentersMu.Unlock()
	if ok {
		return c
	}
	c = buildAnchorCenters(height, width, stride, numAnchors)
	anchorCentersMu.Lock()
	anchorCentersCache[key] = c
	anchorCentersMu.Unlock()
	return c
}

// buildAnchorCenters constructs one grid: np.mgrid[:height,:width][::-1] gives
// (x,y) per cell, then * stride.
func buildAnchorCenters(height, width, stride, numAnchors int) [][2]float32 {
	n := height * width * numAnchors
	out := make([][2]float32, 0, n)
	for i := 0; i < height; i++ {
		for j := 0; j < width; j++ {
			cx := float32(j * stride)
			cy := float32(i * stride)
			for a := 0; a < numAnchors; a++ {
				out = append(out, [2]float32{cx, cy})
			}
		}
	}
	return out
}

// decodeSCRFD turns the 9 raw output tensors into post-NMS detections. The
// inputs are the flat float data and row counts per stride for scores, bboxes
// and kps. detScale maps model-space coords back to original-image space.
func decodeSCRFD(outputs [][]float32, detScale float64, inputSize int) []Face {
	fmc := 3
	var dets []rawDetection

	for idx, stride := range scrfdStrides {
		scores := outputs[idx]
		bboxPreds := outputs[idx+fmc]
		kpsPreds := outputs[idx+fmc*2]
		fs := float32(stride) // preds are multiplied by stride (mirrors the reference insightface/SCRFD implementation)

		height := inputSize / stride
		width := inputSize / stride
		centers := anchorCenters(height, width, stride, scrfdNumAnchors)
		n := len(centers)

		for i := 0; i < n; i++ {
			sc := scores[i]
			if sc < float32(scrfdDetThresh) {
				continue
			}
			cx, cy := centers[i][0], centers[i][1]
			// distance2bbox: preds*stride give distances from the anchor center.
			bo := i * 4
			d0 := bboxPreds[bo+0] * fs
			d1 := bboxPreds[bo+1] * fs
			d2 := bboxPreds[bo+2] * fs
			d3 := bboxPreds[bo+3] * fs
			d := rawDetection{
				x1:    cx - d0,
				y1:    cy - d1,
				x2:    cx + d2,
				y2:    cy + d3,
				score: sc,
			}
			// distance2kps: preds*stride give landmark offsets from the center.
			ko := i * 10
			for k := 0; k < 5; k++ {
				px := cx + kpsPreds[ko+k*2]*fs
				py := cy + kpsPreds[ko+k*2+1]*fs
				d.kps[k] = [2]float32{px, py}
			}
			dets = append(dets, d)
		}
	}

	// Map back to original image coordinates.
	inv := float32(1.0 / detScale)
	for i := range dets {
		dets[i].x1 *= inv
		dets[i].y1 *= inv
		dets[i].x2 *= inv
		dets[i].y2 *= inv
		for k := range dets[i].kps {
			dets[i].kps[k][0] *= inv
			dets[i].kps[k][1] *= inv
		}
	}

	keep := nmsIndices(dets, float32(scrfdNMSThresh))
	faces := make([]Face, 0, len(keep))
	for _, i := range keep {
		d := dets[i]
		lm := make([][2]float64, 5)
		for k := 0; k < 5; k++ {
			lm[k] = [2]float64{float64(d.kps[k][0]), float64(d.kps[k][1])}
		}
		faces = append(faces, Face{
			BBox: [4]float64{
				float64(d.x1), float64(d.y1),
				float64(d.x2 - d.x1), float64(d.y2 - d.y1),
			},
			Score:     float64(d.score),
			Landmarks: lm,
		})
	}
	return faces
}

// nmsIndices returns the indices of detections to keep after greedy NMS:
// order by score desc, suppress IoU > thresh.
func nmsIndices(dets []rawDetection, thresh float32) []int {
	n := len(dets)
	if n == 0 {
		return nil
	}
	areas := make([]float32, n)
	for i, d := range dets {
		areas[i] = (d.x2 - d.x1 + 1) * (d.y2 - d.y1 + 1)
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return dets[order[a]].score > dets[order[b]].score
	})

	var keep []int
	for len(order) > 0 {
		i := order[0]
		keep = append(keep, i)
		rest := order[1:]
		var next []int
		for _, j := range rest {
			xx1 := maxf(dets[i].x1, dets[j].x1)
			yy1 := maxf(dets[i].y1, dets[j].y1)
			xx2 := minf(dets[i].x2, dets[j].x2)
			yy2 := minf(dets[i].y2, dets[j].y2)
			w := maxf(0, xx2-xx1+1)
			h := maxf(0, yy2-yy1+1)
			inter := w * h
			ovr := inter / (areas[i] + areas[j] - inter)
			if ovr <= thresh {
				next = append(next, j)
			}
		}
		order = next
	}
	return keep
}

func maxf(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

func minf(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}
