package local

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// TestContext_PatreonLoginPersists drives a real Patreon login via
// chromedp (no LLM) to prove that the local backend's persistent
// context actually retains a real-world session.
//
// Two phases in one test process:
//
//	Phase 1 — open with a fresh context_id, fill the login form,
//	          confirm we land on /home (or any logged-in URL).
//	          Close gracefully so cookies flush to disk.
//
//	Phase 2 — open a brand-new Computer with the SAME context_id,
//	          navigate straight to /home, confirm we land there
//	          WITHOUT being redirected to /login. Stronger signal
//	          than a cookie inspection because it proves the full
//	          session (cookies + any localStorage Patreon checks)
//	          survived.
//
// Hermetic: uses t.TempDir() for the context base and t.Cleanup()
// to wipe. Set APTEVA_LOCAL_CONTEXT_DIR_KEEP=1 to override this and
// keep the directory after the test for manual inspection / a second
// run.
//
//	RUN_BROWSER_TEST=1 PATREON_EMAIL=… PATREON_PASSWORD=… \
//	  go test -v -run TestContext_PatreonLoginPersists -timeout 5m ./local
func TestContext_PatreonLoginPersists(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (real Patreon login, launches Chrome)")
	}
	email := os.Getenv("PATREON_EMAIL")
	password := os.Getenv("PATREON_PASSWORD")
	if email == "" || password == "" {
		t.Skip("PATREON_EMAIL + PATREON_PASSWORD required")
	}

	// Hermetic context base unless the operator explicitly wants it
	// kept across runs.
	if os.Getenv("APTEVA_LOCAL_CONTEXT_DIR_KEEP") == "" {
		t.Setenv("APTEVA_LOCAL_CONTEXT_DIR", t.TempDir())
	}
	const ctxID = "patreon-persist-test"

	// ─── Phase 1 ─────────────────────────────────────────────────
	t.Log("=== Phase 1: fresh login + persist ===")
	c1, err := New(computer.DisplaySize{Width: 1280, Height: 900})
	if err != nil {
		t.Fatalf("New (phase 1): %v", err)
	}
	if err := c1.OpenSession(computer.OpenOptions{ContextID: ctxID, URL: "https://www.patreon.com/login"}); err != nil {
		c1.Close()
		t.Fatalf("OpenSession login (phase 1): %v", err)
	}
	if err := patreonLoginFormFill(t, c1, email, password); err != nil {
		c1.Close()
		t.Fatalf("phase 1 form fill: %v", err)
	}
	loggedIn1, finalURL1 := patreonIsLoggedIn(t, c1)
	t.Logf("phase 1 final URL: %s (logged-in detected: %v)", finalURL1, loggedIn1)
	if !loggedIn1 {
		c1.Close()
		t.Fatalf("phase 1 FAIL: form-fill did not result in a logged-in state (URL %q)", finalURL1)
	}
	cookieCount1 := countPatreonCookies(t, c1)
	t.Logf("✓ phase 1 logged in — %d patreon.com cookies in jar", cookieCount1)
	// Graceful Close — this is what flushes cookies to disk under the
	// context dir. The local backend now does chromedp.Cancel before
	// the hard cancel for exactly this case.
	c1.Close()

	// ─── Phase 2 ─────────────────────────────────────────────────
	t.Log("=== Phase 2: fresh Computer, same context_id, expect already-logged-in ===")
	c2, err := New(computer.DisplaySize{Width: 1280, Height: 900})
	if err != nil {
		t.Fatalf("New (phase 2): %v", err)
	}
	defer c2.Close()
	// Open with the SAME context_id and navigate straight to /home.
	// If persistence works, Patreon serves /home directly. If it
	// doesn't, Patreon redirects to /login.
	if err := c2.OpenSession(computer.OpenOptions{ContextID: ctxID, URL: "https://www.patreon.com/home"}); err != nil {
		t.Fatalf("OpenSession /home (phase 2): %v", err)
	}
	loggedIn2, finalURL2 := patreonIsLoggedIn(t, c2)
	cookieCount2 := countPatreonCookies(t, c2)
	t.Logf("phase 2 final URL: %s (logged-in: %v) — %d patreon.com cookies", finalURL2, loggedIn2, cookieCount2)

	if strings.Contains(finalURL2, "/login") {
		t.Fatalf("phase 2 FAIL: navigating to /home redirected to /login — context did NOT restore the session (URL %q)", finalURL2)
	}
	if !loggedIn2 {
		t.Fatalf("phase 2 FAIL: did not land on a logged-in page (URL %q)", finalURL2)
	}
	if cookieCount2 == 0 {
		t.Fatalf("phase 2 FAIL: zero patreon.com cookies in jar — context dir wasn't loaded?")
	}
	t.Log("✓ phase 2 PASS: same context_id resumed an already-logged-in Patreon session — local persistence works end-to-end")
}

