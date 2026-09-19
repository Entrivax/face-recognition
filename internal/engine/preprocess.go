package engine

import (
	"fmt"
	"image"
)

// Preprocessing converts decoded images into the float32 CHW tensors the ONNX
// models expect, matching the reference insightface/OpenCV pipeline exactly.
//
// Detector (SCRFD): letterbox the image into a 640x640 canvas (aspect ratio
// preserved, top-left anchored, zero padding), then normalise with
// scalefactor=1/128 and mean=(127.5,127.5,127.5) in RGB order:
//     out = (v - 127.5) * (1.0/128.0)
// (OpenCV decodes BGR but uses swapRB=true, which yields RGB — the same order
// Go's image package produces, so no channel swap is needed on our side.)
//
// Embedder (ArcFace): take the already-aligned 112x112 image and normalise
// with scalefactor=1/127.5, mean=127.5, RGB:
//     out = (v - 127.5) * (1.0/127.5)

const detInputSize = 640

// detLetterbox holds the detector input tensor plus the scale factor needed to
// map detected coordinates back to the original image space.
type detLetterbox struct {
	tensor []float32 // 1x3x640x640 CHW
	scale  float64   // det_scale: resizedHeight / origHeight
	newW   int
	newH   int
}

// preprocessDetect decodes imgBytes (pixel-count gated, like every engine
// decode) and builds the SCRFD input tensor.
func preprocessDetect(imgBytes []byte) (*detLetterbox, error) {
	src, err := decodeLimited(imgBytes)
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return letterboxDetect(src)
}

// letterboxDetect builds the SCRFD input tensor from an already-decoded
// image, so callers that also align faces can decode the bytes only once
// (mirrors alignFaceFromImage on the alignment side).
func letterboxDetect(src image.Image) (*detLetterbox, error) {
	b := src.Bounds()
	origW, origH := b.Dx(), b.Dy()
	if origW == 0 || origH == 0 {
		return nil, fmt.Errorf("empty image")
	}

	// Letterbox dimensions (mirror python/infer.py detect()).
	imRatio := float64(origH) / float64(origW)
	var newW, newH int
	if imRatio > 1.0 { // model is square, so model_ratio == 1
		newH = detInputSize
		newW = int(float64(newH) / imRatio)
	} else {
		newW = detInputSize
		newH = int(float64(newW) * imRatio)
	}
	scale := float64(newH) / float64(origH)

	resized := resizeBilinear(src, newW, newH)

	// Build 1x3x640x640 CHW float tensor; padding stays 0 after normalisation
	// because (0-127.5)/128 is a constant we must apply to the padded area too.
	const inv = 1.0 / 128.0
	const mean = 127.5
	padVal := float32((0.0 - mean) * inv)
	tensor := make([]float32, 3*detInputSize*detInputSize)
	for i := range tensor {
		tensor[i] = padVal
	}
	plane := detInputSize * detInputSize
	// resized is always *image.NRGBA (resizeBilinear's output type), so read
	// its Pix bytes directly instead of going through resized.At().RGBA().
	// Exactness: for opaque pixels (A==0xff) NRGBA.RGBA() returns v8*0x101 and
	// (v8*0x101)>>8 == v8 — the same value the generic path's r>>8 produces —
	// and v8*0x101/257 == v8 exactly in float64. Semi-transparent pixels
	// (only possible for sources with an alpha channel) replicate the
	// premultiplication NRGBA.RGBA() performs before the >>8, so every
	// tensor value is bit-identical to the generic loop.
	for y := 0; y < newH; y++ {
		row := resized.Pix[resized.PixOffset(0, y):]
		idx := y * detInputSize
		for x := 0; x < newW; x++ {
			i := x * 4
			r8, g8, b8, a8 := row[i], row[i+1], row[i+2], row[i+3]
			var rf, gf, bf float32
			if a8 == 0xff {
				rf = float32(r8)
				gf = float32(g8)
				bf = float32(b8)
			} else {
				// NRGBA.RGBA() premultiplies: v16 = v8*0x101*A/0xff.
				a := uint32(a8)
				rf = float32((uint32(r8) * 0x101 * a / 0xff) >> 8)
				gf = float32((uint32(g8) * 0x101 * a / 0xff) >> 8)
				bf = float32((uint32(b8) * 0x101 * a / 0xff) >> 8)
			}
			tensor[0*plane+idx] = (rf - mean) * inv
			tensor[1*plane+idx] = (gf - mean) * inv
			tensor[2*plane+idx] = (bf - mean) * inv
			idx++
		}
	}
	return &detLetterbox{tensor: tensor, scale: scale, newW: newW, newH: newH}, nil
}

// preprocessFace builds the 1x3x112x112 CHW float tensor for ArcFace from an
// already-aligned 112x112 image.
func preprocessFace(aligned *image.NRGBA) []float32 {
	const inv = 1.0 / 127.5
	const mean = 127.5
	const size = 112
	plane := size * size
	tensor := make([]float32, 3*plane)
	b := aligned.Bounds()
	// The aligned input is exactly size x size (affineWarp / resizeBilinear
	// output), so read its Pix bytes directly — byte-exact versus the generic
	// At().RGBA() path for the same reason as preprocessDetect (opaque
	// NRGBA pixels: v8*0x101>>8 == v8; semi-transparent ones replicate the
	// premultiplication below).
	if aligned.Rect.Dx() >= size && aligned.Rect.Dy() >= size {
		for y := 0; y < size; y++ {
			i := aligned.PixOffset(b.Min.X, b.Min.Y+y)
			idx := y * size
			for x := 0; x < size; x++ {
				r8, g8, b8, a8 := aligned.Pix[i], aligned.Pix[i+1], aligned.Pix[i+2], aligned.Pix[i+3]
				var rf, gf, bf float32
				if a8 == 0xff {
					rf = float32(r8)
					gf = float32(g8)
					bf = float32(b8)
				} else {
					a := uint32(a8)
					rf = float32((uint32(r8) * 0x101 * a / 0xff) >> 8)
					gf = float32((uint32(g8) * 0x101 * a / 0xff) >> 8)
					bf = float32((uint32(b8) * 0x101 * a / 0xff) >> 8)
				}
				tensor[0*plane+idx] = (rf - mean) * inv
				tensor[1*plane+idx] = (gf - mean) * inv
				tensor[2*plane+idx] = (bf - mean) * inv
				i += 4
				idx++
			}
		}
		return tensor
	}
	// Generic fallback (input smaller than 112x112: At() returns transparent
	// zeros outside the rect, exactly like the original loop).
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
