package engine

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Annotate renders detection results onto a copy of the image: per face,
// corner brackets and an "NN · name conf%" label — the same 1-based numbering
// and palette as the web UI's canvas overlay (teal for known identities,
// amber for unknown). faces must be in the order the results list showed
// them; NN in the label is the 1-based index. Returns JPEG bytes.
func Annotate(imgBytes []byte, faces []Face) ([]byte, error) {
	src, err := decodeLimited(imgBytes)
	if err != nil {
		return nil, fmt.Errorf("decode source image: %w", err)
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)

	scale := math.Max(float64(b.Dx()), float64(b.Dy())) / 900
	lw := int(2.5 * scale) // bracket line width
	if lw < 2 {
		lw = 2
	}
	arm := int(14 * scale) // bracket arm length
	if arm < 10 {
		arm = 10
	}

	for i, f := range faces {
		known := f.Name != "" && f.Name != "unknown"
		col := annotUnknown
		if known {
			col = annotMatch
		}
		drawBrackets(dst, f.BBox, col, lw, arm)

		conf := int(math.Round(math.Max(0, f.Confidence) * 100))
		who := "unknown"
		if known {
			who = f.Name
		}
		drawFaceLabel(dst, fmt.Sprintf("%02d · %s %d%%", i+1, who, conf), f.BBox, col, scale)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 92}); err != nil {
		return nil, fmt.Errorf("encode annotated image: %w", err)
	}
	return buf.Bytes(), nil
}

var (
	annotMatch   = color.RGBA{R: 56, G: 224, B: 200, A: 255} // teal — matches the web UI
	annotUnknown = color.RGBA{R: 245, G: 181, B: 63, A: 255} // amber
	annotLabelBg = color.RGBA{R: 11, G: 14, B: 18, A: 217}   // rgba(11,14,18,0.85)
)

// drawBrackets draws the four corner brackets of the bbox (clipped to the
// image), mirroring the web overlay's framing.
func drawBrackets(dst *image.NRGBA, bb [4]float64, col color.RGBA, lw, arm int) {
	x0 := int(math.Round(bb[0]))
	y0 := int(math.Round(bb[1]))
	x1 := int(math.Round(bb[0] + bb[2]))
	y1 := int(math.Round(bb[1] + bb[3]))
	corners := [][4]int{
		{x0, y0, 1, 1}, {x1, y0, -1, 1},
		{x0, y1, 1, -1}, {x1, y1, -1, -1},
	}
	for _, c := range corners {
		cx, cy, sx, sy := c[0], c[1], c[2], c[3]
		hline(dst, cx, cx+arm*sx, cy, lw, col)
		vline(dst, cx, cy, cy+arm*sy, lw, col)
	}
}

func hline(dst *image.NRGBA, x0, x1, y, lw int, col color.RGBA) {
	if x1 < x0 {
		x0, x1 = x1, x0
	}
	for dy := 0; dy < lw; dy++ {
		for x := x0; x <= x1; x++ {
			setPx(dst, x, y+dy, col)
		}
	}
}

func vline(dst *image.NRGBA, x, y0, y1, lw int, col color.RGBA) {
	if y1 < y0 {
		y0, y1 = y1, y0
	}
	for dx := 0; dx < lw; dx++ {
		for y := y0; y <= y1; y++ {
			setPx(dst, x+dx, y, col)
		}
	}
}

// setPx writes one pixel, ignoring out-of-bounds coordinates.
func setPx(dst *image.NRGBA, x, y int, col color.RGBA) {
	if x < dst.Bounds().Min.X || y < dst.Bounds().Min.Y ||
		x >= dst.Bounds().Max.X || y >= dst.Bounds().Max.Y {
		return
	}
	off := dst.PixOffset(x, y)
	dst.Pix[off+0] = col.R
	dst.Pix[off+1] = col.G
	dst.Pix[off+2] = col.B
	dst.Pix[off+3] = 255
}

// drawFaceLabel draws label in a filled box above the bbox (below it when the
// box touches the top edge), using the built-in 7x13 bitmap font.
func drawFaceLabel(dst *image.NRGBA, label string, bb [4]float64, col color.RGBA, scale float64) {
	face := basicfont.Face7x13
	tw := (&font.Drawer{Face: face}).MeasureString(label).Ceil()
	pad := int(6*scale) + 2
	lh := face.Height

	x := int(math.Round(bb[0]))
	y := int(math.Round(bb[1])) - lh - pad*2 // above the box…
	if y < 0 {
		y = int(math.Round(bb[1] + bb[3])) // …else below
	}

	// Background panel (clipped to the image).
	panel := image.Rect(x-1, y-1, x+tw+pad*2+1, y+lh+pad+1)
	draw.Draw(dst, panel.Intersect(dst.Bounds()), image.NewUniform(annotLabelBg), image.Point{}, draw.Src)

	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x+pad, y+pad+face.Ascent),
	}
	d.DrawString(label)
}
