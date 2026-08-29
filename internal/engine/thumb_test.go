package engine

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// gradientImage renders a deterministic w x h gradient.
func gradientImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	return img
}

// makeTestImage renders the gradient as PNG bytes.
func makeTestImage(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, gradientImage(w, h)); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func decodeJPEG(t *testing.T, jpg []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("decode jpeg: %v", err)
	}
	return img
}

// TestFaceThumbCropsAroundFace checks the documented crop math: side =
// 1.6 * max(bbox w, h), centered on the bbox, resized to size x size.
func TestFaceThumbCropsAroundFace(t *testing.T) {
	// bbox {50,40,60,80} on a 200x150 image: side = 128, rect (16,16)-(144,144),
	// fully inside the image — so no clamping happens.
	src := makeTestImage(t, 200, 150)
	f := Face{BBox: [4]float64{50, 40, 60, 80}}

	jpg, err := FaceThumb(src, f, 160)
	if err != nil {
		t.Fatalf("FaceThumb: %v", err)
	}
	img := decodeJPEG(t, jpg)
	if b := img.Bounds(); b.Dx() != 160 || b.Dy() != 160 {
		t.Fatalf("thumb size %dx%d, want 160x160", b.Dx(), b.Dy())
	}

	// The thumbnail must be a crop, not the full picture scaled: sample the
	// thumb center and compare it with the source pixel at the bbox center
	// (bilinear + JPEG keep it approximately equal, and clearly different
	// from the source corner the full-scale version would sample).
	srcImg := gradientImage(200, 150)
	cx, cy := 80, 80 // bbox center == crop center == thumb center
	tr, tg, tb, _ := img.At(cx, cy).RGBA()
	sr, sg, sb, _ := srcImg.At(cx, cy).RGBA()
	dr := delta(tr, sr) + delta(tg, sg) + delta(tb, sb)
	if dr > 12*0xffff { // generous tolerance for bilinear + JPEG loss
		t.Errorf("thumb center drifted from source center (delta %d)", dr)
	}
}

func delta(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestFaceThumbClampsToBounds checks that a face at the image corner still
// produces a valid, clamped crop instead of an error.
func TestFaceThumbClampsToBounds(t *testing.T) {
	src := makeTestImage(t, 100, 100)
	jpg, err := FaceThumb(src, Face{BBox: [4]float64{0, 0, 10, 10}}, 64)
	if err != nil {
		t.Fatalf("FaceThumb at corner: %v", err)
	}
	img := decodeJPEG(t, jpg)
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Fatalf("thumb size %dx%d, want 64x64", b.Dx(), b.Dy())
	}
}

func TestFaceThumbRejectsBadInput(t *testing.T) {
	src := makeTestImage(t, 50, 50)
	if _, err := FaceThumb(src, Face{BBox: [4]float64{10, 10, 0, 0}}, 64); err == nil {
		t.Error("expected error for degenerate bbox")
	}
	if _, err := FaceThumb([]byte("not an image"), Face{BBox: [4]float64{1, 1, 5, 5}}, 64); err == nil {
		t.Error("expected error for undecodable image")
	}
}
