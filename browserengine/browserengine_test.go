package browserengine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

// requireBrowserEngine skips unless BROWSER_API_KEY is set. Returns
// (apiKey, baseURL). BROWSER_API_URL overrides the default cloud
// endpoint — useful when iterating against a local browser-service
// deployment without burning real-cloud minutes.
func requireBrowserEngine(t *testing.T) (string, string) {
	t.Helper()
	apiKey := os.Getenv("BROWSER_API_KEY")
	if apiKey == "" {
		t.Skip("BROWSER_API_KEY not set")
	}
	base := os.Getenv("BROWSER_API_URL")
	if base == "" {
		base = "https://api.browserengine.co"
	}
	return apiKey, base
}

// createContext POSTs to {base}/contexts and returns the new context id.
// Mirrors the shape `functions/browser/context-create` writes back.
func createContext(t *testing.T, apiKey, base string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":     fmt.Sprintf("computer-test-%d", time.Now().UnixNano()),
		"metadata": map[string]any{"source": "computer.browserengine.test"},
	})
	req, _ := http.NewRequest("POST", base+"/contexts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create context: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create context: HTTP %d: %s", resp.StatusCode, string(b))
	}
	var got struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode create context: %v", err)
	}
	if got.Data.ID == "" {
		t.Fatal("create context: no id in response")
	}
	return got.Data.ID
}

func deleteContext(t *testing.T, apiKey, base, id string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", base+"/contexts/"+id, nil)
	req.Header.Set("x-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("delete context %s: %v", id, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Logf("delete context %s: HTTP %d", id, resp.StatusCode)
	}
}

// targetURL is the stable public origin we navigate to. Browser Engine
// workers run remotely, so we can't use httptest (127.0.0.1 unreachable
// from cloud workers). example.com is universally routable, returns a
// trivially small page, and is a well-defined origin so localStorage
// has a stable scope.
const targetURL = "https://example.com/"

// TestContextRoundtrip exercises the full persistent-context cycle:
//
//  1. Create a Browser Engine context via the API (operator step).
//  2. OpenSession bound to the context, navigate to example.com,
//     write a persistent cookie (with explicit Expires) on the
//     example.com origin.
//  3. Close the session. Browser Engine snapshots the user-data-dir
//     volume back into the context.
//  4. OpenSession on a fresh Computer with the SAME context_id, hit
//     the same URL, read document.cookie. Expect the same value.
//  5. Confirm the two session ids differ — a single sticky session
//     would pass cookie checks trivially and tell us nothing.
//
// Why persistent cookies, not localStorage: Chrome writes cookies
// with explicit Expires/Max-Age synchronously to the Cookies SQLite
// inside the profile dir, so the volume snapshot at close time is
// guaranteed to capture them. localStorage flushes to its LevelDB
// store on a periodic timer — empirically the writes are still in
// memory at the moment Browser Engine takes the snapshot, so they
// don't survive the round-trip. Cookies are the reliable signal.
//
//	BROWSER_API_KEY=be_... go test -v -run TestContextRoundtrip ./...
func TestContextRoundtrip(t *testing.T) {
	apiKey, base := requireBrowserEngine(t)

	ctxID := createContext(t, apiKey, base)
	t.Logf("created context %s", ctxID)
	defer deleteContext(t, apiKey, base, ctxID)

	display := computer.DisplaySize{Width: 1280, Height: 720}
	cookieName := "apteva_ctx_test"
	cookieValue := fmt.Sprintf("v-%d", time.Now().UnixNano())

	// ---- Run 1 — bind context, write persistent cookie, close ----
	comp1, err := NewWithOptions(apiKey, display, Options{BaseURL: base, Timeout: 600})
	if err != nil {
		t.Fatalf("comp1 New: %v", err)
	}
	// Belt-and-suspenders cleanup: if the test fails before the explicit
	// Close at the bottom, this releases the session so the context lock
	// drops and the deferred deleteContext can succeed instead of 409ing.
	comp1Closed := false
	t.Cleanup(func() {
		if !comp1Closed {
			_ = comp1.Close()
		}
	})
	if err := comp1.OpenSession(computer.OpenOptions{
		URL: targetURL, ContextID: ctxID, Persist: true, Timeout: 300,
	}); err != nil {
		t.Fatalf("comp1 OpenSession: %v", err)
	}
	sess1 := comp1.SessionID()
	t.Logf("session1 id=%s context=%s url=%s", sess1, comp1.ContextID(), comp1.CurrentURL())
	if !strings.Contains(comp1.CurrentURL(), "example.com") {
		t.Fatalf("comp1 did not land on example.com (got %q)", comp1.CurrentURL())
	}

	// document.cookie returns "" when no cookies are scoped to this
	// origin — so the post-write read is a definitive sanity check.
	// Expires is set to a fixed far-future date so Chrome treats this
	// as a persistent cookie (written to the on-disk Cookies SQLite,
	// not held in memory like a session cookie).
	setJS := fmt.Sprintf(
		`(()=>{document.cookie=%q; return document.cookie})()`,
		fmt.Sprintf("%s=%s; expires=Sun, 01 Jan 2099 00:00:00 GMT; path=/", cookieName, cookieValue),
	)
	var setEcho string
	if err := chromedp.Run(comp1.ctx, chromedp.Evaluate(setJS, &setEcho)); err != nil {
		t.Fatalf("comp1 set cookie: %v", err)
	}
	expectedSubstr := cookieName + "=" + cookieValue
	if !strings.Contains(setEcho, expectedSubstr) {
		t.Fatalf("cookie write didn't echo back: got %q, want substring %q", setEcho, expectedSubstr)
	}
	t.Logf("✓ wrote cookie %s=%s under context", cookieName, cookieValue)

	if err := comp1.Close(); err != nil {
		t.Fatalf("comp1 Close: %v", err)
	}
	comp1Closed = true

	// Browser Engine snapshots the user-data-dir on close. Volume-sync
	// can lag a beat; Browserbase docs note "wait a few seconds." Be
	// generous to keep the test from going flaky in CI.
	time.Sleep(3 * time.Second)

	// ---- Run 2 — fresh session, same context, read localStorage ----
	comp2, err := NewWithOptions(apiKey, display, Options{BaseURL: base, Timeout: 600})
	if err != nil {
		t.Fatalf("comp2 New: %v", err)
	}
	defer comp2.Close()
	if err := comp2.OpenSession(computer.OpenOptions{
		URL: targetURL, ContextID: ctxID, Persist: true, Timeout: 300,
	}); err != nil {
		t.Fatalf("comp2 OpenSession: %v", err)
	}
	sess2 := comp2.SessionID()
	t.Logf("session2 id=%s context=%s url=%s", sess2, comp2.ContextID(), comp2.CurrentURL())

	if sess1 == sess2 {
		t.Fatalf("sessions not distinct: both reported %s", sess1)
	}
	if comp2.ContextID() != ctxID {
		t.Fatalf("ContextID() returned %q, want %q", comp2.ContextID(), ctxID)
	}

	var got string
	if err := chromedp.Run(comp2.ctx, chromedp.Evaluate(`document.cookie`, &got)); err != nil {
		t.Fatalf("comp2 read cookie: %v", err)
	}
	if !strings.Contains(got, expectedSubstr) {
		t.Fatalf("cookie did NOT persist across context: got %q, want substring %q (sess1=%s sess2=%s ctx=%s)",
			got, expectedSubstr, sess1, sess2, ctxID)
	}
	t.Logf("✓ cookie persisted across two distinct sessions via context: %s", got)
}

