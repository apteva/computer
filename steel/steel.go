// Package steel implements the Computer interface using Steel.dev sessions via chromedp/CDP.
package steel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apteva/computer/som"
	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// apiBase is the Steel REST API root.
const apiBase = "https://api.steel.dev/v1"

// Options extends what New accepts beyond apiKey/display. All fields are
// optional. Field names match Steel's POST /v1/sessions payload
// (https://docs.steel.dev/api-reference) so omitempty leaves Steel's
// server-side defaults in place.
type Options struct {
	// BlockAds enables Steel's built-in ad blocker.
	BlockAds bool `json:"blockAds,omitempty"`

	// ProxyURL pins the session to a specific upstream proxy. Mutually
	// exclusive with UseProxy (managed residential proxy).
	ProxyURL string `json:"proxyUrl,omitempty"`

	// UseProxy enables Steel's managed residential proxy.
	UseProxy bool `json:"useProxy,omitempty"`

	// Region pins the session to a Steel region (e.g. "lax1", "iad1").
	// Default: Steel picks nearest.
	Region string `json:"region,omitempty"`

	// Timeout is the max session duration in milliseconds. Default and
	// max depend on plan.
	Timeout int `json:"timeout,omitempty"`

	// SolveCaptcha enables Steel's managed CAPTCHA solver.
	SolveCaptcha bool `json:"solveCaptcha,omitempty"`

	// UserAgent overrides the browser's default user agent.
	UserAgent string `json:"userAgent,omitempty"`

	// SessionContext lets the caller seed cookies/localStorage as an
	// opaque inline blob (one-shot, not persisted across sessions).
	// Forwarded as-is. Use OpenOptions.ContextID for the persistent
	// cross-session identity that Steel calls a "profile" — see
	// OpenSession below. This field is here only as an escape hatch
	// for callers that already have a snapshot blob from Steel's
	// contexts endpoint.
	SessionContext map[string]any `json:"sessionContext,omitempty"`
}

type Computer struct {
	apiKey      string
	opts        Options
	sessionID   string
	contextID   string
	viewerURL   string
	display     computer.DisplaySize
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	http        *http.Client

	// SoM: same wiring as local.Computer / browserbase.Computer.
	labelMu    sync.RWMutex
	lastLabels map[int]som.Element
}

// New constructs a Steel-backed Computer. NO session is created yet —
// the agent picks the binding (anonymous or context/profile) at the
// first browser_session.open call. Steel does not support attaching
// to an existing session id.
func New(apiKey string, display computer.DisplaySize) (*Computer, error) {
	return NewWithOptions(apiKey, display, Options{})
}

// NewWithOptions stores provider-level configuration for use at
// session-create time. Like New, it does NOT create a session.
func NewWithOptions(apiKey string, display computer.DisplaySize, opts Options) (*Computer, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("steel: api_key is required")
	}
	return &Computer{
		apiKey:  apiKey,
		opts:    opts,
		display: display,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// sessionCreateRequest is the POST /v1/sessions payload. camelCase to
// match Steel's schema.
type sessionCreateRequest struct {
	Dimensions     map[string]int `json:"dimensions,omitempty"`
	BlockAds       bool           `json:"blockAds,omitempty"`
	ProxyURL       string         `json:"proxyUrl,omitempty"`
	UseProxy       bool           `json:"useProxy,omitempty"`
	Region         string         `json:"region,omitempty"`
	Timeout        int            `json:"timeout,omitempty"`
	SolveCaptcha   bool           `json:"solveCaptcha,omitempty"`
	UserAgent      string         `json:"userAgent,omitempty"`
	SessionContext map[string]any `json:"sessionContext,omitempty"`
	ProfileID      string         `json:"profileId,omitempty"`
	PersistProfile bool           `json:"persistProfile,omitempty"`
}

type sessionCreateResponse struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	WebsocketURL     string `json:"websocketUrl"`
	SessionViewerURL string `json:"sessionViewerUrl"`
	DebugURL         string `json:"debugUrl"`
}

