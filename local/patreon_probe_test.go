package local

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

// TestPatreonProbe_Local navigates a local Chrome to Patreon's login
// page and reports what the page actually serves — bot-gate vs real
// login form. No LLM, no agent loop; this exists to give a fast,
// deterministic answer to the question "what does our local browser
// see when it hits Patreon" so we can decide if a residential proxy
// is required at all.
//
// Output side-effects:
//   - /tmp/patreon_local_probe.png — full screenshot of the landed page
//   - test log lines for: final URL, page title, presence/absence of
//     the Cloudflare challenge widget, presence/absence of the email
//     input. These are the four signals we care about.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestPatreonProbe_Local -timeout 60s ./local
func TestPatreonProbe_Local(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome and hits patreon.com)")
	}

	display := computer.DisplaySize{Width: 1600, Height: 900}
	comp, err := New(display)
	if err != nil {
		t.Fatalf("failed to create browser: %v", err)
	}
	defer comp.Close()

	// Navigate. Patreon's login URL is the target — same one the agent
	// gets directed at in core/computer_patreon_test.go.
	const target = "https://www.patreon.com/login"
	if _, err := comp.Execute(computer.Action{Type: "navigate", URL: target}); err != nil {
		t.Fatalf("navigate %s: %v", target, err)
	}

	// Give the page a moment to settle (Cloudflare may inject its
	// challenge after a redirect; React shells take a tick to render
	// the email input). 3s is enough on warm Chrome.
	time.Sleep(3 * time.Second)

	// Capture page-level signals. Run them all under one chromedp.Run
	// so a single CDP round-trip pulls everything we need.
	var (
		finalURL      string
		pageTitle     string
		cfChallenge   bool
		emailInput    bool
		challengeText string
	)
	probeCtx, cancel := context.WithTimeout(comp.ctx, 10*time.Second)
	defer cancel()
	err = chromedp.Run(probeCtx,
		chromedp.Location(&finalURL),
		chromedp.Title(&pageTitle),
		// Cloudflare's interstitial signals: any of (a) "Ray ID" footer,
		// (b) "Performance and Security by Cloudflare", (c) the older
		// "Verify you are human" / "Checking your browser" copy, or
		// (d) the challenge container DOM. Title-only "Just a
		// moment..." is also a strong tell since real Patreon uses
		// "Log in | Patreon".
		chromedp.Evaluate(`(() => {
			const t = document.body ? (document.body.innerText || '') : '';
			const title = document.title || '';
			return /Ray ID:|Performance and Security by Cloudflare|Performing security verification|Verify you are human|Checking your browser/i.test(t)
				|| /^Just a moment/i.test(title)
				|| !!document.querySelector('#challenge-stage, #cf-wrapper, [data-translate="checking_browser"]');
		})()`, &cfChallenge),
		// Patreon's login form input has name="email" or type="email".
		chromedp.Evaluate(`!!document.querySelector('input[name="email"], input[type="email"]')`, &emailInput),
		// Capture a snippet of body text either way for diagnosis.
		chromedp.Evaluate(`(document.body ? (document.body.innerText || '') : '').slice(0, 400)`, &challengeText),
	)
	if err != nil {
		t.Fatalf("page probe failed: %v", err)
	}

	// Take a screenshot for visual inspection.
	shot, err := comp.Screenshot()
	if err != nil {
		t.Fatalf("screenshot failed: %v", err)
	}
	const shotPath = "/tmp/patreon_local_probe.png"
	if err := os.WriteFile(shotPath, shot, 0644); err != nil {
		t.Fatalf("write screenshot %s: %v", shotPath, err)
	}

	t.Logf("=== Patreon local probe ===")
	t.Logf("final URL:        %s", finalURL)
	t.Logf("page title:       %s", pageTitle)
	t.Logf("Cloudflare gate:  %v", cfChallenge)
	t.Logf("email input:      %v", emailInput)
	t.Logf("screenshot:       %s (%d bytes)", shotPath, len(shot))
	t.Logf("body snippet:     %s", strings.TrimSpace(challengeText))

	// We don't fail the test on either outcome — the goal is to
	// REPORT, not assert. But we make the verdict obvious in one
	// line so a human (or grep) can pull it out fast.
	verdict := "UNKNOWN — neither gate nor email input detected; check screenshot"
	switch {
	case cfChallenge && !emailInput:
		verdict = "BOT-GATED — Cloudflare challenge served, login form not reached"
	case emailInput && !cfChallenge:
		verdict = "OPEN — login form reached, no gate"
	case emailInput && cfChallenge:
		verdict = "MIXED — both gate text and email input present (gate may be passive/script-based)"
	}
	t.Logf("VERDICT: %s", verdict)
	fmt.Fprintf(os.Stderr, "[PATREON_PROBE] %s\n", verdict)
}
