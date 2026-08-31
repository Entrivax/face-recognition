package engine

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

// solidJPEG encodes a w x h grayscale-ish image as JPEG.
func solidJPEG(t *testing.T, w, h int, v uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = v
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestAnnotateRendersBoxes(t *testing.T) {
	src := solidJPEG(t, 320, 240, 40)
	faces := []Face{
		{Name: "Alice", Confidence: 0.91, BBox: [4]float64{20, 20, 80, 80}},
		{Name: "unknown", Confidence: 0.42, BBox: [4]float64{200, 100, 80, 80}},
	}
	out, err := Annotate(src, faces)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	annotated := decodeJPEG(t, out)
	if annotated.Bounds().Dx() != 320 || annotated.Bounds().Dy() != 240 {
		t.Fatalf("annotated bounds = %v, want 320x240", annotated.Bounds())
	}

	// The plain re-encode (no faces) is the JPEG baseline; the annotated
	// render must differ from it in many pixels (brackets + labels drawn).
	baseline := decodeJPEG(t, mustAnnotate(t, src, nil))
	diff := 0
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			ar, ag, ab, _ := annotated.At(x, y).RGBA()
			br, bg, bb, _ := baseline.At(x, y).RGBA()
			if abs8(int(ar>>8), int(br>>8)) > 16 ||
				abs8(int(ag>>8), int(bg>>8)) > 16 ||
				abs8(int(ab>>8), int(bb>>8)) > 16 {
				diff++
			}
		}
	}
	if diff < 100 {
		t.Fatalf("annotated image barely differs from baseline (%d px)", diff)
	}
}

func TestAnnotateEmptyAndInvalid(t *testing.T) {
	// No faces: valid JPEG copy.
	out, err := Annotate(solidJPEG(t, 100, 100, 10), nil)
	if err != nil {
		t.Fatalf("Annotate with no faces: %v", err)
	}
	if img := decodeJPEG(t, out); img.Bounds().Dx() != 100 {
		t.Fatalf("bounds = %v", img.Bounds())
	}
	// Invalid image bytes.
	if _, err := Annotate([]byte("not an image"), nil); err == nil {
		t.Fatal("expected error for invalid image bytes")
	}
}

func mustAnnotate(t *testing.T, src []byte, faces []Face) []byte {
	t.Helper()
	out, err := Annotate(src, faces)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	return out
}

func abs8(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}
