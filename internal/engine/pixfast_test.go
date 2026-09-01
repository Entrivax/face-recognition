package engine

// Equivalence tests for the direct-Pix fast paths in engine.go and
// preprocess.go. Every fast path here is compared against the original
// generic per-pixel implementation (At()/Set()/RGBA()), which is kept as a
// local reference — the two must agree byte-for-byte, including for
// semi-transparent pixels (the premultiply/unpremultiply edge cases).

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// alphaGradientNRGBA fills a w x h NRGBA with deterministic pixels that sweep
// every alpha byte (0 and 255 included), exercising the premultiply /
// unpremultiply edge cases, including invalid premultiplied values (R > A).
func alphaGradientNRGBA(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 17 % 256),
				G: uint8(y * 29 % 256),
				B: uint8((x + y) * 7 % 256),
				A: uint8((x*7 + y*13) % 256),
			})
		}
	}
	return img
}

// alphaGradientRGBA is alphaGradientNRGBA for *image.RGBA (raw premultiplied
// bytes, again including invalid R > A combinations).
func alphaGradientRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 17 % 256),
				G: uint8(y * 29 % 256),
				B: uint8((x + y) * 7 % 256),
				A: uint8((x*7 + y*13) % 256),
			})
		}
	}
	return img
}

// cropGeneric is the original cropImage body: per-pixel At/Set copy.
func cropGeneric(src image.Image, r image.Rectangle) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			dst.Set(x, y, src.At(r.Min.X+x, r.Min.Y+y))
		}
	}
	return dst
}

// resizeGeneric is the original resizeBilinear body: per-pixel bilinear + Set.
func resizeGeneric(src image.Image, w, h int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	if sw == 0 || sh == 0 {
		return dst
	}
	for y := 0; y < h; y++ {
		fy := (float64(y)+0.5)*float64(sh)/float64(h) - 0.5 + float64(sb.Min.Y)
		for x := 0; x < w; x++ {
			fx := (float64(x)+0.5)*float64(sw)/float64(w) - 0.5 + float64(sb.Min.X)
			dst.Set(x, y, bilinear(src, sb, fx, fy))
		}
	}
	return dst
}

// affineWarpGeneric is the original affineWarp body: per-pixel bilinear + Set.
func affineWarpGeneric(src image.Image, m [2][3]float64, w, h int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx := m[0][0]*float64(x) + m[0][1]*float64(y) + m[0][2]
			fy := m[1][0]*float64(x) + m[1][1]*float64(y) + m[1][2]
			c := bilinear(src, sb, fx, fy)
			dst.Set(x, y, c)
		}
	}
	return dst
}

func nrgbaPixEqual(a, b *image.NRGBA) bool {
	return a.Rect == b.Rect && a.Stride == b.Stride && bytes.Equal(a.Pix, b.Pix)
}

func TestCropImageFastPathsExact(t *testing.T) {
	full := alphaGradientNRGBA(37, 23)
	sub := full.SubImage(image.Rect(3, 5, 30, 20)).(*image.NRGBA) // non-zero origin
	cases := []struct {
		name string
		src  image.Image
		rect image.Rectangle
	}{
		{"nrgba", full, image.Rect(5, 3, 29, 19)},
		{"nrgba touching edges", full, image.Rect(0, 0, 37, 23)},
		{"nrgba subimage", sub, image.Rect(6, 7, 25, 17)},
		{"rgba", alphaGradientRGBA(31, 19), image.Rect(2, 4, 27, 15)},
		// Rects reaching outside the source: the fast path must decline (the
		// r.In guard) and the generic At/Set loop must run — At() returns
		// transparent zeros outside the bounds, which must be preserved.
		{"nrgba rect overruns source", full, image.Rect(30, 18, 50, 35)},
		{"rgba rect overruns source", alphaGradientRGBA(20, 15), image.Rect(0, 0, 25, 20)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cropImage(c.src, c.rect)
			want := cropGeneric(c.src, c.rect)
			if !nrgbaPixEqual(got, want) {
				t.Fatalf("fast path differs from generic At/Set copy")
			}
		})
	}
}

func TestResizeBilinearFastPathsExact(t *testing.T) {
	sub := alphaGradientNRGBA(31, 25).SubImage(image.Rect(3, 5, 28, 22)).(*image.NRGBA)
	cases := []struct {
		name string
		src  image.Image
	}{
		{"nrgba", alphaGradientNRGBA(23, 17)},
		{"nrgba subimage", sub},
		{"rgba", alphaGradientRGBA(19, 13)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resizeBilinear(c.src, 37, 29)
			want := resizeGeneric(c.src, 37, 29)
			if !nrgbaPixEqual(got, want) {
				t.Fatalf("fast path differs from generic bilinear+Set path")
			}
		})
	}
}

func TestAffineWarpFastPathsExact(t *testing.T) {
	m := [2][3]float64{{0.9, -0.15, 7.3}, {0.15, 0.85, -3.1}}
	cases := []struct {
		name string
		src  image.Image
	}{
		{"nrgba", alphaGradientNRGBA(29, 21)},
		{"rgba", alphaGradientRGBA(25, 17)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := affineWarp(c.src, m, 41, 33)
			want := affineWarpGeneric(c.src, m, 41, 33)
			if !nrgbaPixEqual(got, want) {
				t.Fatalf("fast path differs from generic bilinear+Set path")
			}
		})
	}
}

