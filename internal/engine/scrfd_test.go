package engine

import (
	"math"
	"testing"
)

func TestAnchorCenters(t *testing.T) {
	// 2x3 feature map, stride 8, 2 anchors -> 2*3*2 = 12 centers.
	c := anchorCenters(2, 3, 8, 2)
	if len(c) != 12 {
		t.Fatalf("expected 12 centers, got %d", len(c))
	}
	// Row-major over (i, j), each location repeated numAnchors times:
	// i=0 row: (j=0)->(0,0), (j=1)->(8,0), (j=2)->(16,0)
	// i=1 row: (j=0)->(0,8), (j=1)->(8,8), (j=2)->(16,8)
	want := [][2]float32{
		{0, 0}, {0, 0},
		{8, 0}, {8, 0},
		{16, 0}, {16, 0},
		{0, 8}, {0, 8},
		{8, 8}, {8, 8},
		{16, 8}, {16, 8},
	}
	for i, wc := range want {
		if c[i] != wc {
			t.Errorf("center[%d] = %v, want %v", i, c[i], wc)
		}
	}
}

func TestNMSIndices(t *testing.T) {
	mk := func(x1, y1, x2, y2, score float32) rawDetection {
		return rawDetection{x1: x1, y1: y1, x2: x2, y2: y2, score: score}
	}
	// Two heavily-overlapping boxes -> keep only the higher-scored one.
	dets := []rawDetection{
		mk(0, 0, 10, 10, 0.9),
		mk(1, 1, 11, 11, 0.8),       // overlaps ~68% with box 0
		mk(100, 100, 110, 110, 0.7), // disjoint
	}
	keep := nmsIndices(dets, 0.4)
	if len(keep) != 2 {
		t.Fatalf("expected 2 kept, got %d (%v)", len(keep), keep)
	}
	if keep[0] != 0 {
		t.Errorf("expected highest-scored box 0 kept first, got %d", keep[0])
	}
	if keep[1] != 2 {
		t.Errorf("expected disjoint box 2 kept, got %d", keep[1])
	}

	// Empty input.
	if got := nmsIndices(nil, 0.4); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestDecodeSCRFDStrideMultiplier(t *testing.T) {
	// Construct synthetic outputs where one anchor at stride 8 has a confident
	// score and known bbox distances; verify the decode scales by stride and by
	// 1/detScale.
	inputSize := 640
	stride := 8
	h := inputSize / stride
	w := inputSize / stride
	n := h * w * scrfdNumAnchors

	scores := make([]float32, n)
	bboxes := make([]float32, n*4)
	kps := make([]float32, n*10)

	// Anchor index for cell (i=10, j=20), anchor 0: idx = (i*w + j)*2.
	cell := (10*w + 20) * scrfdNumAnchors
	scores[cell] = 0.9
	// distances (pre-stride): left, top, right, bottom = 1,1,2,2
	bboxes[cell*4+0] = 1
	bboxes[cell*4+1] = 1
	bboxes[cell*4+2] = 2
	bboxes[cell*4+3] = 2
	// one landmark offset
	kps[cell*10+0] = 0.5
	kps[cell*10+1] = 0.5

	// Build the 9-tensor layout: scores for strides 8,16,32 at idx 0,1,2;
	// bboxes at 3,4,5; kps at 6,7,8. Only stride-8 (idx 0) has our signal.
	emptyS := make([]float32, (inputSize/16)*(inputSize/16)*scrfdNumAnchors)
	emptyS32 := make([]float32, (inputSize/32)*(inputSize/32)*scrfdNumAnchors)
	emptyB := make([]float32, len(emptyS)*4)
	emptyB32 := make([]float32, len(emptyS32)*4)
	emptyK := make([]float32, len(emptyS)*10)
	emptyK32 := make([]float32, len(emptyS32)*10)

	outputs := [][]float32{
		scores, emptyS, emptyS32, // scores
		bboxes, emptyB, emptyB32, // bboxes
		kps, emptyK, emptyK32, // kps
	}

	detScale := 1.0 // model space == image space for this test
	faces := decodeSCRFD(outputs, detScale, inputSize)
	if len(faces) != 1 {
		t.Fatalf("expected 1 face, got %d", len(faces))
	}
	f := faces[0]
	// anchor center = (j*stride, i*stride) = (20*8, 10*8) = (160, 80).
	// distances*stride = (8, 8, 16, 16). box = (160-8, 80-8, w=8+16, h=8+16)
	//   = (152, 72, 24, 24).
	wantX, wantY, wantW, wantH := 152.0, 72.0, 24.0, 24.0
	if math.Abs(f.BBox[0]-wantX) > 0.5 || math.Abs(f.BBox[1]-wantY) > 0.5 ||
		math.Abs(f.BBox[2]-wantW) > 0.5 || math.Abs(f.BBox[3]-wantH) > 0.5 {
		t.Errorf("bbox = %v, want ~(%v %v %v %v)", f.BBox, wantX, wantY, wantW, wantH)
	}
	// landmark 0 = center + offset*stride = (160+0.5*8, 80+0.5*8) = (164, 84).
	if math.Abs(f.Landmarks[0][0]-164) > 0.5 || math.Abs(f.Landmarks[0][1]-84) > 0.5 {
		t.Errorf("landmark0 = %v, want ~(164,84)", f.Landmarks[0])
	}
	if math.Abs(f.Score-0.9) > 1e-4 {
		t.Errorf("score = %v, want 0.9", f.Score)
	}
}
