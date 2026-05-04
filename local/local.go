// Package local implements the Computer interface using a local Chrome/Chromium via CDP.
// It auto-launches Chrome if not already running.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/apteva/computer/som"
	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"golang.org/x/image/draw"
)

// Options is provider-level configuration applied at every Chrome
// launch this Computer performs. The agent's per-session toggles
// (OpenOptions.Proxy in particular) compose with these to produce
// the final launch flags — see relaunchIfProxyChanged.
type Options struct {
	// ProxyURL, if set, makes proxy=true a meaningful agent action.
	// Schemes: http://, https://, socks5://. Inline credentials
	// (http://user:pass@host:port) are extracted and applied via
	// CDP's Fetch.authRequired handler; --proxy-server itself does
	// not accept inline creds. Without this option, agent calls of
	// browser_session(proxy=true) on the local backend return a
	// configuration error rather than silently going direct.
	ProxyURL string
}

type Computer struct {
	display     computer.DisplaySize
	opts        Options
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	// proxyActive tracks whether the *current* Chrome process was
	// launched through ProxyURL. relaunchIfProxyChanged compares
	// this to the agent's intent on each OpenSession to decide
	// whether to tear down + relaunch.
	proxyActive bool

	// SoM (Set-of-Mark) state. Populated on every Screenshot() when
	// APTEVA_SOM is set, consumed by Execute for click/double_click
	// when the action carries a label= instead of coordinate=x,y.
	// Unused (nil map) when SoM is off — zero cost on that path.
	labelMu    sync.RWMutex
	lastLabels map[int]som.Element
}

// New creates a local Chrome-backed Computer with no provider options.
// Launches headed if DISPLAY is set, headless otherwise.
func New(display computer.DisplaySize) (*Computer, error) {
	return NewWithOptions(display, Options{})
}

// NewWithOptions creates a local Chrome-backed Computer with the
// given provider options. Eager: Chrome launches before the call
// returns. Use the returned Computer's OpenSession to toggle
// per-session settings (currently: proxy on/off when ProxyURL is
// configured).
//
// The eager launch happens WITHOUT the proxy applied; the agent's
// browser_session(open, proxy=true) triggers a relaunch through the
// configured ProxyURL. This keeps cold-start fast for direct
// connections and only pays the relaunch cost when the agent
// actually asks for a proxy.
func NewWithOptions(display computer.DisplaySize, opts Options) (*Computer, error) {
	c := &Computer{
		display: display,
		opts:    opts,
	}
	if err := c.launch(false); err != nil {
		return nil, err
	}
	return c, nil
}

