// Package browserbase implements the Computer interface using Browserbase sessions via chromedp/CDP.
package browserbase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

type Computer struct {
	apiKey     string
	projectID  string
	sessionID  string
	display    computer.DisplaySize
	ctx        context.Context
	cancel     context.CancelFunc
	allocCancel context.CancelFunc
}

// New creates a Browserbase-backed Computer, starts a session, and connects via CDP.
func New(apiKey, projectID string, display computer.DisplaySize) (*Computer, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("browserbase: api_key is required")
	}
	c := &Computer{
		apiKey:    apiKey,
		projectID: projectID,
		display:   display,
	}

	connectURL, err := c.createSession()
	if err != nil {
		return nil, fmt.Errorf("browserbase: create session: %w", err)
	}

	// Connect via chromedp remote allocator — NoModifyURL prevents chromedp from
	// trying to fetch /json/version on the Browserbase proxy URL
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), connectURL,
		chromedp.NoModifyURL)
	c.allocCancel = allocCancel

	ctx, cancel := chromedp.NewContext(allocCtx)
	c.ctx = ctx
	c.cancel = cancel

	// Verify connection by running a simple command
	if err := chromedp.Run(ctx); err != nil {
		allocCancel()
		cancel()
		return nil, fmt.Errorf("browserbase: connect: %w", err)
	}

	return c, nil
}

func (c *Computer) createSession() (string, error) {
	body := fmt.Sprintf(`{"projectId":"%s","browserSettings":{"viewport":{"width":%d,"height":%d}}}`,
		c.projectID, c.display.Width, c.display.Height)

	req, err := http.NewRequest("POST", "https://api.browserbase.com/v1/sessions",
		strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bb-api-key", c.apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID         string `json:"id"`
		ConnectURL string `json:"connectUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	c.sessionID = result.ID

	if result.ConnectURL == "" {
		return "", fmt.Errorf("no connectUrl in session response")
	}
	return result.ConnectURL, nil
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
		if err := c.dispatchClick(action.X, action.Y, 1); err != nil {
			return nil, fmt.Errorf("click: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
		return c.Screenshot()

	case "double_click":
		if err := c.dispatchClick(action.X, action.Y, 2); err != nil {
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

func (c *Computer) dispatchClick(x, y, clickCount int) error {
	return chromedp.Run(c.ctx,
		chromedp.MouseClickXY(float64(x), float64(y), chromedp.ClickCount(clickCount)),
	)
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
func (c *Computer) SessionType() string { return "browserbase" }
func (c *Computer) SessionID() string   { return c.sessionID }
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

	// Close Browserbase session via API
	if c.sessionID != "" {
		url := fmt.Sprintf("https://api.browserbase.com/v1/sessions/%s", c.sessionID)
		req, _ := http.NewRequest("DELETE", url, nil)
		req.Header.Set("x-bb-api-key", c.apiKey)
		client := &http.Client{Timeout: 10 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
		c.sessionID = ""
	}
	return nil
}
