//go:build darwin

package shellagent

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativeMacIconPreservesGlyphAndMasksTile(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			source.SetNRGBA(x, y, color.NRGBA{158, 221, 187, 255})
		}
	}
	glyph := color.NRGBA{23, 63, 58, 255}
	for y := 143; y < 389; y++ {
		for x := 132; x < 380; x++ {
			source.SetNRGBA(x, y, glyph)
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}
	data, err := maskNativeMacIcon(input.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	icon, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range []image.Point{{132, 143}, {379, 388}, {256, 256}} {
		if got := color.NRGBAModel.Convert(icon.At(point.X, point.Y)); got != glyph {
			t.Fatalf("glyph pixel at %v changed: %v", point, got)
		}
	}
	for _, point := range []image.Point{{0, 256}, {49, 256}, {462, 256}, {50, 50}} {
		if _, _, _, a := icon.At(point.X, point.Y).RGBA(); a != 0 {
			t.Fatalf("mask pixel at %v is opaque", point)
		}
	}
	if got := color.NRGBAModel.Convert(icon.At(50, 256)); got != (color.NRGBA{158, 221, 187, 255}) {
		t.Fatalf("tile boundary changed: %v", got)
	}
}

func TestFetchNativeMacIconPrefersMaskableAndFallsBack(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(map[bool]string{true: "maskable", false: "legacy"}[available], func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/icon-512-maskable.png" && !available {
					http.NotFound(w, r)
					return
				}
				w.Write(append(append([]byte(nil), pngSignature...), byte(len(paths))))
			}))
			defer server.Close()
			if _, err := fetchNativeIcon(context.Background(), server.Client(), server.URL); err != nil {
				t.Fatal(err)
			}
			if paths[0] != "/icon-512-maskable.png" || (available && len(paths) != 1) || (!available && (len(paths) != 2 || paths[1] != "/icon-512.png")) {
				t.Fatalf("unexpected icon requests: %v", paths)
			}
		})
	}
}