// launch (re)starts Chrome. useProxy=true wires the configured
// ProxyURL into the allocator; false launches direct. Caller is
// responsible for c.Close()ing the previous context first when
// relaunching — see relaunchIfProxyChanged.
func (c *Computer) launch(useProxy bool) error {
	display := c.display
	// Mac and Windows always have a display; Linux needs DISPLAY set
	headless := runtime.GOOS == "linux" && os.Getenv("DISPLAY") == ""
	if os.Getenv("APTEVA_HEADLESS_BROWSER") == "1" {
		headless = true
	}

	proxyTag := "off"
	if useProxy && c.opts.ProxyURL != "" {
		proxyTag = "on"
	}
	fmt.Fprintf(os.Stderr, "[BROWSER] start: goos=%s goarch=%s headless=%v display=%dx%d DISPLAY=%q APTEVA_HEADLESS_BROWSER=%q CHROME_BIN=%q PATH_has_chrome=unknown proxy=%s\n",
		runtime.GOOS, runtime.GOARCH, headless,
		display.Width, display.Height,
		os.Getenv("DISPLAY"), os.Getenv("APTEVA_HEADLESS_BROWSER"),
		os.Getenv("CHROME_BIN"), proxyTag,
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
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-popup-blocking", true),
		// Avoid the headed/headless automation tells most bot
		// detectors trip on. Suppresses the "Chrome is being
		// controlled by automated test software" infobar (which
		// also exposes navigator.webdriver) and the auto-enabled
		// AutomationControlled blink feature.
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
	)
	if headless {
		// "new" is Chrome 109+'s rendering-equal headless mode —
		// shares the same pipeline as headed Chrome, so most JS
		// fingerprint differences disappear. Falls back to legacy
		// headless on older Chromiums via APTEVA_HEADLESS_MODE=old.
		mode := "new"
		if v := os.Getenv("APTEVA_HEADLESS_MODE"); v != "" {
			mode = v
		}
		opts = append(opts,
			chromedp.Flag("headless", mode),
			chromedp.Flag("disable-gpu", true),
		)
	}
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

	// Wire the configured proxy when the agent (or operator) asked
	// for it. parseProxyURL strips inline credentials so the
	// --proxy-server flag stays clean; the creds are then handed to
	// the Fetch.authRequired listener registered after launch.
	var proxyUser, proxyPass string
	if useProxy {
		if c.opts.ProxyURL == "" {
			return fmt.Errorf("local chrome: proxy requested but Options.ProxyURL is empty")
		}
		serverPart, user, pass, perr := parseProxyURL(c.opts.ProxyURL)
		if perr != nil {
			return fmt.Errorf("local chrome: invalid ProxyURL: %w", perr)
		}
		opts = append(opts, chromedp.Flag("proxy-server", serverPart))
		proxyUser, proxyPass = user, pass
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
		return fmt.Errorf("local chrome: failed to start: %w (goos=%s)", err, runtime.GOOS)
	}

	// Proxy auth: Chrome can't accept inline credentials in the
	// --proxy-server flag, so we intercept the proxy's 407 challenge
	// via Fetch.authRequired and respond with the parsed creds.
	// Without this, an http://user:pass@proxy URL would simply 407
	// every request. Skipped when the proxy needs no auth.
	if useProxy && (proxyUser != "" || proxyPass != "") {
		if err := chromedp.Run(ctx, fetch.Enable().WithHandleAuthRequests(true)); err != nil {
			cancel()
			allocCancel()
			return fmt.Errorf("local chrome: enable Fetch domain for proxy auth: %w", err)
		}
		c.installProxyAuthHandler(ctx, proxyUser, proxyPass)
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
		return fmt.Errorf("local chrome: failed to set viewport: %w", err)
	}

	// Inject the stealth patch as a new-document script so it runs
	// before any page JS, including pre-CSP inline blocks. This
	// covers ~80% of off-the-shelf bot-detection libraries
	// (Cloudflare's challenge JS, generic webdriver-checkers, etc.).
	// Failures here are non-fatal: a launched-but-unstealthed Chrome
	// is still useful for friendly sites.
	if _, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[BROWSER] WARN: stealth script install failed: %v\n", err)
	}

	// Verify the override stuck.
	var vpWidth, vpHeight int
	chromedp.Run(ctx, chromedp.Evaluate(`window.innerWidth`, &vpWidth))
	chromedp.Run(ctx, chromedp.Evaluate(`window.innerHeight`, &vpHeight))

	// User agent + process identity. Useful when diagnosing sandbox /
	// elevation issues ("running as Admin" on Windows refuses to sandbox).
	var ua string
	chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &ua))

	// UA rewrite: even with --headless=new, some Chromium builds
	// still leak "HeadlessChrome" in the UA string. Stripping it via
	// Network.SetUserAgentOverride is cheap insurance — the UA value
	// is what server-side fingerprinting reads, and Sec-CH-UA gets
	// patched in the same call when sets the right metadata.
	if strings.Contains(ua, "HeadlessChrome") {
		fixed := strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
		if err := chromedp.Run(ctx, emulation.SetUserAgentOverride(fixed)); err == nil {
			ua = fixed
		}
	}

	fmt.Fprintf(os.Stderr, "[BROWSER] Chrome launched: requested=%dx%d viewport=%dx%d headless=%v proxy=%s\n",
		display.Width, display.Height, vpWidth, vpHeight, headless, proxyTag)
	fmt.Fprintf(os.Stderr, "[BROWSER] pid=%d uid=%d ua=%q\n", os.Getpid(), os.Getuid(), ua)

	c.ctx = ctx
	c.cancel = cancel
	c.allocCancel = allocCancel
	c.proxyActive = useProxy
	return nil
}

// parseProxyURL splits a proxy URL into the bare scheme://host:port
// (which Chrome's --proxy-server accepts) and any inline credentials
// (which it does not). socks5://, http://, https:// all supported.
// A bare host:port is treated as http://host:port.
func parseProxyURL(raw string) (server, user, pass string, err error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	if u.Host == "" {
		return "", "", "", fmt.Errorf("missing host in %q", raw)
	}
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return u.Scheme + "://" + u.Host, user, pass, nil
}

