package engine

import (
	"bytes"
	"errors"
	"fmt"
	"image"
)

// maxDecodePixels caps the declared pixel count of any image decoded from
// untrusted bytes: a small compressed payload can expand to gigabytes of
// RGBA pixels inside the server on a single request (decompression bomb —
// a 4.6 MB PNG declaring 40000x40000 allocated ~6.4 GB during decode).
// 50 MP caps one decode at ~200 MB of pixels while leaving ~25x headroom
// over the dataset's ~1.9 MP photos.
const maxDecodePixels = 50 << 20

// ErrImageTooLarge marks a decode rejection caused by the pixel cap (via
// errors.Is on the returned error), so callers can map it to a 4xx HTTP
// status instead of a backend-failure one.
var ErrImageTooLarge = errors.New("image too large")

// checkPixelCount rejects non-positive and oversized declared dimensions
// before any pixel is decoded. The product is computed in int64 so it
// cannot overflow on 32-bit int platforms.
func checkPixelCount(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("invalid image dimensions %dx%d", w, h)
	}
	if int64(w)*int64(h) > maxDecodePixels {
		return fmt.Errorf("%w: %dx%d exceeds the %d megapixel decode limit",
			ErrImageTooLarge, w, h, maxDecodePixels>>20)
	}
	return nil
}

// decodeLimited decodes b like image.Decode, but first reads only the image
// header (image.DecodeConfig) and rejects images whose declared pixel count
// exceeds maxDecodePixels, so a bomb's pixel data is never allocated. Every
// engine-side decode of untrusted bytes must go through this helper; the
// decoders are registered by the blank imports in engine.go (same package).
func decodeLimited(b []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if err := checkPixelCount(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}
