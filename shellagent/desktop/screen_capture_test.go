package desktop

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

type fakeScreenCapture struct {
	displays []screenDisplay
	png      []byte
	err      error
	calls    int
}

func (f *fakeScreenCapture) listDisplays() ([]screenDisplay, error) {
	if f.displays != nil {
		return f.displays, f.err
	}
	return []screenDisplay{{ID: "main", Name: "Primary", Width: 100, Height: 80, Primary: true}}, f.err
}
func (f *fakeScreenCapture) captureScreen(_ screenDisplay, region screenRegion) ([]byte, error) {
	f.calls++
	if f.png != nil || f.err != nil {
		return f.png, f.err
	}
	img := image.NewRGBA(image.Rect(0, 0, region.Width, region.Height))
	img.Set(0, 0, color.RGBA{R: 200, A: 255})
	var data bytes.Buffer
	err := png.Encode(&data, img)
	return data.Bytes(), err
}

func TestScreenCaptureValidationAndPNG(t *testing.T) {
	native := &fakeScreenCapture{}
	request := func(options any) (any, error) {
		return handleScreenCapture(native, presentationCommand{Method: "capture", Args: []any{options}})
	}
	for _, options := range []any{
		map[string]any{"displayId": "missing"},
		map[string]any{"region": map[string]any{"x": -1, "y": 0, "width": 10, "height": 10}},
		map[string]any{"region": map[string]any{"x": 95, "y": 0, "width": 10, "height": 10}},
		map[string]any{"region": map[string]any{"x": 0, "y": 0, "width": 0, "height": 10}},
		map[string]any{"region": map[string]any{"x": 0.5, "y": 0, "width": 10, "height": 10}},
	} {
		if _, err := request(options); err == nil {
			t.Fatalf("accepted invalid options: %v", options)
		}
	}
	if native.calls != 0 {
		t.Fatal("invalid request reached screen capture")
	}
	result, err := request(map[string]any{"region": map[string]any{"x": 20, "y": 10, "width": 10, "height": 12}})
	if err != nil {
		t.Fatal(err)
	}
	value := result.(map[string]any)
	if value["width"] != 10 || value["height"] != 12 || !strings.HasPrefix(value["dataUrl"].(string), "data:image/png;base64,") {
		t.Fatalf("result=%v", value)
	}
	if _, err := captureRegion(screenDisplay{Width: 100000, Height: 100000}, nil); err == nil {
		t.Fatal("oversized capture accepted")
	}
	if _, err := screenshotResult([]byte("not png"), screenRegion{Width: 1, Height: 1}); err == nil {
		t.Fatal("invalid PNG accepted")
	}
	if _, err := screenshotResult(make([]byte, maxScreenshotBytes+1), screenRegion{Width: 1, Height: 1}); err == nil {
		t.Fatal("oversized PNG accepted")
	}
}

func TestHandleScreenCaptureUsesPrimaryDisplay(t *testing.T) {
	native := &fakeScreenCapture{
		displays: []screenDisplay{{ID: "1", Width: 1, Height: 1, Primary: true}},
	}
	result, err := handleScreenCapture(native, presentationCommand{Service: "screen", Method: "capture"})
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]any)
	if payload["width"] != 1 || payload["mimeType"] != "image/png" {
		t.Fatalf("result=%v", payload)
	}
}

func TestScreenDisplayPrimaryAcceptsNSJSONNumber(t *testing.T) {
	var displays []screenDisplay
	if err := json.Unmarshal([]byte(`[{"id":"1","name":"Main","width":100,"height":80,"scaleFactor":2,"primary":1}]`), &displays); err != nil {
		t.Fatal(err)
	}
	if !bool(displays[0].Primary) || displays[0].Width != 100 {
		t.Fatalf("primary number: %+v", displays[0])
	}
	if err := json.Unmarshal([]byte(`[{"id":"2","primary":true,"width":10,"height":10}]`), &displays); err != nil {
		t.Fatal(err)
	}
	if !bool(displays[0].Primary) {
		t.Fatal("JSON true should remain primary")
	}
	listed, err := handleScreenCapture(&fakeScreenCapture{}, presentationCommand{Method: "listDisplays"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"primary":true`) {
		t.Fatalf("PWA payload should use a JSON boolean: %s", encoded)
	}
}