// installProxyAuthHandler subscribes to Fetch.authRequired events
// and responds with ProvideCredentials. Called once per launch when
// the proxy URL had inline creds. The listener lives for the
// lifetime of ctx — when relaunch tears down ctx, it goes with it.
func (c *Computer) installProxyAuthHandler(ctx context.Context, user, pass string) {
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *fetch.EventAuthRequired:
			go func() {
				_ = chromedp.Run(ctx,
					fetch.ContinueWithAuth(e.RequestID, &fetch.AuthChallengeResponse{
						Response: fetch.AuthChallengeResponseResponseProvideCredentials,
						Username: user,
						Password: pass,
					}),
				)
			}()
		case *fetch.EventRequestPaused:
			// We didn't pause requests for content — only for auth —
			// but Fetch.enable+handleAuthRequests delivers paused
			// non-auth requests we must continue() or they hang.
			go func() {
				_ = chromedp.Run(ctx, fetch.ContinueRequest(e.RequestID))
			}()
		}
	})
}

// relaunchIfProxyChanged compares the agent's per-session proxy
// intent against the current Chrome launch state and tears down +
// relaunches if they disagree. Chrome cannot change its --proxy-server
// at runtime, so this is the only way to honor an agent flip from
// direct → proxy or proxy → direct mid-conversation. nil intent
// (agent didn't specify) preserves whatever state we're in.
func (c *Computer) relaunchIfProxyChanged(want *bool) error {
	if want == nil {
		return nil
	}
	if *want && c.opts.ProxyURL == "" {
		return fmt.Errorf("local backend has no ProxyURL configured; set computer.Config.ProxyURL (or APTEVA_LOCAL_PROXY_URL) or use a cloud backend")
	}
	if *want == c.proxyActive {
		return nil // already in the desired state
	}
	// Tear down the current Chrome before launching a new one.
	if c.cancel != nil {
		c.cancel()
	}
	if c.allocCancel != nil {
		c.allocCancel()
	}
	c.ctx, c.cancel, c.allocCancel = nil, nil, nil
	return c.launch(*want)
}

