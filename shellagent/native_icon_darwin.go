//go:build darwin

package shellagent

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// maskNativeMacIcon clips the full-size artwork to the macOS icon grid. Scaling
// the entire image into a tile would also shrink its glyph. These grid sizes
// match Chromium's macOS maskable PWA icons and Apple's icon design templates.
func maskNativeMacIcon(data []byte) ([]byte, error) {
	source, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode the app icon: %w", err)
	}
	bounds := source.Bounds()
	size := bounds.Dx()
	if size != bounds.Dy() || size == 0 {
		return nil, fmt.Errorf("the app icon must be square")
	}
	inset, radius := float64(size)*100/1024, float64(size)*184/1024
	switch size {
	case 16:
		inset, radius = 1, 2.785
	case 32:
		inset, radius = 2, 5.75
	case 64:
		inset, radius = 6, 11.5
	case 128:
		inset, radius = 12, 23
	case 256:
		inset, radius = 25, 46
	case 512:
		inset, radius = 50, 92
	}
	inside := func(x, y float64) bool {
		if x < inset || y < inset || x >= float64(size)-inset || y >= float64(size)-inset {
			return false
		}
		dx := math.Max(math.Max(inset+radius-x, x-(float64(size)-inset-radius)), 0)
		dy := math.Max(math.Max(inset+radius-y, y-(float64(size)-inset-radius)), 0)
		return dx*dx+dy*dy <= radius*radius
	}
	result := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			// Supersample only the mask, preserving the original glyph pixels.
			coverage := 0
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					if inside(float64(x)+(float64(sx)+0.5)/4, float64(y)+(float64(sy)+0.5)/4) {
						coverage++
					}
				}
			}
			if coverage == 0 {
				continue
			}
			r, g, b, a := source.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			// Composite over white before masking, as Chrome does for transparent
			// artwork. RGBA returns premultiplied channels.
			result.SetNRGBA(x, y, color.NRGBA{
				R: uint8((r + 65535 - a) / 257), G: uint8((g + 65535 - a) / 257),
				B: uint8((b + 65535 - a) / 257), A: uint8((coverage*255 + 8) / 16),
			})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, result); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
