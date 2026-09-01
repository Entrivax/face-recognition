package engine

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"testing"
)

// makeJPEG encodes a solid-colour image of the given size to JPEG bytes.
func makeJPEG(tb testing.TB, w, h int, c color.RGBA) []byte {
	tb.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func TestPreprocessDetectShape(t *testing.T) {
	// Landscape image (wider than tall) -> newW=640, newH<640.
	img := makeJPEG(t, 1000, 500, color.RGBA{200, 100, 50, 255})
	lb, err := preprocessDetect(img)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb.tensor) != 3*detInputSize*detInputSize {
		t.Fatalf("tensor len = %d", len(lb.tensor))
	}
	if lb.newW != detInputSize || lb.newH != 320 {
		t.Errorf("letterbox dims = %dx%d, want 640x320", lb.newW, lb.newH)
	}
	if math.Abs(lb.scale-float64(320)/500) > 1e-6 {
		t.Errorf("scale = %v, want %v", lb.scale, 320.0/500)
	}
}

func TestPreprocessDetectNormalisation(t *testing.T) {
	// Solid colour; JPEG q95 is near-lossless for flat regions, so the centre
	// pixel should normalise to ~(v-127.5)/128.
	img := makeJPEG(t, 200, 200, color.RGBA{255, 0, 127, 255})
	lb, err := preprocessDetect(img)
	if err != nil {
		t.Fatal(err)
	}
	plane := detInputSize * detInputSize
	// A pixel well inside the image (not padding).
	idx := 100*detInputSize + 100
	r := lb.tensor[0*plane+idx]
	g := lb.tensor[1*plane+idx]
	b := lb.tensor[2*plane+idx]
	// Expected ~ (255-127.5)/128 = 0.996, (0-127.5)/128 = -0.996, (127-127.5)/128 ≈ -0.004.
	if math.Abs(float64(r)-0.996) > 0.05 {
		t.Errorf("R normalised = %v, want ~0.996", r)
	}
	if math.Abs(float64(g)-(-0.996)) > 0.05 {
		t.Errorf("G normalised = %v, want ~-0.996", g)
	}
	if math.Abs(float64(b)-(-0.004)) > 0.05 {
		t.Errorf("B normalised = %v, want ~-0.004", b)
	}
	// Padding region (below the 200px-tall content scaled into 640) should be padVal.
	// newH for a square 200px image is 640 (fills), so instead check a corner of a
	// wide image for padding. (Covered by shape test; here just ensure no NaN.)
	for _, v := range []float32{r, g, b} {
		if math.IsNaN(float64(v)) {
			t.Errorf("NaN in tensor")
		}
	}
}

func TestPreprocessFace(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 112, 112))
	for y := 0; y < 112; y++ {
		for x := 0; x < 112; x++ {
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	tensor := preprocessFace(img)
	if len(tensor) != 3*112*112 {
		t.Fatalf("tensor len = %d", len(tensor))
	}
	// White pixel -> (255-127.5)/127.5 = 1.0 in all channels.
	if math.Abs(float64(tensor[0])-1.0) > 1e-3 {
		t.Errorf("tensor[0] = %v, want ~1.0", tensor[0])
	}
	plane := 112 * 112
	if math.Abs(float64(tensor[plane])-1.0) > 1e-3 ||
		math.Abs(float64(tensor[2*plane])-1.0) > 1e-3 {
		t.Errorf("G/B channels not ~1.0: %v %v", tensor[plane], tensor[2*plane])
	}
}

// --- Benchmarks (informational: they measure the preprocessing hot paths the
// direct-Pix fast paths in engine.go / preprocess.go optimise) ---

// benchGradientNRGBA fills a w x h NRGBA with a deterministic opaque gradient,
// so benchmarks exercise the opaque fast path with realistic per-pixel work.
func benchGradientNRGBA(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 255 / (w - 1)),
				G: uint8(y * 255 / (h - 1)),
				B: uint8((x*x + 3*y) % 256),
				A: 0xff,
			})
		}
	}
	return img
}

// benchPhotoJPEG builds a deterministic w x h JPEG with per-pixel variation,
// so its decode cost resembles a real photo rather than a flat fill.
func benchPhotoJPEG(tb testing.TB, w, h int) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, benchGradientNRGBA(w, h), &jpeg.Options{Quality: 92}); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkPreprocessDetect covers the full detector input path: JPEG decode,
// letterbox resize (bilinear) and the 3x640x640 CHW tensor build.
func BenchmarkPreprocessDetect(b *testing.B) {
	img := benchPhotoJPEG(b, 1024, 768)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := preprocessDetect(img); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPreprocessFace covers the embedder input path: aligned 112x112
// NRGBA -> normalised 3x112x112 CHW tensor.
func BenchmarkPreprocessFace(b *testing.B) {
	img := benchGradientNRGBA(112, 112)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		preprocessFace(img)
	}
}

// BenchmarkResizeBilinear covers the letterbox resize on an opaque NRGBA.
func BenchmarkResizeBilinear(b *testing.B) {
	src := benchGradientNRGBA(640, 480)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resizeBilinear(src, 320, 240)
	}
}

// BenchmarkAffineWarp112 covers the Umeyama backward warp that renders one
// aligned 112x112 face crop.
func BenchmarkAffineWarp112(b *testing.B) {
	src := benchGradientNRGBA(320, 240)
	m := [2][3]float64{{0.9, -0.2, 60}, {0.2, 0.9, -20}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		affineWarp(src, m, 112, 112)
	}
}

// BenchmarkCropImage covers the bbox crop used by thumbnails and the
// landmark-less alignment fallback.
func BenchmarkCropImage(b *testing.B) {
	src := benchGradientNRGBA(400, 300)
	r := image.Rect(40, 30, 240, 190)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cropImage(src, r)
	}
}