// OpenSession implements computer.SessionOpener for the local
// backend. The local backend has no remote session lifecycle, so
// this is mostly a thin nav — but it's also the hook that lets the
// agent flip proxy state per session via OpenOptions.Proxy.
func (c *Computer) OpenSession(opts computer.OpenOptions) error {
	if opts.SessionID != "" {
		return fmt.Errorf("local backend cannot resume sessions (no remote state)")
	}
	if opts.ContextID != "" {
		return fmt.Errorf("local backend has no persistent contexts; open without context_id")
	}
	if err := c.relaunchIfProxyChanged(opts.Proxy); err != nil {
		return err
	}
	if opts.URL != "" {
		_, err := c.Execute(computer.Action{Type: "navigate", URL: opts.URL})
		return err
	}
	return nil
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
		// SoM: resolve label to bbox center if the action carries
		// a label= (takes precedence over X/Y). Falls back to raw
		// coordinates when the label is unknown or SoM is off —
		// never a hard error, the agent can always use X,Y.
		x, y := action.X, action.Y
		labelNote := ""
		if action.Label != 0 {
			if e, ok := c.resolveLabel(action.Label); ok {
				cx, cy := e.Center()
				labelNote = fmt.Sprintf(" [label=%d %s '%.20s' → center %d,%d]", action.Label, e.Tag, e.Text, cx, cy)
				x, y = cx, cy
			} else {
				fmt.Fprintf(os.Stderr, "[BROWSER] click: label=%d not in current screenshot map, using X,Y=%d,%d\n", action.Label, action.X, action.Y)
			}
		}
		var urlBefore string
		chromedp.Run(c.ctx, chromedp.Location(&urlBefore))
		fmt.Fprintf(os.Stderr, "[BROWSER] click (%d,%d) on %s (display=%dx%d)%s\n",
			x, y, urlBefore, c.display.Width, c.display.Height, labelNote)
		if err := chromedp.Run(c.ctx,
			chromedp.MouseClickXY(float64(x), float64(y)),
		); err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSER] click ERROR: %v\n", err)
			return nil, fmt.Errorf("click: %w", err)
		}
		// Follow up with explicit focus on the element at (x, y). CDP
		// mouse events don't always move DOM focus to input/textarea
		// elements under the cursor — especially in headless mode —
		// so a click on a form field leaves focus on <body>, and
		// subsequent type actions (insertText / key events) land
		// nowhere. Mimics what Playwright's click does internally:
		// dispatch mouse event, then forcibly focus the element at
		// the click point. Best-effort; errors are non-fatal because
		// we also want plain link/button clicks to work even when
		// focusability is ambiguous.
		focusJS := fmt.Sprintf(`(function(){
			var el = document.elementFromPoint(%d, %d);
			if (el && typeof el.focus === 'function') { el.focus(); return el.tagName; }
			return null;
		})()`, x, y)
		var focusedTag string
		if err := chromedp.Run(c.ctx, chromedp.Evaluate(focusJS, &focusedTag)); err == nil && focusedTag != "" {
			fmt.Fprintf(os.Stderr, "[BROWSER] click focused <%s>\n", strings.ToLower(focusedTag))
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
		// Same label-to-coord resolution as click.
		x, y := action.X, action.Y
		if action.Label != 0 {
			if e, ok := c.resolveLabel(action.Label); ok {
				x, y = e.Center()
			}
		}
		if err := chromedp.Run(c.ctx,
			chromedp.MouseClickXY(float64(x), float64(y), chromedp.ClickCount(2)),
		); err != nil {
			return nil, fmt.Errorf("double_click: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
		return c.Screenshot()

	case "type":
		// Prefer Input.insertText — it pushes text through Chrome's
		// input pipeline the same way a paste does, firing real
		// `input`/`change` events. Works reliably on React/Vue
		// controlled inputs and doesn't require pixel-perfect
		// focus; Chrome inserts into the focused editable element
		// even if the recent click landed on a parent container.
		//
		// Fallback: synthesized keystrokes via chromedp.KeyEvent.
		// insertText requires an editable element to be focused; if
		// the agent is trying to send text outside an input (e.g.
		// search-as-you-type on a non-form page), KeyEvent still
		// dispatches keys at the page level.
		err := chromedp.Run(c.ctx, input.InsertText(action.Text))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSER] insertText failed (%v), falling back to KeyEvent\n", err)
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

// scroll dispatches a real CDP mouseWheel event at (x, y). This scrolls
// whichever element is under the cursor — including nested scrollable
// divs, virtualized lists, and pages where `window.scrollBy` no-ops
// because the document body isn't the actual scroll container. Real
// wheel events also fire DOM `wheel` handlers, so infinite-scroll and
// chat/inbox UIs respond the way they do for a human.
//
// Falls back to window.scrollBy on error (e.g. CDP not available), so
// the call never silently drops.
func (c *Computer) scroll(a computer.Action) error {
	amount := a.Amount
	if amount <= 0 {
		amount = 3
	}
	// 100 px per unit matches the old JS behavior. Chrome's default
	// wheel tick is ~100 px, so amount=3 = ~3 ticks ≈ one "flick".
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
		return fmt.Errorf("unknown scroll direction %q (want up/down/left/right)", a.Direction)
	}

	// Default target: center of the viewport. Callers that know what
	// they're scrolling pass explicit x,y (e.g. scroll_at).
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

	// Fallback: the old JS scroll. Better than dropping the action if
	// the wheel event failed for some reason (off-screen, odd host).
	fmt.Fprintf(os.Stderr, "[BROWSER] wheel dispatch failed (%v), falling back to window.scrollBy\n", err)
	js := fmt.Sprintf("window.scrollBy(%d, %d)", int(dx), int(dy))
	return chromedp.Run(c.ctx, chromedp.Evaluate(js, nil))
}

func (c *Computer) Screenshot() ([]byte, error) {
	// VIEWPORT-ONLY screenshot. Previously we used chromedp.FullScreenshot
	// which captures the entire scrollable page — on a long page this
	// produced e.g. a 1600×1800 image, and scaleToDisplay then vertically
	// squashed it to 1600×800 (the viewport). The agent then picked
	// coordinates in that squashed image, and we dispatched clicks at
	// those pixel values against the actual (un-squashed) viewport —
	// hitting completely different elements. Classic cause of "click
	// doesn't land on the input" bug reports.
	//
	// page.CaptureScreenshot without captureBeyondViewport returns
	// exactly the visible area at CSS-pixel resolution (we pinned
	// deviceScaleFactor=1 via SetDeviceMetricsOverride), so the image
	// dimensions match what the agent's x,y click coordinates mean.
	// JPEG quality override via APTEVA_SCREENSHOT_QUALITY (1-100).
	// Lower = smaller payload, fewer vision tokens, less detail. Useful
	// for vision-model A/B: some models read coarser compressed images
	// more reliably than high-fidelity ones (less artifact-picking).
	quality := int64(60)
	if v := os.Getenv("APTEVA_SCREENSHOT_QUALITY"); v != "" {
		var q int64
		if _, perr := fmt.Sscanf(v, "%d", &q); perr == nil && q >= 1 && q <= 100 {
			quality = q
		}
	}

	var buf []byte
	err := chromedp.Run(c.ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			b, err := page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatJpeg).
				WithQuality(quality).
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

	// Defensive: deviceScaleFactor is pinned to 1, so the image should
	// already match the viewport dimensions. scaleToDisplay is a no-op
	// unless the image comes back larger (shouldn't happen now, but
	// kept for HiDPI/agent-modified-viewport edge cases). It now only
	// scales UNIFORMLY when the aspect ratio matches — any mismatch
	// is logged and returned as-is so a future debugger can notice.
	buf, _ = c.scaleToDisplay(buf)

	// SoM annotation — off by default, enabled via APTEVA_SOM=1. On
	// the happy path (SoM on, enumeration returns N elements), this
	// adds a read-only DOM query, stores the resulting label→bbox
	// map, and composites numeric badges onto the image bytes. On
	// any failure we log and return the raw screenshot — SoM never
	// blocks a successful screenshot round-trip.
	if som.Enabled() {
		elements, err := c.enumerate()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSER] som enum failed: %v (returning un-annotated screenshot)\n", err)
			return buf, nil
		}
		// Publish the label map first so a click(label=N) issued
		// against the just-taken screenshot always finds its bbox,
		// even if the composite step below fails.
		m := make(map[int]som.Element, len(elements))
		for _, e := range elements {
			m[e.Label] = e
		}
		c.labelMu.Lock()
		c.lastLabels = m
		c.labelMu.Unlock()

		annotated, err := som.Annotate(buf, elements)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[BROWSER] som annotate failed: %v (returning un-annotated screenshot)\n", err)
			return buf, nil
		}
		fmt.Fprintf(os.Stderr, "[BROWSER] som annotated: %d elements labeled\n", len(elements))
		return annotated, nil
	}

	return buf, nil
}

