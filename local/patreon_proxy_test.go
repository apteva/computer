package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

// DataImpulse credentials for this test. Hard-coded so the test runs
// out of the box on this dev machine; override the whole URL with
// PATREON_PROXY_URL to point at a different proxy.
const (
	dataImpulseUser = "cce5701490a7d414689c"
	dataImpulsePass = "2ae7e7c069c1ef1b"
	dataImpulseHost = "gw.dataimpulse.com:823"
)

// buildDataImpulseProxyURL produces a sticky-session, country-pinned
// DataImpulse proxy URL. Username convention (verified against
// gw.dataimpulse.com:823 by probing): "__cr.<cc>" pins the exit
// country and "__sessid.<id>" pins the exit IP across consecutive
// requests sharing that id. Other syntaxes seen in proxy-provider
// docs ("-country-us", "__sid.X", "__sessionid.X") silently fall
// through on this gateway — auth either rejects or stickiness is
// ignored — so don't use them here.
//
// Both knobs are load-bearing for Patreon: random rotating exits
// get freshly flagged by Cloudflare every request, and non-US exits
// hit geo-blocks more often than US ones. The session id is random
// per test invocation so a previously-flagged exit doesn't re-poison
// subsequent runs.
func buildDataImpulseProxyURL(country, sessionID string) string {
	if sessionID == "" {
		var b [4]byte
		_, _ = rand.Read(b[:])
		sessionID = hex.EncodeToString(b[:])
	}
	username := dataImpulseUser
	if country != "" {
		username += "__cr." + country
	}
	username += "__sessid." + sessionID
	return fmt.Sprintf("http://%s:%s@%s", username, dataImpulsePass, dataImpulseHost)
}

