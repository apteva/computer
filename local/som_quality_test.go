package local

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/apteva/computer/som"
	"github.com/apteva/core/pkg/computer"
)

// somTestServer serves a single page that exercises the four SoM
// quality wins:
//
//   • #wrap-input  — <div onclick> wrapping a <input> (nested
//     wrapper that should be deduped to the input)
//   • #span-in-btn — <button> containing a <span tabindex=0>
//     (decorative inner that should be deduped to the button)
//   • #occluded    — <button> covered by a fixed-position #modal
//     that visually overlaps it (occlusion-aware skip)
//   • #publish     — small <button> in the corner (should win the
//     sort over a 600x400 background <div onclick> via
//     type-weighted ranking)
//   • #bg          — large background <div onclick> (low priority,
//     should be ranked below the actual buttons)
func somTestServer(t *testing.T) (url string, stop func()) {
	t.Helper()
	const html = `<!doctype html>
<html><body style="margin:0;padding:0">
<div id="bg" onclick="" style="position:absolute;left:0;top:0;width:1000px;height:600px;background:#eee">
  bg
</div>

<div id="wrap-input" onclick="" style="position:absolute;left:50px;top:50px;width:200px;height:30px;background:#ccf">
  <input id="real-input" style="width:100%;height:100%" />
</div>

<button id="span-in-btn" style="position:absolute;left:300px;top:50px;width:120px;height:30px">
  <span id="dec-span" tabindex="0">click me</span>
</button>

<button id="occluded" style="position:absolute;left:50px;top:200px;width:120px;height:30px">
  occluded
</button>

<!-- Realistic modal: a positioned wrapper with a real interactive
     button on top. The new occlusion check only prunes when the
     topmost element is interactive — a bare decorative dimmer
     would pass-through (lenient bias). So the modal HAS to have
     a button to genuinely test occlusion-aware pruning. -->
<div style="position:fixed;left:0;top:180px;width:1000px;height:80px;background:rgba(0,0,0,0.8);z-index:99">
  <button id="modal-button" style="position:absolute;left:60px;top:25px;width:200px;height:30px">
    Modal Action
  </button>
</div>

<button id="publish" style="position:absolute;left:800px;top:50px;width:80px;height:30px">
  Publish
</button>

</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})
	srv := httptest.NewServer(mux)
	return srv.URL, srv.Close
}

// runSoMOnPage runs the same enumeration the local backend uses
// internally during Screenshot() — JS injection + AX-tree fallback,
// merged. Tests should use this instead of calling EnumScript
// directly so they exercise the full production path (including
// the closed-shadow-DOM AX fallback).
func runSoMOnPage(t *testing.T, c *Computer) []som.Element {
	t.Helper()
	els, err := c.enumerate()
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	return els
}

// hasElementMatching returns the first element whose Text contains
// the substring s. Returns zero-value + false if none.
func hasElementMatching(els []som.Element, s string) (som.Element, bool) {
	for _, e := range els {
		if containsCI(e.Text, s) {
			return e, true
		}
	}
	return som.Element{}, false
}

// containsCI is strings.Contains case-insensitive, inline to avoid
// importing strings just for this one check.
func containsCI(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	hl := []rune(haystack)
	nl := []rune(needle)
	for i := 0; i+len(nl) <= len(hl); i++ {
		match := true
		for j, r := range nl {
			a := hl[i+j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			b := r
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestSoM_DedupsWrapperAroundInput proves that <div onclick> wrapping
// an <input> emits ONE label (the input), not two.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_DedupsWrapperAroundInput -timeout 60s ./local
func TestSoM_DedupsWrapperAroundInput(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := somTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)

	// Count how many elements claim the <input>'s position. With
	// dedup working, exactly one (the input itself) — without it,
	// the wrapper div would also be there.
	inputCount := 0
	for _, e := range els {
		if e.X >= 50 && e.X <= 60 && e.Y >= 50 && e.Y <= 60 && e.Tag == "input" {
			inputCount++
		}
	}
	if inputCount != 1 {
		t.Errorf("expected exactly 1 input at (50,50), got %d (els: %+v)", inputCount, summarizeElements(els))
	}

	// And the wrapper div onclick must NOT have a label of its own.
	for _, e := range els {
		if e.Tag == "div" && e.X >= 50 && e.X <= 60 && e.Y >= 50 && e.Y <= 60 {
			t.Errorf("wrapper div around input got a label — dedup broken: %+v", e)
		}
	}
}

// TestSoM_DedupsDecorativeChildOfButton proves that a <span
// tabindex> inside a <button> doesn't get a separate label. The
// button is the right click target; the span is decorative.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_DedupsDecorativeChildOfButton -timeout 60s ./local
func TestSoM_DedupsDecorativeChildOfButton(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := somTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)

	// The "click me" button should appear; the inner span tabindex
	// should NOT also be enumerated as a separate target.
	buttonCount := 0
	spanCount := 0
	for _, e := range els {
		if e.X >= 290 && e.X <= 310 && e.Y >= 50 && e.Y <= 60 {
			if e.Tag == "button" {
				buttonCount++
			}
			if e.Tag == "span" {
				spanCount++
			}
		}
	}
	if buttonCount != 1 {
		t.Errorf("expected 1 button at (300,50), got %d (els: %+v)", buttonCount, summarizeElements(els))
	}
	if spanCount != 0 {
		t.Errorf("decorative span inside button got a label — dedup broken (count=%d)", spanCount)
	}
}

// TestSoM_OcclusionSkipsHiddenButton proves that a <button> sitting
// behind a fixed-position modal overlay does NOT get a label, even
// though the DOM still enumerates it. This is the bug that bit us
// on Patreon's GDPR overlay — agent kept clicking "through" the
// modal.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_OcclusionSkipsHiddenButton -timeout 60s ./local
func TestSoM_OcclusionSkipsHiddenButton(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := somTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)

	// #occluded is at (50, 200, 120, 30); #modal covers y=180..260,
	// full width. So center of #occluded (110, 215) is fully under
	// the modal. Should be skipped.
	for _, e := range els {
		if e.Tag == "button" && containsCI(e.Text, "occluded") {
			t.Errorf("occluded button got a label despite being under modal: %+v (els: %+v)", e, summarizeElements(els))
		}
	}

	// Sanity: the modal itself should have a label (it's a clickable
	// region by virtue of being the topmost element).
	// Actually no — modal is just a div with no onclick / tabindex,
	// so it shouldn't be enumerated at all. We're not asserting on
	// its label presence; just confirming the occluded button is gone.
}

// TestSoM_DecorativeOverlayDoesNotPrune is the counterweight to
// the occlusion test. A non-interactive overlay (purely decorative
// dimmer / wrapper / styling div) should NOT cause underlying
// candidates to be pruned. Rationale: the cost of a false-positive
// (pruning a real clickable) is much worse than letting the agent
// click through a decorative div. We hit this bug on Patreon's
// login: a non-interactive wrapper above the form pruned the
// email/password/Continue inputs and left the agent with nothing
// to click.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_DecorativeOverlayDoesNotPrune -timeout 60s ./local
func TestSoM_DecorativeOverlayDoesNotPrune(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	const html = `<!doctype html>
<html><body style="margin:0">
<input id="email" style="position:absolute;left:50px;top:50px;width:200px;height:30px" />
<button id="cta" style="position:absolute;left:50px;top:100px;width:120px;height:30px">CTA</button>
<!-- Pure decorative overlay: no role, no onclick, no tabindex.
     elementFromPoint will report this as topmost over the inputs,
     but it's not interactive — the lenient occlusion check should
     skip it and keep #email + #cta in the label set. -->
<div style="position:fixed;inset:0;background:rgba(255,255,255,0.05);pointer-events:none"></div>
</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: srv.URL}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)

	if _, ok := hasElementMatching(els, "CTA"); !ok {
		t.Errorf("CTA button got pruned despite the overlay being decorative — over-strict occlusion check. Got: %+v", summarizeElements(els))
	}
	gotEmail := false
	for _, e := range els {
		if e.Tag == "input" {
			gotEmail = true
			break
		}
	}
	if !gotEmail {
		t.Errorf("email input got pruned despite decorative overlay — over-strict occlusion check. Got: %+v", summarizeElements(els))
	}
}