// preprocessFaceGeneric is the original preprocessFace body: per-pixel
// At().RGBA() reads with the >>8 conversion.
func preprocessFaceGeneric(aligned *image.NRGBA) []float32 {
	const inv = 1.0 / 127.5
	const mean = 127.5
	const size = 112
	plane := size * size
	tensor := make([]float32, 3*plane)
	b := aligned.Bounds()
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			r, g, bl, _ := aligned.At(b.Min.X+x, b.Min.Y+y).RGBA()
			rf := float32(r >> 8)
			gf := float32(g >> 8)
			bf := float32(bl >> 8)
			idx := y*size + x
			tensor[0*plane+idx] = (rf - mean) * inv
			tensor[1*plane+idx] = (gf - mean) * inv
			tensor[2*plane+idx] = (bf - mean) * inv
		}
	}
	return tensor
}

func float32SlicesEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] { // bitwise equality is the point here
			return false
		}
	}
	return true
}

func TestPreprocessFaceDirectIndexing(t *testing.T) {
	cases := []struct {
		name string
		img  *image.NRGBA
	}{
		{"full alpha sweep", alphaGradientNRGBA(112, 112)},
		// Sub-image with non-zero bounds origin (PixOffset path).
		{"subimage", alphaGradientNRGBA(140, 140).SubImage(image.Rect(7, 9, 119, 121)).(*image.NRGBA)},
		// Smaller than 112x112: exercises the generic fallback (At() returns
		// transparent zeros outside the rect).
		{"small fallback", alphaGradientNRGBA(40, 40)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := preprocessFace(c.img)
			want := preprocessFaceGeneric(c.img)
			if !float32SlicesEqual(got, want) {
				t.Fatalf("direct-indexing tensor differs from At().RGBA() path")
			}
		})
	}
}

// preprocessDetectTensorGeneric rebuilds the detector tensor via the original
// letterbox math plus the per-pixel At().RGBA() loop, for comparison with the
// direct-indexing fast path.
func preprocessDetectTensorGeneric(t *testing.T, imgBytes []byte) []float32 {
	t.Helper()
	src, _, err := image.Decode(bytes.NewReader(imgBytes))
	if err != nil {
		t.Fatal(err)
	}
	b := src.Bounds()
	origW, origH := b.Dx(), b.Dy()
	imRatio := float64(origH) / float64(origW)
	var newW, newH int
	if imRatio > 1.0 {
		newH = detInputSize
		newW = int(float64(newH) / imRatio)
	} else {
		newW = detInputSize
		newH = int(float64(newW) * imRatio)
	}
	resized := resizeBilinear(src, newW, newH)
	const inv = 1.0 / 128.0
	const mean = 127.5
	padVal := float32((0.0 - mean) * inv)
	tensor := make([]float32, 3*detInputSize*detInputSize)
	for i := range tensor {
		tensor[i] = padVal
	}
	plane := detInputSize * detInputSize
	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			r, g, bl, _ := resized.At(x, y).RGBA()
			rf := float32(r >> 8)
			gf := float32(g >> 8)
			bf := float32(bl >> 8)
			idx := y*detInputSize + x
			tensor[0*plane+idx] = (rf - mean) * inv
			tensor[1*plane+idx] = (gf - mean) * inv
			tensor[2*plane+idx] = (bf - mean) * inv
		}
	}
	return tensor
}

func TestPreprocessDetectTensorDirectIndexing(t *testing.T) {
	// PNG keeps the alpha channel, so the semi-transparent premultiply branch
	// of the direct-indexing loop is exercised; the JPEG case covers the
	// opaque branch on a YCbCr-decoded source.
	cases := map[string][]byte{}
	var buf bytes.Buffer
	if err := png.Encode(&buf, alphaGradientNRGBA(320, 200)); err != nil {
		t.Fatal(err)
	}
	cases["png-with-alpha"] = append([]byte(nil), buf.Bytes()...)
	buf.Reset()
	if err := png.Encode(&buf, alphaGradientNRGBA(500, 250)); err != nil {
		t.Fatal(err)
	}
	cases["png-landscape"] = append([]byte(nil), buf.Bytes()...)
	for name, imgBytes := range cases {
		t.Run(name, func(t *testing.T) {
			lb, err := preprocessDetect(imgBytes)
			if err != nil {
				t.Fatal(err)
			}
			want := preprocessDetectTensorGeneric(t, imgBytes)
			if !float32SlicesEqual(lb.tensor, want) {
				t.Fatalf("direct-indexing tensor differs from At().RGBA() path")
			}
		})
	}
}

func TestAnchorCentersCacheStable(t *testing.T) {
	a := anchorCenters(8, 8, 16, 2) // populates the cache
	b := anchorCenters(8, 8, 16, 2) // must come from the cache, identical
	if len(a) != len(b) || len(a) != 8*8*2 {
		t.Fatalf("cached grid len = %d/%d, want %d", len(a), len(b), 8*8*2)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("cached grid changed at %d: %v vs %v", i, a[i], b[i])
		}
	}
}