// enumerate runs the SoM enumeration script in the page's main world
// and returns the parsed element list. Used only when SoM is active.
// Read-only DOM access: no mutations, no event listeners, no globals.
func (c *Computer) enumerate() ([]som.Element, error) {
	var raw json.RawMessage
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(som.EnumScript, &raw)); err != nil {
		return nil, err
	}
	els, err := som.UnmarshalElements(raw)
	if err != nil {
		return nil, err
	}
	return els, nil
}

// resolveLabel looks up the bbox for a previously-issued label. Called
// from Execute's click/double_click path when action.Label != 0.
// Returns ok=false if no screenshot has been taken yet, the label
// doesn't exist, or SoM was off at screenshot time (map nil).
func (c *Computer) resolveLabel(label int) (som.Element, bool) {
	c.labelMu.RLock()
	defer c.labelMu.RUnlock()
	if c.lastLabels == nil {
		return som.Element{}, false
	}
	e, ok := c.lastLabels[label]
	return e, ok
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

	// Refuse to non-uniformly scale. If the aspect ratio doesn't match
	// (within 1% tolerance) we'd be squashing an image from one shape
	// into another — every click coordinate Kimi picks in the squashed
	// image would dispatch against a differently-shaped viewport.
	// Previous FullScreenshot + squash was exactly this bug. Log and
	// return the raw image so the caller sees real pixel coords.
	srcAspect := float64(actualW) / float64(actualH)
	dstAspect := float64(targetW) / float64(targetH)
	if srcAspect/dstAspect > 1.01 || dstAspect/srcAspect > 1.01 {
		fmt.Fprintf(os.Stderr, "[BROWSER] screenshot aspect mismatch %dx%d (%.2f:1) vs target %dx%d (%.2f:1) — returning unscaled\n",
			actualW, actualH, srcAspect, targetW, targetH, dstAspect)
		return data, nil
	}

	fmt.Fprintf(os.Stderr, "[BROWSER] scaling screenshot %dx%d -> %dx%d (uniform)\n", actualW, actualH, targetW, targetH)

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
