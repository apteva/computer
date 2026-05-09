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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

	// activeContextID is the Chrome user-data-dir keyed in
	// localContextsBaseDir() that the *current* Chrome process is
	// using. Empty means an ephemeral profile (chromedp default —
	// fresh cookies every launch). relaunchIfContextChanged compares
	// this to the agent's intent on each OpenSession the same way
	// relaunchIfProxyChanged does.
	activeContextID string

	// SoM (Set-of-Mark) state. Populated on every Screenshot() when
	// APTEVA_SOM is set, consumed by Execute for click/double_click
	// when the action carries a label= instead of coordinate=x,y.
	// Unused (nil map) when SoM is off — zero cost on that path.
	labelMu    sync.RWMutex
	lastLabels map[int]som.Element

	// debugURL is Chrome's DevTools frontend URL for the active page
	// target — opening it in any browser yields a live, interactive
	// view of this Computer's Chrome (screencast + input forwarded
	// over CDP). Mirrors browserbase.Computer.DebugURL() so callers
	// can type-assert against `interface{ DebugURL() string }`
	// without caring which backend they hold. Empty if the post-launch
	// fetch failed; see WARN log line for cause.
	debugURL string
}

// pickFreePort asks the OS for an unused TCP port on loopback by
// opening a listener on :0, reading the assigned port, and closing.
// Used to pin Chrome's --remote-debugging-port to a known value so
// we can fetch the DevTools frontend URL after launch. Tiny race
// between Close and Chrome bind; in practice fine.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// fetchFrontendURL queries Chrome's HTTP DevTools endpoint
// (http://127.0.0.1:<port>/json) for the list of inspectable
// targets and returns the devtoolsFrontendUrl of the first
// page-type target. That URL points at Chrome's own DevTools
// frontend, which both renders the live page and forwards input
// over CDP — opening it in another browser is enough to interact
// with the underlying Chrome.
func fetchFrontendURL(port int) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var targets []struct {
		Type                string `json:"type"`
		DevtoolsFrontendURL string `json:"devtoolsFrontendUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.Type == "page" && t.DevtoolsFrontendURL != "" {
			return t.DevtoolsFrontendURL, nil
		}
	}
	return "", fmt.Errorf("no page target found in /json response")
}

// localContextsBaseDir returns where per-context Chrome user-data-dirs
// live. Override via APTEVA_LOCAL_CONTEXT_DIR; default ~/.apteva/local-contexts.
// Falls back to /tmp/apteva-local-contexts if HOME isn't resolvable
// (CI containers without a $HOME etc.) so we always have a usable path.
func localContextsBaseDir() string {
	if v := os.Getenv("APTEVA_LOCAL_CONTEXT_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/tmp/apteva-local-contexts"
	}
	return filepath.Join(home, ".apteva", "local-contexts")
}

// contextDirFor resolves a context-id to its on-disk Chrome
// user-data-dir. Does NOT create the directory; caller (launch)
// passes it to chromedp which creates it on first use.
func contextDirFor(id string) string {
	return filepath.Join(localContextsBaseDir(), id)
}

// validateContextID guards the on-disk path from path-traversal
// inputs. Empty is allowed (ephemeral profile); anything else must
// match a slug pattern. Keeps the agent from passing
// "../../../etc/passwd" as a context_id and writing Chrome state
// outside the contexts dir.
func validateContextID(id string) error {
	if id == "" {
		return nil
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("invalid context_id %q: only [A-Za-z0-9._-] allowed", id)
		}
	}
	if id == "." || id == ".." || strings.HasPrefix(id, "/") {
		return fmt.Errorf("invalid context_id %q", id)
	}
	return nil
}

// reclaimSingletonLockIfStale removes Chrome's SingletonLock file
// from a context dir if it exists AND the PID it points at is not
// alive. This handles "previous run crashed and left a lock" without
// requiring the operator to clean up by hand. If the lock points at
// a live process, returns the PID and the caller surfaces a clear
// "context in use" error.
//
// SingletonLock is a symlink whose target text is "<hostname>-<pid>".
// A stale lock has a target whose pid no longer exists.
func reclaimSingletonLockIfStale(contextDir string) (alivePID int, err error) {
	lockPath := filepath.Join(contextDir, "SingletonLock")
	target, err := os.Readlink(lockPath)
	if err != nil {
		// No lock = clean to launch. ENOENT, ENOTDIR, etc. all map here.
		return 0, nil
	}
	// Target form: "hostname-12345". Pull the trailing pid.
	dash := strings.LastIndex(target, "-")
	if dash < 0 || dash == len(target)-1 {
		// Unparseable target — assume stale and remove.
		_ = os.Remove(lockPath)
		return 0, nil
	}
	pid, perr := strconv.Atoi(target[dash+1:])
	if perr != nil {
		_ = os.Remove(lockPath)
		return 0, nil
	}
	proc, ferr := os.FindProcess(pid)
	if ferr != nil {
		_ = os.Remove(lockPath)
		return 0, nil
	}
	// On Unix, Signal(0) is the canonical "is this PID alive" probe.
	// Returns nil if the process exists (and we can signal it),
	// "process already finished" otherwise.
	if sigErr := proc.Signal(syscall.Signal(0)); sigErr != nil {
		_ = os.Remove(lockPath)
		return 0, nil
	}
	// Lock is held by a live process — caller should error out.
	return pid, nil
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
	if err := c.launch(false, ""); err != nil {
		return nil, err
	}
	return c, nil
}

// launch (re)starts Chrome. useProxy=true wires the configured
// ProxyURL into the allocator; false launches direct. Caller is
// responsible for c.Close()ing the previous context first when
// relaunching — see relaunchIfProxyChanged.
func (c *Computer) launch(useProxy bool, contextID string) error {
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

	// Pin Chrome's remote debugging endpoint to a known port on
	// loopback. chromedp would otherwise pass --remote-debugging-port=0
	// and parse the port from stderr internally without exposing it,
	// so we wouldn't be able to reach /json afterwards. Bound to
	// 127.0.0.1 — never expose CDP to non-loopback without an auth proxy.
	debugPort, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("local chrome: pick debug port: %w", err)
	}
	opts = append(opts,
		chromedp.Flag("remote-debugging-port", strconv.Itoa(debugPort)),
		chromedp.Flag("remote-debugging-address", "127.0.0.1"),
		// Chrome 111+ rejects CDP WebSocket upgrades whose Origin
		// header isn't on this allowlist (CSRF protection for
		// localhost-bound CDP). Since the port is already 127.0.0.1
		// only, allowing any origin doesn't widen the attack surface;
		// it just lets our local viewer page (served from a different
		// loopback port) actually connect.
		chromedp.Flag("remote-allow-origins", "*"),
	)

	// Persistent context: when the agent passed a context_id, point
	// Chrome's user-data-dir at the per-context directory on disk so
	// cookies / localStorage / IndexedDB survive between launches.
	// Empty contextID = ephemeral profile (chromedp default).
	if contextID != "" {
		dir := contextDirFor(contextID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("local chrome: create context dir %s: %w", dir, err)
		}
		// Reclaim a stale SingletonLock left behind by a prior crash;
		// surface a clear error if a live process owns this context.
		if pid, _ := reclaimSingletonLockIfStale(dir); pid > 0 {
			return fmt.Errorf("local chrome: context %q is in use by PID %d (Chrome SingletonLock held); close it or pick a different context_id", contextID, pid)
		}
		opts = append(opts, chromedp.UserDataDir(dir))
		fmt.Fprintf(os.Stderr, "[BROWSER] context=%s user-data-dir=%s\n", contextID, dir)
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
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(ctx)
		return err
	})); err != nil {
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

	if u, ferr := fetchFrontendURL(debugPort); ferr == nil {
		c.debugURL = u
	} else {
		fmt.Fprintf(os.Stderr, "[BROWSER] WARN: debug URL fetch failed: %v\n", ferr)
	}

	fmt.Fprintf(os.Stderr, "[BROWSER] Chrome launched: requested=%dx%d viewport=%dx%d headless=%v proxy=%s\n",
		display.Width, display.Height, vpWidth, vpHeight, headless, proxyTag)
	fmt.Fprintf(os.Stderr, "[BROWSER] pid=%d uid=%d ua=%q debug=%s\n", os.Getpid(), os.Getuid(), ua, c.debugURL)

	c.ctx = ctx
	c.cancel = cancel
	c.allocCancel = allocCancel
	c.proxyActive = useProxy
	c.activeContextID = contextID
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
	// Graceful teardown before relaunch so any in-flight cookie
	// writes flush to disk (matters when c.activeContextID is set).
	c.gracefulTeardownForRelaunch()
	c.ctx, c.cancel, c.allocCancel = nil, nil, nil
	c.debugURL = ""
	// Preserve the active context across the proxy-driven relaunch —
	// otherwise toggling proxy mid-conversation would silently drop
	// cookies / localStorage. The context is the agent's persistence
	// surface; proxy is orthogonal.
	return c.launch(*want, c.activeContextID)
}

// relaunchIfContextChanged tears down + relaunches Chrome with a
// different on-disk user-data-dir when the agent asks for a context
// switch. Mirrors relaunchIfProxyChanged: the proxy state is
// preserved across the relaunch via c.proxyActive, just as the
// context is preserved when proxy changes.
//
// want is the agent-requested context_id ("" = ephemeral profile).
// Returns nil if the active context already matches.
func (c *Computer) relaunchIfContextChanged(want string) error {
	if want == c.activeContextID {
		return nil
	}
	// Graceful shutdown is load-bearing here: leaving via SIGKILL
	// loses cookies still in Chrome's write journal, defeating the
	// whole point of persistent contexts.
	c.gracefulTeardownForRelaunch()
	c.ctx, c.cancel, c.allocCancel = nil, nil, nil
	c.debugURL = ""
	return c.launch(c.proxyActive, want)
}

// DebugURL returns Chrome's DevTools frontend URL for the active page
// target. Opening it in any browser yields a live, interactive view
// of this Computer's Chrome — both screencast and input forwarded
// over CDP. Returns "" if the post-launch fetch failed (see WARN log).
//
// The URL is loopback-only (127.0.0.1) by design; expose it via an
// authenticating proxy if the dashboard isn't on the same host.
//
// Mirrors browserbase.Computer.DebugURL() so callers can type-assert
// against `interface{ DebugURL() string }` without branching on backend.
func (c *Computer) DebugURL() string { return c.debugURL }

// OpenSession implements computer.SessionOpener for the local
// backend. Three things this hook does:
//
//   - opts.SessionID — rejected. Local has no remote session
//     lifecycle to attach to; the Computer instance IS the session.
//   - opts.ContextID — opt-in cookie/localStorage/IndexedDB
//     persistence via Chrome's user-data-dir. Empty = ephemeral
//     profile (default). Same id across runs = same logged-in
//     state, same as browserbase contexts.
//   - opts.Proxy — flip the configured ProxyURL on or off.
//
// Both ContextID and Proxy can trigger a Chrome relaunch. We
// process Context first so the proxy relaunch (if also needed)
// inherits the right user-data-dir.
func (c *Computer) OpenSession(opts computer.OpenOptions) error {
	if opts.SessionID != "" {
		return fmt.Errorf("local backend has no remote sessions; pass context_id for persistence instead")
	}
	if err := validateContextID(opts.ContextID); err != nil {
		return err
	}
	if err := c.relaunchIfContextChanged(opts.ContextID); err != nil {
		return err
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
	// Defense against a nil chromedp context. Reachable when the
	// agent issued a sequence that closed the browser, then called
	// computer_use before re-opening (e.g. open → close → screenshot),
	// or when an internal relaunch failed mid-flight and left the
	// Computer half-initialised. Returning a clear error lets the
	// agent recover via browser_session(open, ...) instead of
	// SIGSEGV-ing the test process at chromedp.FromContext(nil).
	if c.ctx == nil {
		return nil, fmt.Errorf("%s: browser not open — call browser_session(action=open, ...) first", action.Type)
	}
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
		// Reject silent no-target clicks. The agent has been observed
		// emitting computer_use(action=click, _reason=...) without
		// label OR coordinate when uncertain — defaulting to (0,0)
		// silently dispatches a click at the page corner that does
		// nothing useful, and the agent never sees the mistake.
		// Surface as a visible error so the agent retries with a
		// real target. Mirrors the empty-key error we ship for the
		// same anti-pattern on action=key.
		if action.Label == 0 && action.X == 0 && action.Y == 0 {
			return nil, fmt.Errorf("click: no target — provide label=N from the latest screenshot, or X,Y coordinates")
		}
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
		// Same no-target guard as click — see the comment there.
		if action.Label == 0 && action.X == 0 && action.Y == 0 {
			return nil, fmt.Errorf("double_click: no target — provide label=N from the latest screenshot, or X,Y coordinates")
		}
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
		if err := c.dispatchKey(action.Key); err != nil {
			return nil, fmt.Errorf("key %q: %w", action.Key, err)
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
	if c.ctx == nil {
		return nil, fmt.Errorf("screenshot: browser not open — call browser_session(action=open, ...) first")
	}
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
			c.maybeDumpScreenshot(buf, nil)
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
			c.maybeDumpScreenshot(buf, elements)
			return buf, nil
		}
		fmt.Fprintf(os.Stderr, "[BROWSER] som annotated: %d elements labeled\n", len(elements))
		c.maybeDumpScreenshot(annotated, elements)
		return annotated, nil
	}

	c.maybeDumpScreenshot(buf, nil)
	return buf, nil
}

// maybeDumpScreenshot writes the JPEG (and SoM element JSON, if any)
// to APTEVA_SCREENSHOT_DUMP_DIR. Off by default — set the env var
// to a writable directory to capture every frame the agent saw.
//
// One file per Screenshot() call:
//   <dir>/screenshot_<unix_ms>.jpg   the EXACT pixels the LLM received,
//                                    SoM badges baked in if SoM is on
//   <dir>/screenshot_<unix_ms>.json  the Element list (label → bbox/tag/text)
//                                    that powered click(label=N) resolution
//
// Useful for diagnosing agent behaviour: open the JPEGs in order to
// see what the model "thought" it was looking at; cross-reference
// the JSON when the agent picked a label that doesn't seem to match
// what the badge claims.
//
// Errors are logged and swallowed — debug instrumentation must NEVER
// break a real screenshot round-trip.
func (c *Computer) maybeDumpScreenshot(buf []byte, elements []som.Element) {
	dir := os.Getenv("APTEVA_SCREENSHOT_DUMP_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[BROWSER] screenshot dump mkdir failed: %v\n", err)
		return
	}
	// Microsecond precision — multiple screenshots per second won't
	// collide. Sortable lexically by filename.
	ts := time.Now().Format("20060102-150405.000000")
	base := filepath.Join(dir, "screenshot_"+ts)
	if err := os.WriteFile(base+".jpg", buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "[BROWSER] screenshot dump write failed: %v\n", err)
		return
	}
	if elements != nil {
		if data, err := json.MarshalIndent(elements, "", "  "); err == nil {
			_ = os.WriteFile(base+".json", data, 0o644)
		}
	}
	fmt.Fprintf(os.Stderr, "[BROWSER] screenshot dumped: %s.jpg (%d bytes, %d labels)\n", base, len(buf), len(elements))
}

// keySpec maps a logical key name to the three CDP fields that
// Input.dispatchKeyEvent needs to fire a real, recognised browser
// keystroke (vs typing the literal characters).
//
//   Key  — DOM KeyboardEvent.key value ("Escape", "Enter", "a")
//   Code — DOM KeyboardEvent.code value (physical key, e.g. "Escape", "KeyA")
//   VK   — Windows virtual key code (legacy fallback some pages still read)
//
// Reference: https://developer.mozilla.org/docs/Web/API/UI_Events/Keyboard_event_code_values
type keySpec struct {
	Key, Code string
	VK        int
}

// specialKeys maps human-typed key names to their CDP descriptor.
// Names are case-insensitive on lookup. Coverage chosen for the
// keys an agent actually reaches for: dialog dismissal, form submit,
// focus traversal, undo/redo/copy/paste, navigation by arrows.
//
// Anything not in this map AND not a single printable char falls
// through to chromedp.KeyEvent which types the runes literally.
var specialKeys = map[string]keySpec{
	"escape":    {"Escape", "Escape", 27},
	"esc":       {"Escape", "Escape", 27},
	"enter":     {"Enter", "Enter", 13},
	"return":    {"Enter", "Enter", 13},
	"tab":       {"Tab", "Tab", 9},
	"backspace": {"Backspace", "Backspace", 8},
	"delete":    {"Delete", "Delete", 46},
	"del":       {"Delete", "Delete", 46},
	"space":     {" ", "Space", 32},
	"arrowup":   {"ArrowUp", "ArrowUp", 38},
	"up":        {"ArrowUp", "ArrowUp", 38},
	"arrowdown": {"ArrowDown", "ArrowDown", 40},
	"down":      {"ArrowDown", "ArrowDown", 40},
	"arrowleft": {"ArrowLeft", "ArrowLeft", 37},
	"left":      {"ArrowLeft", "ArrowLeft", 37},
	"arrowright": {"ArrowRight", "ArrowRight", 39},
	"right":     {"ArrowRight", "ArrowRight", 39},
	"home":      {"Home", "Home", 36},
	"end":       {"End", "End", 35},
	"pageup":    {"PageUp", "PageUp", 33},
	"pgup":      {"PageUp", "PageUp", 33},
	"pagedown":  {"PageDown", "PageDown", 34},
	"pgdn":      {"PageDown", "PageDown", 34},
	"f1":        {"F1", "F1", 112},
	"f2":        {"F2", "F2", 113},
	"f3":        {"F3", "F3", 114},
	"f4":        {"F4", "F4", 115},
	"f5":        {"F5", "F5", 116},
	"f6":        {"F6", "F6", 117},
	"f7":        {"F7", "F7", 118},
	"f8":        {"F8", "F8", 119},
	"f9":        {"F9", "F9", 120},
	"f10":       {"F10", "F10", 121},
	"f11":       {"F11", "F11", 122},
	"f12":       {"F12", "F12", 123},
}

// parseModifier maps human modifier names to CDP modifier-mask bits.
// Returns (mask, ok); ok=false on unrecognised input so the caller
// can fall back to literal-text typing instead of silently dropping.
func parseModifier(s string) (input.Modifier, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "alt", "option", "opt":
		return input.ModifierAlt, true
	case "ctrl", "control":
		return input.ModifierCtrl, true
	case "cmd", "meta", "command", "super", "win":
		return input.ModifierMeta, true
	case "shift":
		return input.ModifierShift, true
	}
	return 0, false
}

// dispatchKey turns the agent's `key` parameter into a real browser
// keystroke. Three input shapes, in order of specificity:
//
//   1. Modifier combo: "ctrl+a", "shift+tab", "ctrl+shift+z",
//      "cmd+c". Split on '+', leftmost segments are modifiers,
//      rightmost is the key. Each piece resolves through specialKeys
//      or single-char fallback. Unknown modifiers degrade to typing
//      the literal string (preserves backwards-compat over silent
//      drop).
//
//   2. Named special key: "Escape", "Enter", "ArrowUp", etc.
//      Looked up in specialKeys (case-insensitive). Dispatched as
//      a real keyDown+keyUp pair via Input.dispatchKeyEvent so
//      browsers see the actual keystroke (not characters typed).
//
//   3. Single printable char: "a", "?", "1". Routed through
//      chromedp.KeyEvent, which types it as text — the historical
//      behaviour the agent was built around.
//
// Anything else (multi-char unknown string) falls through to
// chromedp.KeyEvent. That's how the OLD broken behaviour
// silently corrupted state — but now it's at least a documented
// fallback rather than an unannounced bug. Operator can grep the
// "[BROWSER] key fallback" log to find agent calls that should be
// added to specialKeys.
func (c *Computer) dispatchKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty key")
	}
	mods := input.ModifierNone
	keyName := key

	// Detect modifier combo. Single '+' inside a string is the only
	// signal; we don't try to recognise plain "+" as a key.
	if strings.Contains(key, "+") && len(key) > 1 {
		parts := strings.Split(key, "+")
		recognised := true
		for _, m := range parts[:len(parts)-1] {
			bit, ok := parseModifier(m)
			if !ok {
				recognised = false
				break
			}
			mods |= bit
		}
		if recognised {
			keyName = strings.TrimSpace(parts[len(parts)-1])
		} else {
			// Unknown modifier — fall back to literal typing.
			fmt.Fprintf(os.Stderr, "[BROWSER] key fallback (unknown modifier in %q): typing literally\n", key)
			return chromedp.Run(c.ctx, chromedp.KeyEvent(key))
		}
	}

	// Try the named-key table first.
	if spec, ok := specialKeys[strings.ToLower(keyName)]; ok {
		return chromedp.Run(c.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			down := input.DispatchKeyEvent(input.KeyDown).
				WithKey(spec.Key).WithCode(spec.Code).
				WithWindowsVirtualKeyCode(int64(spec.VK)).
				WithModifiers(mods)
			if err := down.Do(ctx); err != nil {
				return err
			}
			up := input.DispatchKeyEvent(input.KeyUp).
				WithKey(spec.Key).WithCode(spec.Code).
				WithWindowsVirtualKeyCode(int64(spec.VK)).
				WithModifiers(mods)
			return up.Do(ctx)
		}))
	}

	// Single-char with modifier (e.g. ctrl+a, shift+t). Dispatch as
	// a real key event so the browser sees Ctrl+A as "select all"
	// (not the letter "a"). Without this branch, ctrl+a would type
	// the character "a" with the Ctrl modifier IGNORED, because
	// chromedp.KeyEvent doesn't carry modifiers.
	if len(keyName) == 1 {
		ch := keyName[0]
		if mods != input.ModifierNone {
			// Build a plausible Code value: KeyA, KeyB, ..., Digit0, ..., Digit9
			var code string
			switch {
			case ch >= 'a' && ch <= 'z':
				code = "Key" + strings.ToUpper(keyName)
			case ch >= 'A' && ch <= 'Z':
				code = "Key" + keyName
			case ch >= '0' && ch <= '9':
				code = "Digit" + keyName
			}
			vk := int64(strings.ToUpper(keyName)[0])
			return chromedp.Run(c.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				down := input.DispatchKeyEvent(input.KeyDown).
					WithKey(strings.ToLower(keyName)).WithCode(code).
					WithWindowsVirtualKeyCode(vk).
					WithModifiers(mods)
				if err := down.Do(ctx); err != nil {
					return err
				}
				up := input.DispatchKeyEvent(input.KeyUp).
					WithKey(strings.ToLower(keyName)).WithCode(code).
					WithWindowsVirtualKeyCode(vk).
					WithModifiers(mods)
				return up.Do(ctx)
			}))
		}
		// No modifier — type the char as text (existing path).
		return chromedp.Run(c.ctx, chromedp.KeyEvent(keyName))
	}

	// Multi-char unknown name — surface a fallback log and type
	// literally. Less surprising than silently doing nothing; lets
	// us discover real names the agent uses that we should add to
	// specialKeys.
	fmt.Fprintf(os.Stderr, "[BROWSER] key fallback (unknown key %q): typing literally\n", keyName)
	return chromedp.Run(c.ctx, chromedp.KeyEvent(keyName))
}

// enumerate runs the SoM enumeration script in the page's main world
// and returns the parsed element list. Used only when SoM is active.
// Read-only DOM access: no mutations, no event listeners, no globals.
func (c *Computer) enumerate() ([]som.Element, error) {
	// Fast path: JS-injected DOM walk (handles same-origin iframes
	// + open shadow roots + the page's main document, with dedup,
	// occlusion, and type-weighted ranking baked in).
	var raw json.RawMessage
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(som.EnumScript, &raw)); err != nil {
		return nil, err
	}
	jsEls, err := som.UnmarshalElements(raw)
	if err != nil {
		return nil, err
	}

	// Complement: AX tree walk via CDP. Crosses CLOSED shadow DOM
	// boundaries (Transcend cookie banners, OneTrust, browser
	// internal UIs) that injected JS fundamentally cannot reach.
	// Errors swallowed inside enumerateViaAX — never a failure path.
	axEls := c.enumerateViaAX()
	if len(axEls) == 0 {
		return jsEls, nil
	}
	return mergeAXIntoJS(jsEls, axEls), nil
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
	if c.ctx == nil {
		return ""
	}
	var url string
	_ = chromedp.Run(c.ctx, chromedp.Location(&url))
	return url
}

// gracefulCancel runs chromedp.Cancel(ctx) in a goroutine with a
// recover guard + 3s timeout. Used by Close and the relaunch paths.
//
// Two failure modes we have to tolerate:
//   - Cancel can take time (sends Browser.close over CDP, waits for
//     graceful exit so cookies + IndexedDB flush to disk). 3s cap.
//   - Cancel can PANIC with "close of closed channel" if the
//     underlying Browser's closingGracefully chan was already closed
//     elsewhere (an upstream context cancellation, the Browser
//     process dying mid-call, etc.). Recover keeps the panic from
//     crashing the program — the hard-cancel that runs after
//     gracefulCancel still fully cleans up.
func (c *Computer) gracefulCancel() {
	if c.ctx == nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() {
		defer func() {
			_ = recover()
			done <- struct{}{}
		}()
		_ = chromedp.Cancel(c.ctx)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		// Cancel hung — fall through; cancel()/allocCancel() handle the rest.
	}
}

// Close shuts Chrome down. Tries graceful shutdown first via
// chromedp.Cancel (sends Browser.close over CDP, lets Chrome flush
// cookies / IndexedDB to disk before exiting) — load-bearing for
// persistent contexts, where SIGKILL would lose any state still in
// the cookie write journal. Always falls through to a hard cancel.
func (c *Computer) Close() error {
	c.gracefulCancel()
	if c.cancel != nil {
		c.cancel()
	}
	if c.allocCancel != nil {
		c.allocCancel()
	}
	return nil
}

// gracefulTeardownForRelaunch is the same shutdown sequence Close
// runs, but inlined for the relaunch paths so they get cookie-flush
// behaviour identical to Close. Called from relaunchIfContextChanged
// (load-bearing — context switches must flush) and from
// relaunchIfProxyChanged (less critical but matches user expectations).
func (c *Computer) gracefulTeardownForRelaunch() {
	c.gracefulCancel()
	if c.cancel != nil {
		c.cancel()
	}
	if c.allocCancel != nil {
		c.allocCancel()
	}
}