// TestSoM_PublishButtonOutranksBackgroundDiv verifies the type-
// weighted ranking: a small but high-priority <button> ("Publish")
// should sort BEFORE a big <div onclick> background. Pre-fix, the
// sort was area-DESC and the agent got the background as label=1.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_PublishButtonOutranksBackgroundDiv -timeout 60s ./local
func TestSoM_PublishButtonOutranksBackgroundDiv(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := somTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)

	// Find the Publish button and the bg div in the label set.
	// Publish must have a STRICTLY LOWER label number than bg.
	var pubLabel, bgLabel int = -1, -1
	for _, e := range els {
		if e.Tag == "button" && containsCI(e.Text, "publish") {
			pubLabel = e.Label
		}
		if e.Tag == "div" && containsCI(e.Text, "bg") {
			bgLabel = e.Label
		}
	}
	if pubLabel < 0 {
		t.Fatalf("Publish button not in label set — enumerator regression. Got: %+v", summarizeElements(els))
	}
	// bg may or may not be enumerated; the assertion only matters
	// when both are present.
	if bgLabel >= 0 && pubLabel >= bgLabel {
		t.Errorf("Publish button label=%d should come BEFORE bg div label=%d (type-weighted ranking)", pubLabel, bgLabel)
	}
	t.Logf("✓ Publish=label %d, bg=label %d", pubLabel, bgLabel)
}