// TestLiveReattach proves a still-running session can be picked up by
// a fresh Computer when handed its session_id. We:
//
//  1. Open a session and navigate to example.com.
//  2. Drop the CDP transport WITHOUT calling Close — the backend
//     session stays alive.
//  3. New Computer attaches via OpenSession({SessionID}). Backend's
//     GET /sessions/{id} returns the live connect_url; we reattach.
//  4. Confirm the attached session has the same id and the URL is
//     preserved (live reattach, not a fresh navigation).
//
// No context here — purely the session-id attach codepath.
func TestLiveReattach(t *testing.T) {
	apiKey, base := requireBrowserEngine(t)

	display := computer.DisplaySize{Width: 1280, Height: 720}

	comp1, err := NewWithOptions(apiKey, display, Options{BaseURL: base, Timeout: 600})
	if err != nil {
		t.Fatalf("comp1 New: %v", err)
	}
	if err := comp1.OpenSession(computer.OpenOptions{URL: targetURL, Timeout: 300}); err != nil {
		t.Fatalf("comp1 OpenSession: %v", err)
	}
	sess1 := comp1.SessionID()
	url1 := comp1.CurrentURL()
	t.Logf("session1 id=%s url=%s", sess1, url1)
	if !strings.Contains(url1, "example.com") {
		t.Fatalf("comp1 did not land on example.com (got %q)", url1)
	}

	// Drop CDP only — DO NOT call Close(), which would call
	// requestRelease and terminate the backend session. Same-package
	// access lets us call the unexported helper directly.
	comp1.releaseCDP()

	// Fresh Computer attaches to the still-running session.
	comp2, err := NewWithOptions(apiKey, display, Options{BaseURL: base})
	if err != nil {
		t.Fatalf("comp2 New: %v", err)
	}
	// comp2.Close() at the end releases the backend session — cleans
	// up after both Computers, since they shared sess1.
	defer comp2.Close()

	if err := comp2.OpenSession(computer.OpenOptions{SessionID: sess1}); err != nil {
		t.Fatalf("comp2 OpenSession (attach): %v", err)
	}
	if comp2.SessionID() != sess1 {
		t.Fatalf("attach changed session id: got %s, want %s", comp2.SessionID(), sess1)
	}
	if !strings.Contains(comp2.CurrentURL(), "example.com") {
		t.Fatalf("attached session lost URL: got %q (expected example.com)", comp2.CurrentURL())
	}
	t.Logf("✓ live reattach preserved url=%s", comp2.CurrentURL())
}
