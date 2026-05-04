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
	"sync"
	"time"

	"github.com/apteva/computer/som"
	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
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
	opts        Options
	sessionID   string
	contextID   string
	debugURL    string
	display     computer.DisplaySize
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	http        *http.Client

	// SoM: same wiring as local.Computer. See local.go for rationale.
	labelMu    sync.RWMutex
	lastLabels map[int]som.Element
}

// New constructs a Browserbase-backed Computer. NO session is created
// yet — the agent picks the binding (anonymous, context, or attach to
// session id) via the first browser_session.open call, which routes
// through OpenSession.
func New(apiKey, projectID string, display computer.DisplaySize) (*Computer, error) {
	return NewWithOptions(apiKey, projectID, display, Options{})
}

// NewWithOptions stores provider-level configuration (Region, Timeout,
// KeepAlive, Fingerprint, …) for use at session-create time. Like New,
// it does NOT create a session — that's deferred to OpenSession.
func NewWithOptions(apiKey, projectID string, display computer.DisplaySize, opts Options) (*Computer, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("browserbase: api_key is required")
	}
	if projectID == "" {
		// The API requires projectId. Fail fast with a clear message
		// instead of surfacing a confusing HTTP 400 from the server.
		return nil, fmt.Errorf("browserbase: project_id is required")
	}
	return &Computer{
		apiKey:    apiKey,
		projectID: projectID,
		opts:      opts,
		display:   display,
		http:      &http.Client{Timeout: 30 * time.Second},
	}, nil
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

func (c *Computer) createSession(o computer.OpenOptions) (string, error) {
	bs := map[string]any{
		"viewport": map[string]int{
			"width":  c.display.Width,
			"height": c.display.Height,
		},
	}
	if c.opts.Fingerprint != nil {
		bs["fingerprint"] = c.opts.Fingerprint
	}
	if c.opts.SolveCaptchas {
		bs["solveCaptchas"] = true
	}
	if o.ContextID != "" {
		bs["context"] = map[string]any{
			"id":      o.ContextID,
			"persist": o.Persist,
		}
	}

	timeout := c.opts.Timeout
	if o.Timeout > 0 {
		timeout = o.Timeout
	}
	// Agent's per-call OpenOptions.Proxy wins over the harness default.
	// Browserbase encodes "true" as the managed residential proxy; a
	// custom proxy list can also be passed via Options.Proxies (kept
	// in c.opts.Proxies). ProxyCountry is not honored here — it
	// requires a custom proxy list, not the boolean flag.
	proxies := c.opts.Proxies
	if o.Proxy != nil {
		if *o.Proxy {
			proxies = true
		} else {
			proxies = nil
		}
	}
	req := sessionCreateRequest{
		ProjectID:       c.projectID,
		BrowserSettings: bs,
		KeepAlive:       c.opts.KeepAlive,
		Region:          c.opts.Region,
		Timeout:         timeout,
		Proxies:         proxies,
		ExtensionID:     c.opts.ExtensionID,
		UserMetadata:    c.opts.UserMetadata,
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
	if c.ctx == nil {
		return nil, fmt.Errorf("browserbase: no active session — call browser_session open first")
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
		// SoM: label=N takes precedence over x,y.
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
		// local package: CDP mouse events don't reliably move DOM
		// focus to form inputs, so later type/insertText can no-op
		// silently. Best-effort; errors ignored so plain clicks on
		// non-focusable elements still succeed.
		focusJS := fmt.Sprintf(`(function(){
			var el = document.elementFromPoint(%d, %d);
			if (el && typeof el.focus === 'function') { el.focus(); return el.tagName; }
			return null;
		})()`, x, y)
		var focusedTag string
		_ = chromedp.Run(c.ctx, chromedp.Evaluate(focusJS, &focusedTag))
		if focusedTag != "" {
			fmt.Fprintf(os.Stderr, "[BROWSERBASE] click focused <%s>\n", strings.ToLower(focusedTag))
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
		// Same rationale as the local package: Input.insertText is
		// the reliable modern path for text entry (paste-like, fires
		// real input/change events, forgives imprecise focus).
		// KeyEvent fallback for cases with no editable focus.
		err := chromedp.Run(c.ctx, input.InsertText(action.Text))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSERBASE] insertText failed (%v), falling back to KeyEvent\n", err)
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
	if c.ctx == nil {
		return nil, fmt.Errorf("browserbase: no active session — call browser_session open first")
	}
	// Viewport-only screenshot. See local.go for the full rationale:
	// FullScreenshot returns the entire scrollable page, which then
	// either gets aspect-squashed to viewport (silently distorting
	// coordinates) or sent to the LLM at page dimensions that don't
	// match the viewport the agent clicks into. page.CaptureScreenshot
	// returns exactly the visible area at the configured resolution.
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

	// SoM annotation — same pipeline as local.Screenshot. Off unless
	// APTEVA_SOM=1; any failure returns the raw screenshot.
	if som.Enabled() {
		var raw json.RawMessage
		if err := chromedp.Run(c.ctx, chromedp.Evaluate(som.EnumScript, &raw)); err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSERBASE] som enum failed: %v\n", err)
			return buf, nil
		}
		elements, err := som.UnmarshalElements(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSERBASE] som parse failed: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "[BROWSERBASE] som annotate failed: %v\n", aerr)
			return buf, nil
		}
		fmt.Fprintf(os.Stderr, "[BROWSERBASE] som annotated: %d elements\n", len(elements))
		return annotated, nil
	}
	return buf, nil
}

