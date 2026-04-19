// Package browserbase implements the Computer interface using Browserbase sessions via chromedp/CDP.
package browserbase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// apiBase is the Browserbase REST API root. Override for staging via env.
const apiBase = "https://api.browserbase.com/v1"

// Options extends what New accepts beyond apiKey/projectID/display. All
// fields are optional. They correspond 1:1 to the POST /v1/sessions payload
// documented at https://docs.browserbase.com/reference/api/create-a-session.
type Options struct {
	// KeepAlive keeps the session alive after the CDP client disconnects
	// (paid plans only). Default false.
	KeepAlive bool `json:"keepAlive,omitempty"`

	// Region pins the session to a Browserbase region:
	// "us-west-2", "us-east-1", "eu-central-1", "ap-southeast-1".
	// Default: Browserbase picks nearest.
	Region string `json:"region,omitempty"`

	// Timeout is the max session duration in seconds. Default and max
	// depend on plan (Free: 3600).
	Timeout int `json:"timeout,omitempty"`

	// Proxies: true enables Browserbase's managed residential proxy, or
	// pass a raw list for custom proxies. Encoded as-is into the request.
	Proxies any `json:"proxies,omitempty"`

	// Fingerprint settings (device, locales, OS, screen). Opaque — passed through.
	Fingerprint map[string]any `json:"fingerprint,omitempty"`

	// ExtensionID attaches a previously uploaded Chrome extension to the session.
	ExtensionID string `json:"extensionId,omitempty"`

	// SolveCaptchas enables Browserbase's managed CAPTCHA solver.
	SolveCaptchas bool `json:"solveCaptchas,omitempty"`

	// UserMetadata is attached to the session record for later querying.
	UserMetadata map[string]any `json:"userMetadata,omitempty"`
}

type Computer struct {
	apiKey      string
	projectID   string
	sessionID   string
	debugURL    string
	display     computer.DisplaySize
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	http        *http.Client
}

// New creates a Browserbase-backed Computer, starts a session, and connects via CDP.
func New(apiKey, projectID string, display computer.DisplaySize) (*Computer, error) {
	return NewWithOptions(apiKey, projectID, display, Options{})
}

// NewWithOptions is New plus the extended configuration in Options.
func NewWithOptions(apiKey, projectID string, display computer.DisplaySize, opts Options) (*Computer, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("browserbase: api_key is required")
	}
	if projectID == "" {
		// The API requires projectId. Fail fast with a clear message
		// instead of surfacing a confusing HTTP 400 from the server.
		return nil, fmt.Errorf("browserbase: project_id is required")
	}

	c := &Computer{
		apiKey:    apiKey,
		projectID: projectID,
		display:   display,
		http:      &http.Client{Timeout: 30 * time.Second},
	}

	connectURL, err := c.createSession(opts)
	if err != nil {
		return nil, fmt.Errorf("browserbase: create session: %w", err)
	}

	// Connect via chromedp remote allocator. NoModifyURL stops chromedp
	// from probing /json/version on the Browserbase proxy (it doesn't
	// serve that endpoint).
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

	// Best-effort fetch the live debugger URL. Useful for the TUI /
	// dashboard "watch live" link. Missing or failing shouldn't abort
	// the session — the browser still works, we just can't observe it.
	if dbg, err := c.fetchDebugURL(); err == nil {
		c.debugURL = dbg
	} else {
		fmt.Fprintf(os.Stderr, "[BROWSERBASE] debug URL unavailable: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "[BROWSERBASE] session ready id=%s debug=%s display=%dx%d\n",
		c.sessionID, c.debugURL, display.Width, display.Height)

	return c, nil
}

// sessionCreateRequest is the POST /v1/sessions payload. Fields match
// Browserbase's documented schema; omitempty means unset values don't
// override Browserbase's server-side defaults.
type sessionCreateRequest struct {
	ProjectID       string                 `json:"projectId"`
	BrowserSettings map[string]any         `json:"browserSettings,omitempty"`
	KeepAlive       bool                   `json:"keepAlive,omitempty"`
	Region          string                 `json:"region,omitempty"`
	Timeout         int                    `json:"timeout,omitempty"`
	Proxies         any                    `json:"proxies,omitempty"`
	ExtensionID     string                 `json:"extensionId,omitempty"`
	UserMetadata    map[string]any         `json:"userMetadata,omitempty"`
}

type sessionCreateResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	ConnectURL string `json:"connectUrl"`
	// The API also returns projectId, createdAt, region, etc.; we only
	// read the fields we use.
}

