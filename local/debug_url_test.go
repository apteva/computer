package local

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
)

// TestDebugURL_PopulatedAndReachable verifies the DebugURL pipeline
// end-to-end: launch Chrome with a pinned debug port, fetch the page
// target's devtoolsFrontendUrl from /json, store it on the Computer,
// and (importantly) confirm Chrome actually serves the URL — a
// non-empty string that 404s would still pass a naive existence check
// while leaving the live-view feature broken.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestDebugURL_PopulatedAndReachable -timeout 30s ./local
func TestDebugURL_PopulatedAndReachable(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}

	display := computer.DisplaySize{Width: 1024, Height: 768}
	comp, err := New(display)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer comp.Close()

	url := comp.DebugURL()
	if url == "" {
		t.Fatal("DebugURL() returned empty string — fetch failed during launch")
	}
	t.Logf("debug URL: %s", url)

	// Chrome serves the DevTools frontend either inline at
	// /devtools/inspector.html or as an absolute https URL pointing
	// at chrome-devtools-frontend.appspot.com. Both forms include
	// the WS path as a ?ws=... query — that's the load-bearing part
	// for live-view usage. Sanity-check it's there.
	if !strings.Contains(url, "ws=") {
		t.Errorf("DebugURL is missing ws= query param — devtools wouldn't be able to attach: %s", url)
	}

	// Reachability check. Some forms of the URL are absolute
	// (https://chrome-devtools-frontend.appspot.com/...), so a
	// blanket GET would pull from the public internet. We only fetch
	// when it's a relative-ish URL pointing at our local Chrome.
	if strings.HasPrefix(url, "http://127.0.0.1:") || strings.HasPrefix(url, "http://localhost:") {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET %s failed: %v", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("GET %s returned %d, want 200. body: %s", url, resp.StatusCode, string(body))
		}
	} else {
		t.Logf("DebugURL is absolute (devtools-frontend hosted) — skipping local reachability fetch")
	}
}

// TestDebugURL_PortNotShared verifies that two concurrent Computers
// each get their own debug port and their own distinct URL — the
// pickFreePort helper would silently break this if it returned the
// same port twice.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestDebugURL_PortNotShared -timeout 60s ./local
func TestDebugURL_PortNotShared(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches two Chromes)")
	}

	display := computer.DisplaySize{Width: 800, Height: 600}
	a, err := New(display)
	if err != nil {
		t.Fatalf("New(a): %v", err)
	}
	defer a.Close()

	b, err := New(display)
	if err != nil {
		t.Fatalf("New(b): %v", err)
	}
	defer b.Close()

	ua, ub := a.DebugURL(), b.DebugURL()
	t.Logf("computer A: %s", ua)
	t.Logf("computer B: %s", ub)

	if ua == "" || ub == "" {
		t.Fatalf("expected both DebugURLs populated; got a=%q b=%q", ua, ub)
	}
	if ua == ub {
		t.Errorf("two Computers ended up with the same DebugURL — port allocation collided")
	}
}