// TestSoM_WalksSameOriginIframe verifies that a button rendered
// INSIDE a same-origin iframe gets a SoM label with viewport-
// translated coordinates. This is the bug that bit us on Patreon's
// cookie banner — it lived in an iframe and was completely
// invisible to the original enumerator.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_WalksSameOriginIframe -timeout 60s ./local
func TestSoM_WalksSameOriginIframe(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	// Outer page hosts an iframe via srcdoc (same-origin trivially).
	// Inner doc has a single button.
	const html = `<!doctype html>
<html><body style="margin:0">
<button id="outer-button" style="position:absolute;left:50px;top:50px;width:120px;height:30px">outer</button>
<iframe id="banner"
        style="position:fixed;left:200px;top:300px;width:400px;height:120px;border:0"
        srcdoc="<button id='accept' style='position:absolute;left:30px;top:40px;width:120px;height:30px'>Accept all</button>"></iframe>
</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: srv.URL}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)

	// Verify the iframe-internal "Accept all" button is in the label set.
	accept, ok := hasElementMatching(els, "Accept all")
	if !ok {
		t.Fatalf("iframe-internal 'Accept all' button missing from labels — iframe walk broken. Got: %+v", summarizeElements(els))
	}
	// And its coordinates should be translated to main-viewport
	// space: iframe at (200, 300) + button at (30, 40) inside =
	// (230, 340) in viewport.
	if accept.X < 220 || accept.X > 240 {
		t.Errorf("iframe-internal button x not translated: got %d, want ~230 (iframe.left=200 + button.left=30)", accept.X)
	}
	if accept.Y < 330 || accept.Y > 350 {
		t.Errorf("iframe-internal button y not translated: got %d, want ~340 (iframe.top=300 + button.top=40)", accept.Y)
	}

	// Outer button is also there, unchanged.
	if _, ok := hasElementMatching(els, "outer"); !ok {
		t.Errorf("outer button missing — iframe walk broke main-doc enumeration?")
	}
}

// TestSoM_WalksClosedShadowRootViaAX verifies that a button inside
// a CLOSED shadow root (which is invisible to injected JS by
// design — the host page can't access .shadowRoot) gets a SoM
// label via the accessibility-tree fallback. This is the bug
// that bit us on Patreon's cookie banner: Transcend's consent
// platform renders all its UI in a closed shadow root, so JS-only
// enumeration returned 0 buttons inside the banner regardless of
// how aggressively we walked iframes / open roots.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_WalksClosedShadowRootViaAX -timeout 60s ./local
func TestSoM_WalksClosedShadowRootViaAX(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	const html = `<!doctype html>
<html><body style="margin:0">
<button id="outer" style="position:absolute;left:50px;top:50px;width:120px;height:30px">Outer button</button>

<div id="closed-host" style="position:absolute;left:50px;top:200px;width:300px;height:60px"></div>
<script>
  // Closed shadow root — host.shadowRoot returns null from outside.
  var host = document.getElementById('closed-host');
  var sr = host.attachShadow({mode:'closed'});
  sr.innerHTML = '<button id="inside" style="position:absolute;left:10px;top:15px;width:120px;height:30px">Hidden Action</button>';
</script>
</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: srv.URL}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)

	// "Hidden Action" — inside CLOSED shadow root, only reachable
	// via the AX tree fallback.
	hidden, ok := hasElementMatching(els, "Hidden Action")
	if !ok {
		t.Fatalf("button inside closed shadow root missing — AX-tree fallback broken. Got: %+v", summarizeElements(els))
	}
	// Coords should be in main viewport space: host at (50, 200) +
	// button at (10, 15) inside = (60, 215).
	if hidden.X < 50 || hidden.X > 70 {
		t.Errorf("AX-discovered button x not in expected range: got %d, want ~60", hidden.X)
	}
	if hidden.Y < 205 || hidden.Y > 225 {
		t.Errorf("AX-discovered button y not in expected range: got %d, want ~215", hidden.Y)
	}

	// Outer button still found via JS path.
	if _, ok := hasElementMatching(els, "Outer button"); !ok {
		t.Errorf("outer button missing — AX merge broke main-doc enumeration?")
	}
}

