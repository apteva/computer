package local

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

// keyTestServer serves a small page with three things on it:
//
//	#input  — text input that captures any key events that bubble up
//	#dialog — open <dialog> (closes when Escape is pressed natively)
//	#focus  — counts focus changes from Tab presses
//	#status — JSON-ish status read by the test
//
// The page exposes window.__report() which returns a serialised
// snapshot of all observed signals. The test reads it after every
// key dispatch.
func keyTestServer(t *testing.T) (url string, stop func()) {
	t.Helper()
	const html = `<!doctype html>
<html><body>
<input id="input" autofocus />
<dialog id="dialog" open>some dialog</dialog>
<button id="b1">b1</button>
<button id="b2">b2</button>
<pre id="status">empty</pre>
<script>
  const inp = document.getElementById('input');
  const dlg = document.getElementById('dialog');
  const status = document.getElementById('status');
  let lastKey = '', lastCode = '', lastMods = '', dialogClosedBy = '';
  // Capture every keydown — used to verify Key/Code/modifier values
  // arrive correctly.
  document.addEventListener('keydown', (e) => {
    lastKey = e.key;
    lastCode = e.code;
    lastMods = [e.altKey?'alt':'', e.ctrlKey?'ctrl':'', e.metaKey?'meta':'', e.shiftKey?'shift':''].filter(Boolean).join('+');
    refresh();
  });
  // Watching for dialog close via Escape. The native cancel event
  // fires when the user presses Esc on an open <dialog>.
  dlg.addEventListener('cancel', () => { dialogClosedBy = 'esc'; refresh(); });
  dlg.addEventListener('close',  () => { if (!dialogClosedBy) dialogClosedBy = 'other'; refresh(); });
  function refresh() {
    status.textContent = JSON.stringify({
      key: lastKey,
      code: lastCode,
      mods: lastMods,
      input: inp.value,
      activeTag: document.activeElement.tagName.toLowerCase(),
      activeId: document.activeElement.id || '',
      dialogOpen: dlg.open,
      dialogClosedBy: dialogClosedBy,
    });
  }
  refresh();
</script>
</body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	})
	srv := httptest.NewServer(mux)
	return srv.URL, srv.Close
}

// readStatus reads the serialised page state JSON we render into
// #status. Returns the raw string; callers grep substrings instead
// of unmarshalling — keeps the test light.
func readStatus(t *testing.T, c *Computer) string {
	t.Helper()
	var s string
	if err := chromedp.Run(c.ctx,
		chromedp.Text("#status", &s, chromedp.NodeVisible, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

// pressKey is shorthand for the agent path: build the Action,
// route through Execute. Mirrors what the agent's tool call does.
func pressKey(t *testing.T, c *Computer, key string) {
	t.Helper()
	if _, err := c.Execute(computer.Action{Type: "key", Key: key}); err != nil {
		t.Fatalf("key %q: %v", key, err)
	}
}

// TestKey_NamedSpecialKeys covers the historically-broken case:
// the agent passes a literal key name like "Escape" / "Enter" /
// "Tab" / "ArrowUp" and the browser receives a real keystroke (NOT
// the typed characters E-s-c-a-p-e). Asserts on KeyboardEvent.key
// and KeyboardEvent.code arriving correctly — the strongest signal
// our dispatcher works without conflating with browser default-action
// behaviour (which CDP synthetic events handle inconsistently across
// Chrome versions: Esc-closes-dialog and Tab-moves-focus need real
// "trusted" key events that we can't reliably emit from CDP).
//
// Critically, also asserts the OLD-behaviour regression: the literal
// characters "Escape" / "Tab" / "ArrowUp" should NOT appear typed
// into the input.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestKey_NamedSpecialKeys -timeout 60s ./local
func TestKey_NamedSpecialKeys(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := keyTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	cases := []struct {
		press      string
		wantKey    string
		wantCode   string
	}{
		{"Escape", "Escape", "Escape"},
		{"escape", "Escape", "Escape"},
		{"Enter", "Enter", "Enter"},
		{"Tab", "Tab", "Tab"},
		{"Backspace", "Backspace", "Backspace"},
		{"ArrowUp", "ArrowUp", "ArrowUp"},
		{"ArrowDown", "ArrowDown", "ArrowDown"},
		{"PageDown", "PageDown", "PageDown"},
		{"F5", "F5", "F5"},
	}
	for _, tc := range cases {
		pressKey(t, c, tc.press)
		st := readStatus(t, c)
		if !strings.Contains(st, `"key":"`+tc.wantKey+`"`) {
			t.Errorf("press %q: expected key=%s, got %s", tc.press, tc.wantKey, st)
		}
		if !strings.Contains(st, `"code":"`+tc.wantCode+`"`) {
			t.Errorf("press %q: expected code=%s, got %s", tc.press, tc.wantCode, st)
		}
		// Old-bug regression: the literal name must NOT have been
		// typed into the input.
		if strings.Contains(st, `"input":"`+tc.press+`"`) {
			t.Errorf("press %q: literal name was typed into input — old bug back: %s", tc.press, st)
		}
	}
}

// TestKey_ModifierCombos verifies "ctrl+a" etc. arrive as a real
// key event with the modifier bit set, NOT as the literal text
// "ctrl+a".
//
//	RUN_BROWSER_TEST=1 go test -v -run TestKey_ModifierCombos -timeout 60s ./local
func TestKey_ModifierCombos(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := keyTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Pre-fill the input with some text via the existing type
	// action, then press Ctrl+A — the input value shouldn't change
	// (Ctrl+A is "select all" in browsers, not insertion of text).
	if _, err := c.Execute(computer.Action{Type: "type", Text: "hello"}); err != nil {
		t.Fatalf("type hello: %v", err)
	}
	pressKey(t, c, "ctrl+a")
	st := readStatus(t, c)
	if !strings.Contains(st, `"key":"a"`) {
		t.Errorf("ctrl+a: expected key=a, got %s", st)
	}
	if !strings.Contains(st, `"mods":"ctrl"`) {
		t.Errorf("ctrl+a: expected mods=ctrl, got %s", st)
	}
	if !strings.Contains(st, `"input":"hello"`) {
		t.Errorf("ctrl+a: input value should be unchanged at 'hello', got %s", st)
	}

	// shift+tab — code should be "Tab", mods should include shift.
	pressKey(t, c, "shift+tab")
	st = readStatus(t, c)
	if !strings.Contains(st, `"key":"Tab"`) {
		t.Errorf("shift+tab: expected key=Tab, got %s", st)
	}
	if !strings.Contains(st, `"mods":"shift"`) {
		t.Errorf("shift+tab: expected mods=shift, got %s", st)
	}

	// Multi-modifier case: ctrl+shift+z (browser undo→redo on most platforms).
	pressKey(t, c, "ctrl+shift+z")
	st = readStatus(t, c)
	if !strings.Contains(st, `"key":"z"`) {
		t.Errorf("ctrl+shift+z: expected key=z, got %s", st)
	}
	if !strings.Contains(st, `"mods":"ctrl+shift"`) {
		t.Errorf("ctrl+shift+z: expected mods=ctrl+shift, got %s", st)
	}
}

// TestKey_SinglePrintableCharStillWorks is the regression guard:
// passing a single character like "a" must still produce a real
// keydown event with the right Key/Code (preserving the historical
// behaviour our dispatch's single-char branch routes to).
//
// Note: we don't assert the char is INSERTED into a form input —
// chromedp.KeyEvent's behaviour around that is character-class
// dependent (letters insert reliably; digits / punctuation are
// inconsistent across Chrome versions). Agents that want to type
// text into a field should use action=type, not action=key. This
// test guards the dispatch surface only.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestKey_SinglePrintableCharStillWorks -timeout 60s ./local
func TestKey_SinglePrintableCharStillWorks(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := keyTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	cases := []struct {
		press, wantKey, wantCode string
	}{
		{"a", "a", "KeyA"},
		{"Z", "Z", "KeyZ"},
		{"5", "5", "Digit5"},
		{"?", "?", ""}, // Code varies; just ensure key was received
	}
	for _, tc := range cases {
		pressKey(t, c, tc.press)
		st := readStatus(t, c)
		if !strings.Contains(st, `"key":"`+tc.wantKey+`"`) {
			t.Errorf("press %q: expected key=%s, got %s", tc.press, tc.wantKey, st)
		}
		if tc.wantCode != "" && !strings.Contains(st, `"code":"`+tc.wantCode+`"`) {
			t.Errorf("press %q: expected code=%s, got %s", tc.press, tc.wantCode, st)
		}
	}
}

// TestKey_UnknownKeyNameFallsBackToLiteral confirms that an unknown
// multi-char key name doesn't error — it falls back to typing the
// literal characters, matching the OLD behaviour (so we don't break
// anything that depended on it). Surface via the [BROWSER] log so
// operators can see what to add to specialKeys.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestKey_UnknownKeyNameFallsBackToLiteral -timeout 60s ./local
func TestKey_UnknownKeyNameFallsBackToLiteral(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	url, stop := keyTestServer(t)
	defer stop()
	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Execute(computer.Action{Type: "navigate", URL: url}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// "MediaStop" is a real KeyboardEvent.key value but we don't
	// have it in specialKeys — should fall back to typing.
	pressKey(t, c, "Foobar")
	st := readStatus(t, c)
	// Last keydown event observed should be the final char typed
	// during the literal-fallback iteration ("r"). We don't assert
	// the input value because chromedp.KeyEvent's char-insertion
	// behaviour is class-dependent.
	if !strings.Contains(st, `"key":"r"`) {
		t.Errorf("Foobar fallback: expected last keydown to be 'r' (typed literally rune-by-rune), got %s", st)
	}
}