func (c *Computer) createSession(opts Options) (string, error) {
	bs := map[string]any{
		"viewport": map[string]int{
			"width":  c.display.Width,
			"height": c.display.Height,
		},
	}
	if opts.Fingerprint != nil {
		bs["fingerprint"] = opts.Fingerprint
	}
	if opts.SolveCaptchas {
		bs["solveCaptchas"] = true
	}

	req := sessionCreateRequest{
		ProjectID:       c.projectID,
		BrowserSettings: bs,
		KeepAlive:       opts.KeepAlive,
		Region:          opts.Region,
		Timeout:         opts.Timeout,
		Proxies:         opts.Proxies,
		ExtensionID:     opts.ExtensionID,
		UserMetadata:    opts.UserMetadata,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", apiBase+"/sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-BB-API-Key", c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result sessionCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	c.sessionID = result.ID

	// Browserbase returns status=RUNNING on success. Anything else means
	// the session failed to start (quota exceeded, invalid region, …).
	if result.Status != "" && result.Status != "RUNNING" {
		return "", fmt.Errorf("session started with status=%q (expected RUNNING)", result.Status)
	}
	if result.ConnectURL == "" {
		return "", fmt.Errorf("no connectUrl in session response (id=%s status=%s)", result.ID, result.Status)
	}
	return result.ConnectURL, nil
}

// fetchDebugURL calls GET /v1/sessions/{id}/debug and returns the
// fullscreen debugger URL. Empty string if the session doesn't expose one.
func (c *Computer) fetchDebugURL() (string, error) {
	if c.sessionID == "" {
		return "", fmt.Errorf("no session id")
	}
	url := fmt.Sprintf("%s/sessions/%s/debug", apiBase, c.sessionID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-BB-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		DebuggerFullscreenURL string `json:"debuggerFullscreenUrl"`
		DebuggerURL           string `json:"debuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.DebuggerFullscreenURL != "" {
		return result.DebuggerFullscreenURL, nil
	}
	return result.DebuggerURL, nil
}

// requestRelease ends the session via the official Browserbase lifecycle:
// POST /v1/sessions/{id} with status=REQUEST_RELEASE. The previous
// implementation used DELETE /v1/sessions/{id}, which the API does not
// expose — so sessions leaked until their idle timeout.
func (c *Computer) requestRelease() error {
	if c.sessionID == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"projectId": c.projectID,
		"status":    "REQUEST_RELEASE",
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/sessions/%s", apiBase, c.sessionID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BB-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
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
		if err := c.scroll(action); err != nil {
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

// scroll dispatches a real CDP mouseWheel event at (x, y). Browserbase
// pages frequently have nested scroll containers (SaaS dashboards,
// SPAs) where `window.scrollBy` is a no-op; wheel events scroll the
// element under the cursor and fire wheel handlers like a human.
func (c *Computer) scroll(a computer.Action) error {
	amount := a.Amount
	if amount <= 0 {
		amount = 3
	}
	const step = 100
	var dx, dy float64
	switch strings.ToLower(a.Direction) {
	case "up":
		dy = float64(-step * amount)
	case "down":
		dy = float64(step * amount)
	case "left":
		dx = float64(-step * amount)
	case "right":
		dx = float64(step * amount)
	default:
		return fmt.Errorf("unknown scroll direction %q", a.Direction)
	}

	x, y := float64(a.X), float64(a.Y)
	if x == 0 && y == 0 {
		x = float64(c.display.Width) / 2
		y = float64(c.display.Height) / 2
	}

	err := chromedp.Run(c.ctx,
		input.DispatchMouseEvent(input.MouseWheel, x, y).
			WithDeltaX(dx).WithDeltaY(dy),
	)
	if err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "[BROWSERBASE] wheel dispatch failed (%v), falling back to window.scrollBy\n", err)
	js := fmt.Sprintf("window.scrollBy(%d, %d)", int(dx), int(dy))
	return chromedp.Run(c.ctx, chromedp.Evaluate(js, nil))
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

// DebugURL returns the Browserbase live-view URL for this session, or ""
// if not available. Callers can type-assert against this method to expose
// a "watch live" link in UIs without widening the core Computer interface.
func (c *Computer) DebugURL() string { return c.debugURL }

func (c *Computer) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	if c.allocCancel != nil {
		c.allocCancel()
	}

	// Officially release the session so Browserbase stops billing minutes.
	if err := c.requestRelease(); err != nil {
		fmt.Fprintf(os.Stderr, "[BROWSERBASE] release failed id=%s: %v\n", c.sessionID, err)
	} else if c.sessionID != "" {
		fmt.Fprintf(os.Stderr, "[BROWSERBASE] session released id=%s\n", c.sessionID)
	}
	c.sessionID = ""
	return nil
}