// TestSoM_WalksOpenShadowRoot verifies that a button inside an open
// shadow root (web component pattern) gets a SoM label.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestSoM_WalksOpenShadowRoot -timeout 60s ./local
func TestSoM_WalksOpenShadowRoot(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	const html = `<!doctype html>
<html><body style="margin:0">
<div id="host" style="position:absolute;left:50px;top:50px;width:300px;height:80px"></div>
<script>
  var host = document.getElementById('host');
  var shadow = host.attachShadow({mode:'open'});
  shadow.innerHTML = '<button id="shadow-btn" style="position:absolute;left:10px;top:20px;width:120px;height:30px">Shadow Action</button>';
</script>
</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(computer.DisplaySize{Width: 1024, Height: 700})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: srv.URL}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	els := runSoMOnPage(t, c)
	if _, ok := hasElementMatching(els, "Shadow Action"); !ok {
		t.Fatalf("shadow-DOM button missing from labels — open shadow root walk broken. Got: %+v", summarizeElements(els))
	}
}

// summarizeElements is a debug helper for failure messages. Returns
// a tiny slice with just the fields you'd want to read in a log.
func summarizeElements(els []som.Element) []string {
	out := make([]string, 0, len(els))
	for _, e := range els {
		out = append(out, formatBrief(e))
	}
	return out
}

func formatBrief(e som.Element) string {
	const truncTo = 20
	t := e.Text
	if len([]rune(t)) > truncTo {
		t = string([]rune(t)[:truncTo]) + "…"
	}
	return e.Tag + "@(" + itoa(e.X) + "," + itoa(e.Y) + ")=" + itoa(e.Label) + " '" + t + "'"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