// patreonLoginFormFill scripts the email + password + submit flow on
// patreon.com/login WITHOUT an LLM. Selectors are kept loose: any
// input[type=email] / input[type=password] / button matching common
// labels. Patreon may A/B-test the form — if this breaks, the
// failing assertion shows which step couldn't find its element.
func patreonLoginFormFill(t *testing.T, c *Computer, email, password string) error {
	t.Helper()

	// Patreon shows a GDPR cookie banner on first visit. Try to
	// dismiss it (best-effort — if the button isn't there, ignore).
	dismissCookieBanner(c)

	// Email step.
	if err := chromedp.Run(c.ctx,
		chromedp.WaitVisible(`input[type="email"]`, chromedp.ByQuery),
		chromedp.SendKeys(`input[type="email"]`, email, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("type email: %w", err)
	}
	t.Logf("typed email")

	// Click whichever submit-style button is on the page after typing
	// the email. Patreon's button text alternates ("Continue" /
	// "Log in"); we click the FIRST visible button[type=submit].
	if err := chromedp.Run(c.ctx,
		chromedp.WaitVisible(`button[type="submit"]`, chromedp.ByQuery),
		chromedp.Click(`button[type="submit"]`, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("click after-email submit: %w", err)
	}
	t.Logf("clicked Continue")

	// Password step. Patreon may skip this if the device is trusted —
	// try, but tolerate "no password field appeared" as a soft success
	// (we'll check loggedIn via URL state at the call site).
	pwCtx, pwCancel := context.WithTimeout(c.ctx, 8*time.Second)
	defer pwCancel()
	if err := chromedp.Run(pwCtx,
		chromedp.WaitVisible(`input[type="password"]`, chromedp.ByQuery),
		chromedp.SendKeys(`input[type="password"]`, password, chromedp.ByQuery),
		chromedp.Click(`button[type="submit"]`, chromedp.ByQuery),
	); err != nil {
		t.Logf("password step skipped or failed (may be a trusted-device shortcut): %v", err)
	} else {
		t.Logf("typed password + clicked Log in")
	}

	// Give Patreon a few seconds to land on /home or wherever it's
	// going. We don't poll — the URL check at the call site is the
	// real assertion.
	time.Sleep(4 * time.Second)
	return nil
}

// dismissCookieBanner tries common selectors for Patreon's GDPR
// banner accept button. Best-effort, errors swallowed — the test
// proceeds either way.
func dismissCookieBanner(c *Computer) {
	timeout, cancel := context.WithTimeout(c.ctx, 3*time.Second)
	defer cancel()
	selectors := []string{
		`button[data-testid="accept-cookies"]`,
		`button[aria-label*="Accept"]`,
		`button[id*="accept"]`,
	}
	for _, sel := range selectors {
		if err := chromedp.Run(timeout,
			chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible),
		); err == nil {
			return // dismissed something
		}
	}
}

// patreonIsLoggedIn returns true if the live URL is one of Patreon's
// post-login pages. Tolerant of the various landing pages Patreon
// uses (/home, /c/<creator>, /create/profile, etc.). Anything that
// contains "/login" or "/signup" or is /posts/new style is treated
// as not-logged-in for the purposes of this test.
func patreonIsLoggedIn(t *testing.T, c *Computer) (bool, string) {
	t.Helper()
	var url string
	if err := chromedp.Run(c.ctx, chromedp.Location(&url)); err != nil {
		return false, ""
	}
	low := strings.ToLower(url)
	if strings.Contains(low, "/login") || strings.Contains(low, "/signup") {
		return false, url
	}
	// Anything that's on patreon.com and isn't an auth page counts —
	// /home, /c/<creator>, /create/profile, etc.
	return strings.Contains(low, "patreon.com"), url
}

// countPatreonCookies asks Chrome (via CDP) for cookies scoped to
// any patreon.com origin. Used as a secondary signal: a logged-in
// session should have several (session_id, csrf, etc.); a fresh
// jar has none.
func countPatreonCookies(t *testing.T, c *Computer) int {
	t.Helper()
	var n int
	err := chromedp.Run(c.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		cookies, err := network.GetCookies().WithURLs([]string{"https://www.patreon.com/"}).Do(ctx)
		if err != nil {
			return err
		}
		n = len(cookies)
		return nil
	}))
	if err != nil {
		t.Logf("cookie count failed: %v", err)
		return 0
	}
	return n
}
