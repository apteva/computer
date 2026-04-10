// Package local implements the Computer interface using a local Chrome/Chromium via CDP.
// It auto-launches Chrome if not already running.
package local

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
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

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(display.Width, display.Height),
		chromedp.Flag("headless", headless),
		chromedp.Flag("disable-gpu", headless),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("no-sandbox", true),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)

	// Verify Chrome launches by running a simple command
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("local chrome: failed to start: %w", err)
	}

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
		if err := chromedp.Run(c.ctx, chromedp.Navigate(action.URL)); err != nil {
			return nil, fmt.Errorf("navigate: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
		return c.Screenshot()

	case "click":
		if err := chromedp.Run(c.ctx,
			chromedp.MouseClickXY(float64(action.X), float64(action.Y)),
		); err != nil {
			return nil, fmt.Errorf("click: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
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
			dur = 1000
		}
		time.Sleep(time.Duration(dur) * time.Millisecond)
		return c.Screenshot()

	default:
		return nil, fmt.Errorf("unknown action: %s", action.Type)
	}
}

func (c *Computer) Screenshot() ([]byte, error) {
	var buf []byte
	if err := chromedp.Run(c.ctx, chromedp.FullScreenshot(&buf, 90)); err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	return buf, nil
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
