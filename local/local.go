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
	"sync"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"golang.org/x/image/draw"
)

// netFailure is one recent network-level failure surfaced by Chrome.
// Captured via Network.loadingFailed so callers can see the real
// errorText (e.g. "net::ERR_CONNECTION_RESET") instead of a generic
// navigate error.
type netFailure struct {
	URL       string
	ErrorText string
	Canceled  bool
	At        time.Time
}

type Computer struct {
	display     computer.DisplaySize
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc

	// recent network failures captured from CDP, most recent last.
	failMu     sync.Mutex
	failures   []netFailure
	requestURL map[network.RequestID]string // requestID -> URL for loadingFailed lookup
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
	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[BROWSER][cdp] "+format+"\n", args...)
	}))

	c := &Computer{
		display:     display,
		ctx:         ctx,
		cancel:      cancel,
		allocCancel: allocCancel,
		requestURL:  make(map[network.RequestID]string),
	}

	// Verify Chrome launches by running a simple command
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("local chrome: failed to start: %w (goos=%s)", err, runtime.GOOS)
	}

	// Enable Network + Page domains so we can observe real failure
	// reasons (errorText) and lifecycle events.
	if err := chromedp.Run(ctx,
		network.Enable(),
		page.Enable(),
	); err != nil {
		fmt.Fprintf(os.Stderr, "[BROWSER] warn: failed to enable Network/Page domains: %v\n", err)
	}

	// Listen for CDP events. This is where the real error from a
	// navigation shows up on Windows — ERR_CONNECTION_RESET comes
	// through Network.loadingFailed.errorText, not from chromedp.Run.
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			c.failMu.Lock()
			c.requestURL[e.RequestID] = e.Request.URL
			c.failMu.Unlock()
		case *network.EventLoadingFailed:
			c.failMu.Lock()
			url := c.requestURL[e.RequestID]
			delete(c.requestURL, e.RequestID)
			f := netFailure{URL: url, ErrorText: e.ErrorText, Canceled: bool(e.Canceled), At: time.Now()}
			c.failures = append(c.failures, f)
			if len(c.failures) > 32 {
				c.failures = c.failures[len(c.failures)-32:]
			}
			c.failMu.Unlock()
			fmt.Fprintf(os.Stderr, "[BROWSER][cdp] loadingFailed url=%s err=%s canceled=%v type=%s blocked=%v\n",
				url, e.ErrorText, e.Canceled, e.Type, e.BlockedReason)
		case *network.EventResponseReceived:
			if e.Response != nil && e.Response.Status >= 400 {
				fmt.Fprintf(os.Stderr, "[BROWSER][cdp] response status=%d url=%s\n", e.Response.Status, e.Response.URL)
			}
		case *page.EventFrameNavigated:
			if e.Frame != nil && e.Frame.ParentID == "" {
				fmt.Fprintf(os.Stderr, "[BROWSER][cdp] frameNavigated url=%s\n", e.Frame.URL)
			}
		}
	})

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

	// User agent + process identity can affect sandbox / network-service
	// behavior on Windows. Log both so issues like "running as Admin"
	// (which Chrome refuses to sandbox) are visible.
	var ua string
	chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &ua))

	// Probe the network from inside Chrome with a data: URL (no network)
	// and a real URL to disambiguate "Chrome network stack broken" from
	// "route to internet broken". Result logs next to each other so the
	// user can tell at a glance whether the reset is Windows-wide or
	// only affects real HTTPS.
	var dataOK, netOK bool
	dataCtx, dataCancel := context.WithTimeout(ctx, 3*time.Second)
	chromedp.Run(dataCtx, chromedp.Navigate("data:text/plain,ok"))
	chromedp.Run(dataCtx, chromedp.Evaluate(`document.body && document.body.innerText==="ok"`, &dataOK))
	dataCancel()

	netCtx, netCancel := context.WithTimeout(ctx, 6*time.Second)
	netErr := chromedp.Run(netCtx, chromedp.Evaluate(`
		fetch('https://www.gstatic.com/generate_204', {cache:'no-store'})
		  .then(r => r.status === 204)
		  .catch(() => false)
	`, &netOK))
	netCancel()

	fmt.Fprintf(os.Stderr, "[BROWSER] pid=%d uid=%d ua=%q probe_data=%v probe_net=%v probe_err=%v\n",
		os.Getpid(), os.Getuid(), ua, dataOK, netOK, netErr)

	return c, nil
}

func firstFailText(fs []netFailure) string {
	if len(fs) == 0 {
		return ""
	}
	return fs[0].ErrorText
}

// recentFailures returns failures observed since t (inclusive).
func (c *Computer) recentFailures(since time.Time) []netFailure {
	c.failMu.Lock()
	defer c.failMu.Unlock()
	out := make([]netFailure, 0, len(c.failures))
	for _, f := range c.failures {
		if !f.At.Before(since) {
			out = append(out, f)
		}
	}
	return out
}

func (c *Computer) Execute(action computer.Action) ([]byte, error) {
	switch action.Type {
	case "screenshot":
		return c.Screenshot()

	case "navigate":
		started := time.Now()
		fmt.Fprintf(os.Stderr, "[BROWSER] navigate to %s\n", action.URL)
		runErr := chromedp.Run(c.ctx, chromedp.Navigate(action.URL))
		time.Sleep(500 * time.Millisecond)
		var url, title, readyState string
		chromedp.Run(c.ctx, chromedp.Location(&url))
		chromedp.Run(c.ctx, chromedp.Title(&title))
		chromedp.Run(c.ctx, chromedp.Evaluate(`document.readyState`, &readyState))
		fails := c.recentFailures(started)
		fmt.Fprintf(os.Stderr, "[BROWSER] navigate done: URL=%s title=%q readyState=%s cdpErr=%v netFailures=%d\n",
			url, title, readyState, runErr, len(fails))
		for _, f := range fails {
			fmt.Fprintf(os.Stderr, "[BROWSER]   failure: %s → %s (canceled=%v)\n", f.URL, f.ErrorText, f.Canceled)
		}
		if runErr != nil {
			return nil, fmt.Errorf("navigate: %w (netFailures=%d first=%s)", runErr, len(fails), firstFailText(fails))
		}
		// Chrome reports CONNECTION_RESET via CDP even when chromedp.Navigate
		// returns nil. If the main-document request failed and we're still
		// on about:blank, surface that as an error instead of silently
		// returning a blank screenshot.
		if (url == "" || url == "about:blank") && len(fails) > 0 {
			return nil, fmt.Errorf("navigate reached no page: %s (see [BROWSER] logs)", firstFailText(fails))
		}
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