func (c *Computer) createSession(o computer.OpenOptions) (string, error) {
	timeout := c.opts.Timeout
	if o.Timeout > 0 {
		// Steel's API takes ms; OpenOptions.Timeout is seconds.
		timeout = o.Timeout * 1000
	}
	// Agent's per-call OpenOptions.Proxy wins over the harness default.
	// ProxyCountry is not honored — Steel's useProxy boolean doesn't
	// take a country (custom routing requires the ProxyURL escape
	// hatch in Options).
	useProxy := c.opts.UseProxy
	if o.Proxy != nil {
		useProxy = *o.Proxy
	}
	req := sessionCreateRequest{
		Dimensions: map[string]int{
			"width":  c.display.Width,
			"height": c.display.Height,
		},
		BlockAds:       c.opts.BlockAds,
		ProxyURL:       c.opts.ProxyURL,
		UseProxy:       useProxy,
		Region:         c.opts.Region,
		Timeout:        timeout,
		SolveCaptcha:   c.opts.SolveCaptcha,
		UserAgent:      c.opts.UserAgent,
		SessionContext: c.opts.SessionContext,
	}
	if o.ContextID != "" {
		req.ProfileID = o.ContextID
		req.PersistProfile = o.Persist
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
	httpReq.Header.Set("Steel-Api-Key", c.apiKey)

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
	if result.SessionViewerURL != "" {
		c.viewerURL = result.SessionViewerURL
	} else {
		c.viewerURL = result.DebugURL
	}

	if result.WebsocketURL == "" {
		return "", fmt.Errorf("no websocketUrl in session response (id=%s status=%s)", result.ID, result.Status)
	}
	// Steel's CDP endpoint requires the API key as a query parameter.
	sep := "?"
	if strings.Contains(result.WebsocketURL, "?") {
		sep = "&"
	}
	return result.WebsocketURL + sep + "apiKey=" + c.apiKey, nil
}

// requestRelease ends the session via POST /v1/sessions/{id}/release.
func (c *Computer) requestRelease() error {
	if c.sessionID == "" {
		return nil
	}
	url := fmt.Sprintf("%s/sessions/%s/release", apiBase, c.sessionID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Steel-Api-Key", c.apiKey)

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

// OpenSession establishes a session matching opts and (if URL set) navigates.
// Steel maps ContextID/Persist → profileId/persistProfile. SessionID-based
// attach is unsupported (Steel offers no session reconnection — each
// resume is a fresh session bound to the same profile).
func (c *Computer) OpenSession(o computer.OpenOptions) error {
	if o.SessionID != "" {
		return fmt.Errorf("steel: SessionID-based attach is not supported (open with the same context_id to reuse the profile)")
	}
	// Fast path: same context already attached, just navigate.
	if c.sessionID != "" && o.ContextID != "" && o.ContextID == c.contextID {
		if o.URL != "" {
			return c.navigate(o.URL)
		}
		return nil
	}
	if c.sessionID != "" {
		c.releaseCDP()
		c.sessionID = ""
		c.contextID = ""
		c.viewerURL = ""
	}
	connectURL, err := c.createSession(o)
	if err != nil {
		return fmt.Errorf("steel: create session: %w", err)
	}
	if o.ContextID != "" {
		c.contextID = o.ContextID
	}
	if err := c.establishCDP(connectURL); err != nil {
		return fmt.Errorf("steel: connect: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[STEEL] session ready id=%s context=%s viewer=%s display=%dx%d\n",
		c.sessionID, c.contextID, c.viewerURL, c.display.Width, c.display.Height)
	if o.URL != "" {
		return c.navigate(o.URL)
	}
	return nil
}

func (c *Computer) establishCDP(connectURL string) error {
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), connectURL,
		chromedp.NoModifyURL)
	ctx, cancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		allocCancel()
		return err
	}
	c.allocCancel = allocCancel
	c.ctx = ctx
	c.cancel = cancel
	return nil
}

func (c *Computer) releaseCDP() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.allocCancel != nil {
		c.allocCancel()
		c.allocCancel = nil
	}
	c.ctx = nil
}

func (c *Computer) navigate(url string) error {
	if c.ctx == nil {
		return fmt.Errorf("steel: no active session — cannot navigate")
	}
	_, err := c.Execute(computer.Action{Type: "navigate", URL: url})
	return err
}

