// Package local implements the Computer interface using a local Chrome/Chromium via CDP.
// It auto-launches Chrome if not already running.
package local

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"golang.org/x/image/draw"
)

type Computer struct {
	display     computer.DisplaySize
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
}

// New creates a local Chrome-backed Computer.
// It launches a new Chrome instance with the given display size.
// New creates a local Chrome-backed Computer.
// Launches headed if DISPLAY is set, headless otherwise.
func New(display computer.DisplaySize) (*Computer, error) {
	// Mac and Windows always have a display; Linux needs DISPLAY set
	headless := runtime.GOOS == "linux" && os.Getenv("DISPLAY") == ""
	if os.Getenv("APTEVA_HEADLESS_BROWSER") == "1" {
		headless = true
	}

	fmt.Fprintf(os.Stderr, "[BROWSER] start: goos=%s goarch=%s headless=%v display=%dx%d DISPLAY=%q APTEVA_HEADLESS_BROWSER=%q CHROME_BIN=%q PATH_has_chrome=unknown\n",
		runtime.GOOS, runtime.GOARCH, headless,
		display.Width, display.Height,
		os.Getenv("DISPLAY"), os.Getenv("APTEVA_HEADLESS_BROWSER"),
		os.Getenv("CHROME_BIN"),
	)

	// In headed mode the OS window has a real chrome bar (tabs, address
	// bar) ~140px tall that eats into the visible area. Pad the window
	// height in headed mode only so a human watching sees the same
	// 1600×800 viewport the agent does. Headless mode has no chrome,
	// so the request maps 1:1 with no padding.
	winW, winH := display.Width, display.Height
	if !headless {
		winH += 140
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(winW, winH),
		chromedp.Flag("headless", headless),
		chromedp.Flag("disable-gpu", headless),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-popup-blocking", true),
	)
	// --no-sandbox is required on Linux (root/containers) but on Windows
	// it breaks the network service IPC: Chrome launches and CDP connects,
	// but every navigation fails with ERR_CONNECTION_RESET because the
	// network service can't initialize without the sandbox scaffolding.
	if runtime.GOOS != "windows" {
		opts = append(opts, chromedp.Flag("no-sandbox", true))
	}

	// Windows-specific workarounds for ERR_CONNECTION_RESET. These target
	// the three most common causes on Windows when Chrome launches but
	// connections are reset at the TCP/TLS layer:
	//   1. QUIC handshake rejected by corporate middleboxes / some AVs —
	//      Chrome tries QUIC first for many hosts, hits RST, should fall
	//      back to TCP but the reset bubbles up. Disable QUIC to force TCP.
	//   2. Antivirus / Defender hooking Chrome's network service sandbox.
	//      Disabling NetworkServiceSandbox lets the network service run
	//      without the Win32k lockdown that some AVs incompatibly filter.
	//   3. System proxy auto-detection (WPAD) finding a broken/blocking
	//      proxy; we do NOT default to --no-proxy-server because many
	//      legitimate users need system proxies, but it's exposed below.
	// Skippable via APTEVA_CHROME_DEFAULT_WIN=0 if they turn out to hurt.
	if runtime.GOOS == "windows" && os.Getenv("APTEVA_CHROME_DEFAULT_WIN") != "0" {
		opts = append(opts,
			chromedp.Flag("disable-quic", true),
			chromedp.Flag("disable-features", "NetworkServiceSandbox,RendererCodeIntegrity"),
		)
	}

	// Escape hatch: pass arbitrary Chrome flags via env var, e.g.
	//   APTEVA_CHROME_FLAGS="--no-proxy-server --disable-features=UseDnsHttpsSvcb"
	// Useful for quickly validating a fix on a user machine without a
	// rebuild cycle. Flags may be space-separated; only --key / --key=value
	// forms are supported (no quoted values).
	if extra := os.Getenv("APTEVA_CHROME_FLAGS"); extra != "" {
		for _, tok := range strings.Fields(extra) {
			tok = strings.TrimPrefix(tok, "--")
			if eq := strings.IndexByte(tok, '='); eq >= 0 {
				opts = append(opts, chromedp.Flag(tok[:eq], tok[eq+1:]))
			} else {
				opts = append(opts, chromedp.Flag(tok, true))
			}
		}
		fmt.Fprintf(os.Stderr, "[BROWSER] APTEVA_CHROME_FLAGS=%q applied\n", extra)
	}

	fmt.Fprintf(os.Stderr, "[BROWSER] allocator opts count=%d (no-sandbox=%v win-defaults=%v)\n",
		len(opts), runtime.GOOS != "windows",
		runtime.GOOS == "windows" && os.Getenv("APTEVA_CHROME_DEFAULT_WIN") != "0")

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)

	// Verify Chrome launches by running a simple command
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("local chrome: failed to start: %w (goos=%s)", err, runtime.GOOS)
	}

	// Pin the viewport to exactly the requested size via CDP's
	// Emulation.setDeviceMetricsOverride. chromedp.WindowSize() sets the
	// OS window dimensions, but Chrome (even in headless mode) reserves
	// a default toolbar height inside its layout, so the actual viewport
	// ends up ~140px shorter than requested. This override bypasses
	// window sizing entirely — same approach Puppeteer and Playwright
	// use — so window.innerWidth/innerHeight match exactly what the
	// caller asked for. mobile=false keeps desktop layout; deviceScaleFactor=1
	// avoids retina double-pixel screenshots that would then need
	// downscaling in scaleToDisplay.
	if err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(
			int64(display.Width), int64(display.Height), 1, false,
		),
	); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("local chrome: failed to set viewport: %w", err)
	}

	// Verify the override stuck.
	var vpWidth, vpHeight int
	chromedp.Run(ctx, chromedp.Evaluate(`window.innerWidth`, &vpWidth))
	chromedp.Run(ctx, chromedp.Evaluate(`window.innerHeight`, &vpHeight))

	fmt.Fprintf(os.Stderr, "[BROWSER] Chrome launched: requested=%dx%d viewport=%dx%d headless=%v\n",
		display.Width, display.Height, vpWidth, vpHeight, headless)

	// User agent + process identity. Useful when diagnosing sandbox /
	// elevation issues ("running as Admin" on Windows refuses to sandbox).
	var ua string
	chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &ua))
	fmt.Fprintf(os.Stderr, "[BROWSER] pid=%d uid=%d ua=%q\n", os.Getpid(), os.Getuid(), ua)

	return &Computer{
		display:     display,
		ctx:         ctx,
		cancel:      cancel,
		allocCancel: allocCancel,
	}, nil
}