// TestPatreonProbe_LocalProxy is the proxy-on counterpart to
// TestPatreonProbe_Local. Same probe shape, same target, but Chrome
// launches through the configured residential proxy via the new
// agent-controlled OpenSession(Proxy:&true) path. It exists to give
// a fast, reproducible answer to the question "does egressing through
// a residential exit actually bypass the Cloudflare gate?" without
// running the full agent loop and burning LLM tokens.
//
// Expected verdict on a working proxy: OPEN — the login form's email
// input renders. If verdict is BOT-GATED, the gate fired anyway
// (likely the proxy's exit IP is itself flagged or geo-blocked);
// inspect the screenshot for what was served.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestPatreonProbe_LocalProxy -timeout 60s ./local
func TestPatreonProbe_LocalProxy(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome and hits patreon.com via a paid proxy)")
	}

	// Default to a US sticky session so the run gets one stable
	// residential IP from the US pool. Random session id per run
	// avoids sticking to a previously-flagged exit. PATREON_PROXY_URL
	// overrides for ad-hoc experiments.
	proxyURL := os.Getenv("PATREON_PROXY_URL")
	if proxyURL == "" {
		proxyURL = buildDataImpulseProxyURL("us", "")
	}

	display := computer.DisplaySize{Width: 1600, Height: 900}
	comp, err := NewWithOptions(display, Options{ProxyURL: proxyURL})
	if err != nil {
		t.Fatalf("NewWithOptions (proxy): %v", err)
	}
	defer comp.Close()

	// Flip proxy on for this session (agent-equivalent call). This
	// triggers the relaunch path: the eager Chrome was direct; this
	// tears it down and brings it back up with --proxy-server +
	// CDP auth handler.
	wantProxy := true
	const target = "https://www.patreon.com/login"
	if err := comp.OpenSession(computer.OpenOptions{Proxy: &wantProxy, URL: target}); err != nil {
		t.Fatalf("OpenSession(proxy=true, url=%s): %v", target, err)
	}
	if !comp.proxyActive {
		t.Fatalf("proxyActive should be true after OpenSession(Proxy:&true)")
	}

	// Same settle delay as the no-proxy probe — Cloudflare's JS
	// challenge runs ~3s before resolving. Residential proxies add
	// 200–800ms of round-trip on top, but 3s still covers it.
	time.Sleep(3 * time.Second)

	var (
		finalURL      string
		pageTitle     string
		cfChallenge   bool
		emailInput    bool
		challengeText string
		exitIP        string // sanity: prove which IP we egressed from
	)
	probeCtx, cancel := context.WithTimeout(comp.ctx, 15*time.Second)
	defer cancel()
	err = chromedp.Run(probeCtx,
		chromedp.Location(&finalURL),
		chromedp.Title(&pageTitle),
		chromedp.Evaluate(`(() => {
			const t = document.body ? (document.body.innerText || '') : '';
			const title = document.title || '';
			return /Ray ID:|Performance and Security by Cloudflare|Performing security verification|Verify you are human|Checking your browser/i.test(t)
				|| /^Just a moment/i.test(title)
				|| !!document.querySelector('#challenge-stage, #cf-wrapper, [data-translate="checking_browser"]');
		})()`, &cfChallenge),
		chromedp.Evaluate(`!!document.querySelector('input[name="email"], input[type="email"]')`, &emailInput),
		chromedp.Evaluate(`(document.body ? (document.body.innerText || '') : '').slice(0, 400)`, &challengeText),
	)
	if err != nil {
		t.Fatalf("page probe failed: %v", err)
	}

	// Side-channel: hit a tiny IP-echo service through the same
	// browser context. If the proxy is engaged, this returns the
	// proxy exit IP; if Chrome is somehow still going direct,
	// it returns this dev box's address — a cheap end-to-end check
	// independent of Patreon's own response.
	ipCtx, cancelIP := context.WithTimeout(comp.ctx, 8*time.Second)
	defer cancelIP()
	_ = chromedp.Run(ipCtx,
		chromedp.Navigate("https://api.ipify.org/?format=text"),
		chromedp.Text("body", &exitIP, chromedp.NodeVisible),
	)
	exitIP = strings.TrimSpace(exitIP)

	// Re-navigate back to Patreon for the screenshot record.
	_, _ = comp.Execute(computer.Action{Type: "navigate", URL: target})
	time.Sleep(2 * time.Second)
	shot, err := comp.Screenshot()
	if err != nil {
		t.Fatalf("screenshot failed: %v", err)
	}
	const shotPath = "/tmp/patreon_local_proxy_probe.png"
	if err := os.WriteFile(shotPath, shot, 0644); err != nil {
		t.Fatalf("write screenshot %s: %v", shotPath, err)
	}

	t.Logf("=== Patreon local probe (proxy=on) ===")
	t.Logf("proxy URL:        %s", redactProxyCreds(proxyURL))
	t.Logf("exit IP:          %s", exitIP)
	t.Logf("final URL:        %s", finalURL)
	t.Logf("page title:       %s", pageTitle)
	t.Logf("Cloudflare gate:  %v", cfChallenge)
	t.Logf("email input:      %v", emailInput)
	t.Logf("screenshot:       %s (%d bytes)", shotPath, len(shot))
	t.Logf("body snippet:     %s", strings.TrimSpace(challengeText))

	verdict := "UNKNOWN — neither gate nor email input detected; check screenshot"
	switch {
	case cfChallenge && !emailInput:
		verdict = "BOT-GATED — Cloudflare challenge served via proxy (exit IP may be flagged)"
	case emailInput && !cfChallenge:
		verdict = "OPEN — login form reached through proxy, gate bypassed"
	case emailInput && cfChallenge:
		verdict = "MIXED — both gate text and email input present"
	}
	t.Logf("VERDICT: %s", verdict)
	fmt.Fprintf(os.Stderr, "[PATREON_PROXY_PROBE] %s exit_ip=%s\n", verdict, exitIP)
}

// redactProxyCreds strips inline user:pass from a proxy URL for log
// output. Doesn't bother with strict URL parsing — anything between
// "://" and the next "@" gets replaced.
func redactProxyCreds(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	rest := u[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return u
	}
	return u[:i+3] + "***:***" + rest[at:]
}