func (c *Computer) Execute(action computer.Action) ([]byte, error) {
	if c.ctx == nil {
		return nil, fmt.Errorf("steel: no active session — call browser_session open first")
	}
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
		x, y := action.X, action.Y
		if action.Label != 0 {
			if e, ok := c.resolveLabel(action.Label); ok {
				x, y = e.Center()
			}
		}
		if err := c.dispatchClick(x, y, 1); err != nil {
			return nil, fmt.Errorf("click: %w", err)
		}
		// Explicit focus at the click point — same rationale as the
		// local and browserbase packages.
		focusJS := fmt.Sprintf(`(function(){
			var el = document.elementFromPoint(%d, %d);
			if (el && typeof el.focus === 'function') { el.focus(); return el.tagName; }
			return null;
		})()`, x, y)
		var focusedTag string
		_ = chromedp.Run(c.ctx, chromedp.Evaluate(focusJS, &focusedTag))
		if focusedTag != "" {
			fmt.Fprintf(os.Stderr, "[STEEL] click focused <%s>\n", strings.ToLower(focusedTag))
		}
		time.Sleep(200 * time.Millisecond)
		return c.Screenshot()

	case "double_click":
		x, y := action.X, action.Y
		if action.Label != 0 {
			if e, ok := c.resolveLabel(action.Label); ok {
				x, y = e.Center()
			}
		}
		if err := c.dispatchClick(x, y, 2); err != nil {
			return nil, fmt.Errorf("double_click: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
		return c.Screenshot()

	case "type":
		err := chromedp.Run(c.ctx, input.InsertText(action.Text))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[STEEL] insertText failed (%v), falling back to KeyEvent\n", err)
			if err := chromedp.Run(c.ctx, chromedp.KeyEvent(action.Text)); err != nil {
				return nil, fmt.Errorf("type: %w", err)
			}
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
	fmt.Fprintf(os.Stderr, "[STEEL] wheel dispatch failed (%v), falling back to window.scrollBy\n", err)
	js := fmt.Sprintf("window.scrollBy(%d, %d)", int(dx), int(dy))
	return chromedp.Run(c.ctx, chromedp.Evaluate(js, nil))
}

func (c *Computer) dispatchClick(x, y, clickCount int) error {
	return chromedp.Run(c.ctx,
		chromedp.MouseClickXY(float64(x), float64(y), chromedp.ClickCount(clickCount)),
	)
}

func (c *Computer) Screenshot() ([]byte, error) {
	if c.ctx == nil {
		return nil, fmt.Errorf("steel: no active session — call browser_session open first")
	}
	var buf []byte
	err := chromedp.Run(c.ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			b, err := page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatJpeg).
				WithQuality(90).
				Do(ctx)
			if err != nil {
				return err
			}
			buf = b
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}

	if som.Enabled() {
		var raw json.RawMessage
		if err := chromedp.Run(c.ctx, chromedp.Evaluate(som.EnumScript, &raw)); err != nil {
			fmt.Fprintf(os.Stderr, "[STEEL] som enum failed: %v\n", err)
			return buf, nil
		}
		elements, err := som.UnmarshalElements(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[STEEL] som parse failed: %v\n", err)
			return buf, nil
		}
		m := make(map[int]som.Element, len(elements))
		for _, e := range elements {
			m[e.Label] = e
		}
		c.labelMu.Lock()
		c.lastLabels = m
		c.labelMu.Unlock()

		annotated, aerr := som.Annotate(buf, elements)
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "[STEEL] som annotate failed: %v\n", aerr)
			return buf, nil
		}
		fmt.Fprintf(os.Stderr, "[STEEL] som annotated: %d elements\n", len(elements))
		return annotated, nil
	}
	return buf, nil
}

func (c *Computer) resolveLabel(label int) (som.Element, bool) {
	c.labelMu.RLock()
	defer c.labelMu.RUnlock()
	if c.lastLabels == nil {
		return som.Element{}, false
	}
	e, ok := c.lastLabels[label]
	return e, ok
}

func (c *Computer) DisplaySize() computer.DisplaySize { return c.display }

func (c *Computer) SessionType() string { return "steel" }
func (c *Computer) SessionID() string   { return c.sessionID }
func (c *Computer) ContextID() string   { return c.contextID }
func (c *Computer) CurrentURL() string {
	var url string
	_ = chromedp.Run(c.ctx, chromedp.Location(&url))
	return url
}

// DebugURL returns the Steel session viewer URL, or "" if unavailable.
func (c *Computer) DebugURL() string { return c.viewerURL }

func (c *Computer) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	if c.allocCancel != nil {
		c.allocCancel()
	}

	if err := c.requestRelease(); err != nil {
		fmt.Fprintf(os.Stderr, "[STEEL] release failed id=%s: %v\n", c.sessionID, err)
	} else if c.sessionID != "" {
		fmt.Fprintf(os.Stderr, "[STEEL] session released id=%s\n", c.sessionID)
	}
	c.sessionID = ""
	return nil
}
