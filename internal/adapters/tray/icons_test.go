package tray

import (
	"bytes"
	"image/png"
	"testing"
)

func TestTrayIconsDecodeAsValid32x32PNG(t *testing.T) {
	icons := map[string][]byte{
		"green": IconGreenObserving,
		"amber": IconAmberPaused30s,
		"blue":  IconBlueExecuting,
		"red":   IconRedHardStop,
		"slate": IconSlateDisabled,
	}

	for name, raw := range icons {
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("icon %s failed PNG decode: %v", name, err)
		}
		b := img.Bounds()
		if b.Dx() != 32 || b.Dy() != 32 {
			t.Fatalf("icon %s expected 32x32, got %dx%d", name, b.Dx(), b.Dy())
		}
	}
}
