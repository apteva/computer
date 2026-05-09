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

// TestStealthProbe_Sannysoft hits bot.sannysoft.com, the canonical
// public probe for headless/automation tells. The page renders a table
// of checks (WebDriver, Chrome, Permissions, Plugins Length, Languages,
// WebGL Vendor, WebGL Renderer, ...) with row classes "passed"/"failed".
// We scrape those rows and report — passing means the stealth script
// patched the signal, failing means it didn't.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestStealthProbe_Sannysoft -timeout 60s ./local
func TestStealthProbe_Sannysoft(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome and hits bot.sannysoft.com)")
	}

	display := computer.DisplaySize{Width: 1280, Height: 900}
	comp, err := New(display)
	if err != nil {
		t.Fatalf("failed to create browser: %v", err)
	}
	defer comp.Close()

	const target = "https://bot.sannysoft.com/"
	if _, err := comp.Execute(computer.Action{Type: "navigate", URL: target}); err != nil {
		t.Fatalf("navigate %s: %v", target, err)
	}
	time.Sleep(3 * time.Second)

	probeCtx, cancel := context.WithTimeout(comp.ctx, 10*time.Second)
	defer cancel()

	var rowsJSON string
	err = chromedp.Run(probeCtx,
		chromedp.Evaluate(`(() => {
			const rows = Array.from(document.querySelectorAll('table tr'));
			const out = [];
			for (const r of rows) {
				const cells = r.querySelectorAll('td');
				if (cells.length < 2) continue;
				const name = (cells[0].innerText || '').trim();
				const value = (cells[1].innerText || '').trim();
				const cls = (cells[1].className || '').trim();
				out.push({ name, value, cls });
			}
			return JSON.stringify(out);
		})()`, &rowsJSON),
	)
	if err != nil {
		t.Fatalf("page probe failed: %v", err)
	}

	shot, err := comp.Screenshot()
	if err != nil {
		t.Fatalf("screenshot failed: %v", err)
	}
	const shotPath = "/tmp/stealth_sannysoft_probe.png"
	if err := os.WriteFile(shotPath, shot, 0644); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}

	var passed, failed int
	for _, line := range strings.Split(rowsJSON, "},{") {
		switch {
		case strings.Contains(line, `"cls":"passed"`):
			passed++
		case strings.Contains(line, `"cls":"failed"`):
			failed++
		}
	}

	t.Logf("=== Sannysoft stealth probe ===")
	t.Logf("rows passed: %d", passed)
	t.Logf("rows failed: %d", failed)
	t.Logf("screenshot:  %s (%d bytes)", shotPath, len(shot))
	t.Logf("raw rows:    %s", rowsJSON)
	fmt.Fprintf(os.Stderr, "[STEALTH_SANNYSOFT] passed=%d failed=%d\n", passed, failed)
}

// TestStealthProbe_AreYouHeadless hits arh.antoinevastel.com's
// "are you headless" probe, which renders one of two strings:
//   - "You are not Chrome headless" (good — stealth or genuine head)
//   - "You are Chrome headless"     (bad — automation detected)
//
//	RUN_BROWSER_TEST=1 go test -v -run TestStealthProbe_AreYouHeadless -timeout 60s ./local
func TestStealthProbe_AreYouHeadless(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome and hits arh.antoinevastel.com)")
	}

	display := computer.DisplaySize{Width: 1280, Height: 900}
	comp, err := New(display)
	if err != nil {
		t.Fatalf("failed to create browser: %v", err)
	}
	defer comp.Close()

	const target = "https://arh.antoinevastel.com/bots/areyouheadless"
	if _, err := comp.Execute(computer.Action{Type: "navigate", URL: target}); err != nil {
		t.Fatalf("navigate %s: %v", target, err)
	}
	time.Sleep(3 * time.Second)

	probeCtx, cancel := context.WithTimeout(comp.ctx, 10*time.Second)
	defer cancel()

	var bodyText string
	err = chromedp.Run(probeCtx,
		chromedp.Evaluate(`(document.body ? (document.body.innerText || '') : '').slice(0, 600)`, &bodyText),
	)
	if err != nil {
		t.Fatalf("page probe failed: %v", err)
	}

	shot, err := comp.Screenshot()
	if err != nil {
		t.Fatalf("screenshot failed: %v", err)
	}
	const shotPath = "/tmp/stealth_areyouheadless_probe.png"
	if err := os.WriteFile(shotPath, shot, 0644); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}

	verdict := "UNKNOWN — neither verdict string found"
	switch {
	case strings.Contains(bodyText, "You are not Chrome headless"):
		verdict = "PASS — stealth holds, page reports not headless"
	case strings.Contains(bodyText, "You are Chrome headless"):
		verdict = "FAIL — page detected Chrome headless"
	}

	t.Logf("=== AreYouHeadless stealth probe ===")
	t.Logf("screenshot: %s (%d bytes)", shotPath, len(shot))
	t.Logf("body:       %s", strings.TrimSpace(bodyText))
	t.Logf("VERDICT: %s", verdict)
	fmt.Fprintf(os.Stderr, "[STEALTH_AREYOUHEADLESS] %s\n", verdict)
}
