package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// StatusColorPalette generates crisp 32x32 circular status indicator PNGs in memory
// for Fedora Wayland StatusNotifierItem / AppIndicator tray icons.
var (
	IconGreenObserving = renderCircleIcon(color.RGBA{R: 34, G: 197, B: 94, A: 255}, color.RGBA{R: 21, G: 128, B: 61, A: 255})
	IconAmberPaused30s = renderCircleIcon(color.RGBA{R: 245, G: 158, B: 11, A: 255}, color.RGBA{R: 180, G: 83, B: 9, A: 255})
	IconBlueExecuting  = renderCircleIcon(color.RGBA{R: 59, G: 130, B: 246, A: 255}, color.RGBA{R: 29, G: 78, B: 216, A: 255})
	IconRedHardStop    = renderCircleIcon(color.RGBA{R: 239, G: 68, B: 68, A: 255}, color.RGBA{R: 185, G: 28, B: 28, A: 255})
	IconSlateDisabled  = renderCircleIcon(color.RGBA{R: 148, G: 163, B: 184, A: 255}, color.RGBA{R: 71, G: 85, B: 105, A: 255})
)

func renderCircleIcon(fill, border color.RGBA) []byte {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	cx, cy := float64(size-1)/2.0, float64(size-1)/2.0
	outerR := 13.5
	innerR := 10.5

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - cx
			dy := float64(y) - cy
			dist := math.Sqrt(dx*dx + dy*dy)
			switch {
			case dist <= innerR:
				img.SetRGBA(x, y, fill)
			case dist <= outerR:
				img.SetRGBA(x, y, border)
			case dist <= outerR+1.0:
				alpha := uint8((outerR + 1.0 - dist) * 255)
				img.SetRGBA(x, y, color.RGBA{R: border.R, G: border.G, B: border.B, A: alpha})
			}
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