// resolveLabel mirrors local.resolveLabel.
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

// SessionInfo implementation
func (c *Computer) SessionType() string { return "browserbase" }
func (c *Computer) SessionID() string   { return c.sessionID }
func (c *Computer) ContextID() string   { return c.contextID }
func (c *Computer) CurrentURL() string {
	var url string
	_ = chromedp.Run(c.ctx, chromedp.Location(&url))
	return url
}

// DebugURL returns the Browserbase live-view URL for this session, or ""
// if not available. Callers can type-assert against this method to expose
// a "watch live" link in UIs without widening the core Computer interface.
func (c *Computer) DebugURL() string { return c.debugURL }

// OpenSession establishes a session matching opts and (if a URL is
// given) navigates to it. Implements computer.SessionOpener — this is
// the agent-runtime entry point for create-with-context, attach-by-id,
// and rebind-to-different-context. Idempotent when opts match the
// current binding (no-op or just navigate). Otherwise tears down the
// current session before establishing the new one.
func (c *Computer) OpenSession(o computer.OpenOptions) error {
	if o.SessionID != "" && o.ContextID != "" {
		return fmt.Errorf("browserbase: SessionID and ContextID are mutually exclusive")
	}
	// Fast path: caller wants the same session/context we already have.
	if c.sessionID != "" {
		sameSession := o.SessionID != "" && o.SessionID == c.sessionID
		sameContext := o.SessionID == "" && o.ContextID != "" && o.ContextID == c.contextID
		if sameSession || sameContext {
			if o.URL != "" {
				return c.navigate(o.URL)
			}
			return nil
		}
		// Different binding — drop the current connection. The previous
		// Browserbase session is left to time out (or release on Close).
		c.releaseCDP()
		c.sessionID = ""
		c.contextID = ""
		c.debugURL = ""
	}

	var connectURL string
	if o.SessionID != "" {
		u, err := c.fetchSessionConnectURL(o.SessionID)
		if err != nil {
			return fmt.Errorf("browserbase: lookup session %s: %w", o.SessionID, err)
		}
		connectURL = u
		c.sessionID = o.SessionID
	} else {
		u, err := c.createSession(o)
		if err != nil {
			return fmt.Errorf("browserbase: create session: %w", err)
		}
		connectURL = u
		// c.sessionID is set inside createSession.
		if o.ContextID != "" {
			c.contextID = o.ContextID
		}
	}
	if err := c.establishCDP(connectURL); err != nil {
		return fmt.Errorf("browserbase: connect: %w", err)
	}
	if dbg, derr := c.fetchDebugURL(); derr == nil {
		c.debugURL = dbg
	} else {
		fmt.Fprintf(os.Stderr, "[BROWSERBASE] debug URL unavailable: %v\n", derr)
	}
	fmt.Fprintf(os.Stderr, "[BROWSERBASE] session ready id=%s context=%s debug=%s display=%dx%d\n",
		c.sessionID, c.contextID, c.debugURL, c.display.Width, c.display.Height)
	if o.URL != "" {
		return c.navigate(o.URL)
	}
	return nil
}

// Resume is a thin wrapper around OpenSession for callers that hold the
// older Resumable interface. Same code path as OpenSession({SessionID}).
func (c *Computer) Resume(sessionID string) error {
	return c.OpenSession(computer.OpenOptions{SessionID: sessionID})
}

// establishCDP wires up the chromedp remote-allocator + context for the
// given Browserbase connectURL. Tears down on failure.
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

// releaseCDP cancels the chromedp context + allocator. Safe to call
// when no session is attached (no-op).
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

// navigate is a small CDP nav wrapper used by OpenSession after a
// successful attach/create.
func (c *Computer) navigate(url string) error {
	if c.ctx == nil {
		return fmt.Errorf("browserbase: no active session — cannot navigate")
	}
	_, err := c.Execute(computer.Action{Type: "navigate", URL: url})
	return err
}

// fetchSessionConnectURL hits GET /v1/sessions/{id} and returns its
// connectUrl, erroring if the session is no longer RUNNING.
func (c *Computer) fetchSessionConnectURL(sessionID string) (string, error) {
	url := fmt.Sprintf("%s/sessions/%s", apiBase, sessionID)
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
		ID         string `json:"id"`
		Status     string `json:"status"`
		ConnectURL string `json:"connectUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Status != "" && result.Status != "RUNNING" {
		return "", fmt.Errorf("session %s is %s, not RUNNING (was KeepAlive set?)", sessionID, result.Status)
	}
	if result.ConnectURL == "" {
		return "", fmt.Errorf("session %s has no connectUrl", sessionID)
	}
	return result.ConnectURL, nil
}

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
