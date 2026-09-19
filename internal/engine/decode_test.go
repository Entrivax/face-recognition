package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// craftPNGHeader builds a minimal PNG valid for image.DecodeConfig: the
// 8-byte signature, an IHDR chunk declaring w x h (8-bit RGBA), and IEND —
// with no pixel data at all. That is enough for a decompression-bomb probe:
// the pixel gate must reject it from the declared header alone, before any
// pixel data exists or is read.
func craftPNGHeader(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(typ string, data []byte) {
		var be [4]byte
		binary.BigEndian.PutUint32(be[:], uint32(len(data)))
		buf.Write(be[:])
		buf.WriteString(typ)
		buf.Write(data)
		binary.BigEndian.PutUint32(be[:], crc32.ChecksumIEEE(append([]byte(typ), data...)))
		buf.Write(be[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // color type: RGBA
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return buf.Bytes()
}

func TestCheckPixelCount(t *testing.T) {
	// 8192*6400 == 50<<20 exactly: the limit is inclusive (strict >).
	if err := checkPixelCount(8192, 6400); err != nil {
		t.Errorf("8192x6400 (= exactly %d pixels) should be allowed: %v", maxDecodePixels, err)
	}
	if err := checkPixelCount(8192, 6401); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("8192x6401 should be ErrImageTooLarge, got %v", err)
	}
	for _, d := range [][2]int{{0, 0}, {0, 10}, {10, 0}, {-1, 10}, {10, -1}} {
		if err := checkPixelCount(d[0], d[1]); err == nil {
			t.Errorf("checkPixelCount(%d, %d) should fail", d[0], d[1])
		}
	}
	// The sentinel must be absent from a non-oversized error so callers do
	// not map malformed headers to the pixel-limit 4xx.
	err := checkPixelCount(0, 0)
	if err == nil || errors.Is(err, ErrImageTooLarge) {
		t.Errorf("zero dimensions: got %v, want a non-ErrImageTooLarge error", err)
	}
}

func TestDecodeLimitedRejectsOversizedPNG(t *testing.T) {
	// 10000x10000 = 100 MP > 50 MP: DecodeConfig succeeds (the header is
	// valid), so the gate must reject it without ever running image.Decode.
	bomb := craftPNGHeader(t, 10000, 10000)
	src, err := decodeLimited(bomb)
	if err == nil {
		t.Fatal("oversized header should be rejected")
	}
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("got %v, want ErrImageTooLarge", err)
	}
	if src != nil {
		t.Errorf("no image should be returned, got %T", src)
	}
	// The rejection must name the offending dimensions and the limit.
	if !bytes.Contains([]byte(err.Error()), []byte("10000x10000")) ||
		!bytes.Contains([]byte(err.Error()), []byte("megapixel")) {
		t.Errorf("error message should name dimensions and the megapixel limit: %v", err)
	}
}

func TestDecodeLimitedAcceptsSmallImages(t *testing.T) {
	// Small PNG and JPEG decode exactly as before the gate existed.
	for name, b := range map[string][]byte{
		"png": makeTestImage(t, 64, 64),
		"jpg": solidJPEG(t, 64, 64, 128),
	} {
		src, err := decodeLimited(b)
		if err != nil {
			t.Fatalf("%s: decodeLimited: %v", name, err)
		}
		if got := src.Bounds(); got.Dx() != 64 || got.Dy() != 64 {
			t.Errorf("%s: decoded %v, want 64x64", name, got)
		}
	}
}

// TestOversizedImageRejectedAtEveryEntryPoint pins the C1 fix: every decode
// site reachable from untrusted bytes must bounce a >50 MP declared image
// with ErrImageTooLarge before any pixels are allocated.
func TestOversizedImageRejectedAtEveryEntryPoint(t *testing.T) {
	bomb := craftPNGHeader(t, 10000, 10000) // 100 MP > maxDecodePixels
	face := Face{BBox: [4]float64{0, 0, 10, 10}}
	e := NewWithInferencer(&fakeInferencer{}, 0.5)
	tests := []struct {
		name string
		fn   func() error
	}{
		{"Recognize", func() error { _, err := e.Recognize(bomb); return err }},
		{"alignFaceImage", func() error { _, err := alignFaceImage(bomb, face); return err }},
		{"FaceThumb", func() error { _, err := FaceThumb(bomb, face, 64); return err }},
		{"Annotate", func() error { _, err := Annotate(bomb, nil); return err }},
		{"preprocessDetect", func() error { _, err := preprocessDetect(bomb); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); !errors.Is(err, ErrImageTooLarge) {
				t.Fatalf("got %v, want ErrImageTooLarge", err)
			}
		})
	}
	// Sanity: the same gate must not reject an image below the limit. A
	// header-only 64x64 PNG has no pixel data, so Decode fails with a
	// non-sentinel error — assert exactly that.
	small := craftPNGHeader(t, 64, 64)
	_, err := e.Recognize(small)
	if errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("64x64 image must not be rejected by the pixel gate: %v", err)
	}
}