func (c *Computer) Execute(action computer.Action) ([]byte, error) {
	switch action.Type {
	case "screenshot":
		return c.Screenshot()

	case "navigate":
		fmt.Fprintf(os.Stderr, "[BROWSER] navigate to %s\n", action.URL)
		if err := chromedp.Run(c.ctx, chromedp.Navigate(action.URL)); err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSER] navigate ERROR: %v\n", err)
			return nil, fmt.Errorf("navigate: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
		var url, title string
		chromedp.Run(c.ctx, chromedp.Location(&url))
		chromedp.Run(c.ctx, chromedp.Title(&title))
		fmt.Fprintf(os.Stderr, "[BROWSER] navigate done: URL=%s title=%q\n", url, title)
		return c.Screenshot()

	case "click":
		var urlBefore string
		chromedp.Run(c.ctx, chromedp.Location(&urlBefore))
		fmt.Fprintf(os.Stderr, "[BROWSER] click (%d,%d) on %s (display=%dx%d)\n",
			action.X, action.Y, urlBefore, c.display.Width, c.display.Height)
		if err := chromedp.Run(c.ctx,
			chromedp.MouseClickXY(float64(action.X), float64(action.Y)),
		); err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSER] click ERROR: %v\n", err)
			return nil, fmt.Errorf("click: %w", err)
		}
		// Wait for potential navigation to complete
		chromedp.Run(c.ctx, chromedp.WaitReady("body", chromedp.ByQuery))
		time.Sleep(200 * time.Millisecond)
		var urlAfter string
		chromedp.Run(c.ctx, chromedp.Location(&urlAfter))
		if urlAfter != urlBefore {
			fmt.Fprintf(os.Stderr, "[BROWSER] click navigated: %s → %s\n", urlBefore, urlAfter)
		} else {
			fmt.Fprintf(os.Stderr, "[BROWSER] click done, URL unchanged: %s\n", urlAfter)
		}
		return c.Screenshot()

	case "double_click":
		if err := chromedp.Run(c.ctx,
			chromedp.MouseClickXY(float64(action.X), float64(action.Y), chromedp.ClickCount(2)),
		); err != nil {
			return nil, fmt.Errorf("double_click: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
		return c.Screenshot()

	case "type":
		if err := chromedp.Run(c.ctx, chromedp.KeyEvent(action.Text)); err != nil {
			return nil, fmt.Errorf("type: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
		return c.Screenshot()

	case "key":
		if err := chromedp.Run(c.ctx, chromedp.KeyEvent(action.Key)); err != nil {
			return nil, fmt.Errorf("key: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
		return c.Screenshot()

	case "scroll":
		deltaY := 0
		amount := action.Amount
		if amount == 0 {
			amount = 3
		}
		switch action.Direction {
		case "up":
			deltaY = -100 * amount
		case "down":
			deltaY = 100 * amount
		}
		js := fmt.Sprintf("window.scrollBy(0, %d)", deltaY)
		if err := chromedp.Run(c.ctx, chromedp.Evaluate(js, nil)); err != nil {
			return nil, fmt.Errorf("scroll: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
		return c.Screenshot()

	case "wait":
		dur := action.Duration
		if dur <= 0 {
			dur = 1000 // default 1s
		} else if dur < 100 {
			dur = dur * 1000 // Claude sends seconds, convert to ms
		}
		time.Sleep(time.Duration(dur) * time.Millisecond)
		return c.Screenshot()

	default:
		return nil, fmt.Errorf("unknown action: %s", action.Type)
	}
}

func (c *Computer) Screenshot() ([]byte, error) {
	var buf []byte
	if err := chromedp.Run(c.ctx, chromedp.FullScreenshot(&buf, 60)); err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}

	// Downscale if image is larger than declared display size (e.g. Retina 2x)
	buf, _ = c.scaleToDisplay(buf)

	return buf, nil
}

// scaleToDisplay resizes a screenshot to match the declared DisplaySize.
// Handles Retina/HiDPI where Chrome captures at device pixel ratio.
func (c *Computer) scaleToDisplay(data []byte) ([]byte, error) {
	// Decode to get actual dimensions
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, nil // can't decode, return as-is
	}

	bounds := img.Bounds()
	actualW, actualH := bounds.Dx(), bounds.Dy()
	targetW, targetH := c.display.Width, c.display.Height

	// No scaling needed if already at target size (or smaller)
	if actualW <= targetW && actualH <= targetH {
		return data, nil
	}

	fmt.Fprintf(os.Stderr, "[BROWSER] scaling screenshot %dx%d -> %dx%d\n", actualW, actualH, targetW, targetH)

	// Resize using high-quality interpolation
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)

	// Re-encode
	var out bytes.Buffer
	if format == "png" {
		err = png.Encode(&out, dst)
	} else {
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 60})
	}
	if err != nil {
		return data, nil
	}

	return out.Bytes(), nil
}

func (c *Computer) DisplaySize() computer.DisplaySize { return c.display }

// SessionInfo implementation
func (c *Computer) SessionType() string { return "local" }
func (c *Computer) SessionID() string   { return "" }
func (c *Computer) CurrentURL() string {
	var url string
	_ = chromedp.Run(c.ctx, chromedp.Location(&url))
	return url
}

func (c *Computer) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	if c.allocCancel != nil {
		c.allocCancel()
	}
	return nil
}
