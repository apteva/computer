package local

import (
	"image"
	"image/jpeg"
	"image/png"
	"bytes"
	"os"
	"testing"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

// TestScreenshotDimensions launches Chrome, takes a screenshot, and verifies
// the returned image matches the declared DisplaySize (not device pixels).
//
// RUN_BROWSER_TEST=1 go test -v -run TestScreenshotDimensions -timeout 30s
func TestScreenshotDimensions(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}

	display := computer.DisplaySize{Width: 1024, Height: 768}
	comp, err := New(display)
	if err != nil {
		t.Fatalf("failed to create browser: %v", err)
	}
	defer comp.Close()

	// Get device pixel ratio
	var dpr float64
	chromedp.Run(comp.ctx, chromedp.Evaluate(`window.devicePixelRatio`, &dpr))
	t.Logf("devicePixelRatio: %.1f", dpr)

	// Navigate so there's content
	comp.Execute(computer.Action{Type: "navigate", URL: "https://example.com"})

	// Take screenshot
	buf, err := comp.Screenshot()
	if err != nil {
		t.Fatalf("screenshot failed: %v", err)
	}
	t.Logf("screenshot: %d bytes", len(buf))

	// Decode image to get actual dimensions
	var img image.Image
	if buf[0] == 0xFF && buf[1] == 0xD8 {
		img, err = jpeg.Decode(bytes.NewReader(buf))
		t.Log("format: JPEG")
	} else {
		img, err = png.Decode(bytes.NewReader(buf))
		t.Log("format: PNG")
	}
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	bounds := img.Bounds()
	actualW, actualH := bounds.Dx(), bounds.Dy()
	realDisplay := comp.DisplaySize()
	t.Logf("image dimensions: %dx%d (display: %dx%d, requested: %dx%d, DPR: %.1f)",
		actualW, actualH, realDisplay.Width, realDisplay.Height, display.Width, display.Height, dpr)

	// Screenshot must match the actual DisplaySize (which accounts for viewport vs window)
	if actualW != realDisplay.Width || actualH != realDisplay.Height {
		t.Errorf("FAIL: screenshot %dx%d does not match display %dx%d (DPR=%.1f)",
			actualW, actualH, realDisplay.Width, realDisplay.Height, dpr)
	} else {
		t.Logf("PASS: screenshot matches display size %dx%d", realDisplay.Width, realDisplay.Height)
	}
}

// TestScreenshotDownscale verifies that scaleToDisplay correctly resizes
// a larger image down to the display size.
func TestScreenshotDownscale(t *testing.T) {
	comp := &Computer{
		display: computer.DisplaySize{Width: 1024, Height: 768},
	}

	// Create a fake 2048x1536 JPEG (Retina 2x)
	img := image.NewRGBA(image.Rect(0, 0, 2048, 1536))
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	original := buf.Bytes()

	t.Logf("original: %dx%d (%d bytes)", 2048, 1536, len(original))

	// Downscale
	scaled, err := comp.scaleToDisplay(original)
	if err != nil {
		t.Fatalf("scaleToDisplay error: %v", err)
	}

	// Decode result
	result, err := jpeg.Decode(bytes.NewReader(scaled))
	if err != nil {
		t.Fatalf("decode scaled: %v", err)
	}

	bounds := result.Bounds()
	t.Logf("scaled: %dx%d (%d bytes)", bounds.Dx(), bounds.Dy(), len(scaled))

	if bounds.Dx() != 1024 || bounds.Dy() != 768 {
		t.Errorf("FAIL: expected 1024x768, got %dx%d", bounds.Dx(), bounds.Dy())
	} else {
		t.Log("PASS: downscaled to 1024x768")
	}
}

// TestScreenshotNoScale verifies that scaleToDisplay is a no-op
// when the image already matches the display size.
func TestScreenshotNoScale(t *testing.T) {
	comp := &Computer{
		display: computer.DisplaySize{Width: 1024, Height: 768},
	}

	// Create a 1024x768 JPEG (no scaling needed)
	img := image.NewRGBA(image.Rect(0, 0, 1024, 768))
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	original := buf.Bytes()

	scaled, _ := comp.scaleToDisplay(original)

	// Should be identical (no scaling)
	if len(scaled) != len(original) {
		t.Errorf("FAIL: size changed from %d to %d — should be no-op", len(original), len(scaled))
	} else {
		t.Log("PASS: no scaling applied for matching dimensions")
	}
}
