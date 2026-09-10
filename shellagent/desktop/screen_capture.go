package desktop

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
)

const maxScreenshotPixels = 32 * 1024 * 1024
const maxScreenshotBytes = 8 * 1024 * 1024

// NSJSONSerialization encodes NSNumber booleans as 1/0. Accept both.
type jsonBool bool

func (b *jsonBool) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", "1":
		*b = true
	case "false", "0", "null":
		*b = false
	default:
		return fmt.Errorf("invalid bool %s", data)
	}
	return nil
}

type screenDisplay struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	ScaleFactor float64  `json:"scaleFactor"`
	Primary     jsonBool `json:"primary"`
	left, top   int
}
type screenRegion struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}
type screenCaptureOptions struct {
	DisplayID string        `json:"displayId"`
	Region    *screenRegion `json:"region"`
}
type screenCaptureNative interface {
	listDisplays() ([]screenDisplay, error)
	captureScreen(screenDisplay, screenRegion) ([]byte, error)
}

func captureRegion(display screenDisplay, requested *screenRegion) (screenRegion, error) {
	region := screenRegion{Width: display.Width, Height: display.Height}
	if requested != nil {
		region = *requested
	}
	if region.X < 0 || region.Y < 0 || region.Width <= 0 || region.Height <= 0 || region.Width > display.Width || region.Height > display.Height || region.X > display.Width-region.Width || region.Y > display.Height-region.Height {
		return region, errors.New("capture region must fit inside the selected display")
	}
	if region.Width > maxScreenshotPixels/region.Height {
		return region, errors.New("screenshot exceeds 32 megapixels; choose a smaller region")
	}
	return region, nil
}
func screenshotResult(data []byte, region screenRegion) (any, error) {
	if len(data) > maxScreenshotBytes {
		return nil, errors.New("screenshot exceeds 8 MiB; choose a smaller region")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("native screenshot is not a valid PNG")
	}
	if config.Width != region.Width || config.Height != region.Height {
		return nil, errors.New("native screenshot dimensions do not match the requested region")
	}
	return map[string]any{"dataUrl": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), "width": config.Width, "height": config.Height, "mimeType": "image/png"}, nil
}
func handleScreenCapture(native screenCaptureNative, command presentationCommand) (any, error) {
	if command.Method != "capture" && command.Method != "listDisplays" {
		return nil, errors.New("unsupported screen method")
	}
	var options screenCaptureOptions
	if command.Method == "capture" && len(command.Args) > 0 {
		if err := decodePresentationArg(command.Args, &options); err != nil {
			return nil, err
		}
	}
	displays, err := native.listDisplays()
	if err != nil {
		return nil, err
	}
	if command.Method == "listDisplays" {
		return displays, nil
	}
	var selected *screenDisplay
	for i := range displays {
		if (options.DisplayID == "" && bool(displays[i].Primary)) || displays[i].ID == options.DisplayID {
			selected = &displays[i]
			break
		}
	}
	if selected == nil {
		return nil, errors.New("display is unavailable; list displays again")
	}
	region, err := captureRegion(*selected, options.Region)
	if err != nil {
		return nil, err
	}
	data, err := native.captureScreen(*selected, region)
	if err != nil {
		return nil, err
	}
	return screenshotResult(data, region)
}
