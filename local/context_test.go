package local

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

// All these tests launch real Chrome and write to a real on-disk
// context dir. RUN_BROWSER_TEST=1 gates them like the rest of the
// browser suite. Each test points APTEVA_LOCAL_CONTEXT_DIR at a
// per-test t.TempDir() so runs are hermetic and don't leak
// directories into ~/.apteva.

// withTempContextBase isolates a test's context dirs into t.TempDir.
// Sets the env var, restores via t.Cleanup. Returns the base path so
// the test can assert on what was created.
func withTempContextBase(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("APTEVA_LOCAL_CONTEXT_DIR", base)
	return base
}

// localTestServer spins up a tiny HTTP server that:
//   GET /set?k=&v=  → writes a long-lived cookie, returns 200 "ok"
//   GET /show       → returns the value of the cookie named "k"
//                     (or "MISSING" if absent)
// Used by the persistence/isolation tests so we don't depend on
// any external site (and can hammer it freely without rate limits).
func localTestServer(t *testing.T) (url string, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:    "ctxprobe",
			Value:   r.URL.Query().Get("v"),
			Path:    "/",
			Expires: time.Now().Add(24 * time.Hour),
		})
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/show", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("ctxprobe")
		if err != nil {
			w.Write([]byte("MISSING"))
			return
		}
		w.Write([]byte(c.Value))
	})
	srv := httptest.NewServer(mux)
	return srv.URL, srv.Close
}

// readCookieProbe drives a Computer through nav→/set, nav→/show,
// returns the body text shown at /show. Encapsulates the chromedp
// glue so each test is one logical step per line.
func readCookieProbe(t *testing.T, c *Computer, baseURL, value string) string {
	t.Helper()
	if value != "" {
		// Set the cookie via /set?v=<value>
		if _, err := c.Execute(computer.Action{
			Type: "navigate",
			URL:  baseURL + "/set?v=" + value,
		}); err != nil {
			t.Fatalf("nav /set: %v", err)
		}
	}
	// Navigate to /show and read body text.
	if _, err := c.Execute(computer.Action{
		Type: "navigate",
		URL:  baseURL + "/show",
	}); err != nil {
		t.Fatalf("nav /show: %v", err)
	}
	var body string
	if err := chromedp.Run(c.ctx,
		chromedp.Text("body", &body, chromedp.NodeVisible, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.TrimSpace(body)
}

// TestContext_PersistsCookiesAcrossLaunches is the headline test:
// open with context_id="t1", set a cookie, close. Open again with
// context_id="t1", confirm the cookie survived. Without persistence
// the second launch sees a fresh ephemeral profile and the probe
// returns MISSING.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestContext_PersistsCookiesAcrossLaunches -timeout 90s ./local
func TestContext_PersistsCookiesAcrossLaunches(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	withTempContextBase(t)
	url, stopSrv := localTestServer(t)
	defer stopSrv()

	const ctxID = "persist-1"
	const want = "hello-from-run-1"

	// Run 1: set the cookie under context "persist-1".
	c1, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New (run 1): %v", err)
	}
	if err := c1.OpenSession(computer.OpenOptions{ContextID: ctxID}); err != nil {
		t.Fatalf("OpenSession (run 1): %v", err)
	}
	got := readCookieProbe(t, c1, url, want)
	if got != want {
		t.Fatalf("run 1 (same session) /show = %q, want %q", got, want)
	}
	c1.Close()

	// Run 2: brand new Computer, same context_id. Cookie should
	// already be there without re-setting it.
	c2, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New (run 2): %v", err)
	}
	defer c2.Close()
	if err := c2.OpenSession(computer.OpenOptions{ContextID: ctxID}); err != nil {
		t.Fatalf("OpenSession (run 2): %v", err)
	}
	got = readCookieProbe(t, c2, url, "") // "" = don't re-set
	if got != want {
		t.Fatalf("run 2 (new Computer, same context) /show = %q, want %q — cookie did not persist", got, want)
	}
	t.Logf("✓ cookie %q survived Chrome restart with context_id=%q", want, ctxID)
}

// TestContext_IsolatesCookiesBetweenIDs proves two contexts have
// independent cookie jars: set in ctx_a, switch to ctx_b, must not
// see the value.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestContext_IsolatesCookiesBetweenIDs -timeout 90s ./local
func TestContext_IsolatesCookiesBetweenIDs(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	withTempContextBase(t)
	url, stopSrv := localTestServer(t)
	defer stopSrv()

	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Set under ctx_a.
	if err := c.OpenSession(computer.OpenOptions{ContextID: "ctx_a"}); err != nil {
		t.Fatalf("open ctx_a: %v", err)
	}
	if got := readCookieProbe(t, c, url, "value-a"); got != "value-a" {
		t.Fatalf("ctx_a /show after set = %q, want %q", got, "value-a")
	}

	// Switch to ctx_b on the same Computer — triggers
	// relaunchIfContextChanged → fresh Chrome with different
	// user-data-dir. Cookie should be absent.
	if err := c.OpenSession(computer.OpenOptions{ContextID: "ctx_b"}); err != nil {
		t.Fatalf("open ctx_b: %v", err)
	}
	if got := readCookieProbe(t, c, url, ""); got != "MISSING" {
		t.Fatalf("ctx_b /show = %q, want MISSING — cookie leaked across contexts", got)
	}

	// And switching back to ctx_a should restore it.
	if err := c.OpenSession(computer.OpenOptions{ContextID: "ctx_a"}); err != nil {
		t.Fatalf("re-open ctx_a: %v", err)
	}
	if got := readCookieProbe(t, c, url, ""); got != "value-a" {
		t.Fatalf("ctx_a re-open /show = %q, want %q — cookie didn't survive context switch", got, "value-a")
	}
	t.Log("✓ contexts have independent cookie jars + survive switching back")
}

// TestContext_DefaultLocationCreated verifies that opening a context
// creates the per-context directory under the configured base.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestContext_DefaultLocationCreated -timeout 60s ./local
func TestContext_DefaultLocationCreated(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	base := withTempContextBase(t)

	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.OpenSession(computer.OpenOptions{ContextID: "default-loc"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	want := filepath.Join(base, "default-loc")
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat %s: %v — context dir was not created", want, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", want)
	}
	t.Logf("✓ context dir created at %s", want)
}

// TestContext_StaleSingletonLockReclaimed simulates a previous run
// crashing and leaving a SingletonLock pointing at a dead PID. The
// next OpenSession with the same context_id should silently remove
// the stale lock and proceed.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestContext_StaleSingletonLockReclaimed -timeout 60s ./local
func TestContext_StaleSingletonLockReclaimed(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	base := withTempContextBase(t)
	const ctxID = "stale-lock"

	// Pre-create the dir + a SingletonLock pointing at PID 1
	// flipped negative so it's guaranteed-impossible. Chrome's
	// real lock format is "hostname-PID". We use a target like
	// "ghost-99999999" which os.FindProcess won't be able to
	// signal (FindProcess on Unix succeeds for any PID, but
	// Signal(0) returns "process already finished").
	dir := filepath.Join(base, ctxID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir context dir: %v", err)
	}
	lockPath := filepath.Join(dir, "SingletonLock")
	if err := os.Symlink("ghost-99999999", lockPath); err != nil {
		t.Fatalf("symlink stale lock: %v", err)
	}

	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	// This should silently remove the stale lock and launch.
	if err := c.OpenSession(computer.OpenOptions{ContextID: ctxID}); err != nil {
		t.Fatalf("OpenSession: %v — stale lock not reclaimed?", err)
	}
	t.Log("✓ stale SingletonLock with dead PID was reclaimed and Chrome launched")
}

// TestContext_LockConflictDetected has two simultaneous Computers
// trying to use the same context_id. The first should succeed, the
// second should fail with a clear "in use" error rather than a
// confusing chromedp internal error.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestContext_LockConflictDetected -timeout 90s ./local
func TestContext_LockConflictDetected(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	withTempContextBase(t)
	const ctxID = "conflict-1"

	c1, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New (1): %v", err)
	}
	defer c1.Close()
	if err := c1.OpenSession(computer.OpenOptions{ContextID: ctxID}); err != nil {
		t.Fatalf("OpenSession (1): %v", err)
	}

	// Second Computer wants the same context. SingletonLock is
	// held by the live PID of c1's Chrome — the launch should
	// reject it cleanly. We don't defer Close on c2 because if
	// the launch succeeds despite the lock we want the assertion
	// fatal to surface that. If launch fails (the success path),
	// c2 owns no Chrome and Close is a no-op.
	c2, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New (2): %v", err)
	}
	defer c2.Close()
	err = c2.OpenSession(computer.OpenOptions{ContextID: ctxID})
	if err == nil {
		t.Fatalf("expected lock-conflict error from second OpenSession with the same context_id; got nil")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("error %q does not mention 'in use' — message regression?", err)
	}
	t.Logf("✓ second OpenSession on busy context rejected: %v", err)
}

// TestContext_RelaunchOnIDChange asserts that switching context_id
// on the same Computer actually tears down + relaunches Chrome (new
// PID) — not just changes a label. Without the relaunch the cookie
// jar wouldn't isolate.
//
//	RUN_BROWSER_TEST=1 go test -v -run TestContext_RelaunchOnIDChange -timeout 60s ./local
func TestContext_RelaunchOnIDChange(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	withTempContextBase(t)

	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Pin context A and capture Chrome's process id from CDP.
	if err := c.OpenSession(computer.OpenOptions{ContextID: "ctx-a"}); err != nil {
		t.Fatalf("open ctx-a: %v", err)
	}
	pidA := chromePIDFromCDP(t, c)
	if pidA == 0 {
		t.Fatal("could not read Chrome PID after first OpenSession")
	}

	// Switch to context B → expect a relaunch with a fresh PID.
	if err := c.OpenSession(computer.OpenOptions{ContextID: "ctx-b"}); err != nil {
		t.Fatalf("open ctx-b: %v", err)
	}
	pidB := chromePIDFromCDP(t, c)
	if pidB == 0 {
		t.Fatal("could not read Chrome PID after second OpenSession")
	}
	if pidA == pidB {
		t.Fatalf("PID unchanged across context switch (%d) — relaunch did not fire", pidA)
	}
	t.Logf("✓ context switch relaunched Chrome: PID %d → %d", pidA, pidB)

	// Re-opening the same context (B) on the same Computer must NOT
	// relaunch — it's already in the requested state.
	if err := c.OpenSession(computer.OpenOptions{ContextID: "ctx-b"}); err != nil {
		t.Fatalf("re-open ctx-b: %v", err)
	}
	pidB2 := chromePIDFromCDP(t, c)
	if pidB2 != pidB {
		t.Fatalf("PID changed (%d → %d) on no-op same-context OpenSession — gratuitous relaunch", pidB, pidB2)
	}
	t.Logf("✓ no-op same-context OpenSession kept the same Chrome (PID %d)", pidB2)
}

// chromePIDFromCDP queries Chrome's HTTP DevTools endpoint for its
// process id via the Browser version response. We piggyback on the
// pinned debug port the Computer already exposes via DebugURL: the
// URL embeds "ws=127.0.0.1:<port>/...", from which we lift <port>.
func chromePIDFromCDP(t *testing.T, c *Computer) int {
	t.Helper()
	debug := c.DebugURL()
	const marker = "ws=127.0.0.1:"
	idx := strings.Index(debug, marker)
	if idx < 0 {
		t.Logf("DebugURL does not embed loopback ws=...: %s", debug)
		return 0
	}
	rest := debug[idx+len(marker):]
	end := strings.IndexAny(rest, "/?")
	if end < 0 {
		end = len(rest)
	}
	port := rest[:end]
	if _, err := net.LookupPort("tcp", port); err != nil {
		t.Logf("invalid port in debug URL: %v", err)
		return 0
	}
	// HTTP DevTools /json/version returns {"WebKit-Version":..., ...}
	// without process id — but Browser.getVersion CDP method does.
	// The browser's PID isn't exposed over HTTP /json — fall back to
	// pgrepping for our debug port instead. We launched with
	// --remote-debugging-port=<port>, so lsof on that port gives us
	// the Chrome PID.
	return chromePIDFromListeningPort(t, port)
}

// chromePIDFromListeningPort uses lsof to find the PID listening on
// 127.0.0.1:<port>. macOS + most Linux distros ship lsof; the few
// that don't will see the test return 0 and fail with a clear log.
// Returns 0 on any failure (test surfaces it).
func chromePIDFromListeningPort(t *testing.T, port string) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 2*time.Second)
	if err != nil {
		return 0 // port not listening
	}
	conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-iTCP:"+port, "-sTCP:LISTEN", "-Pn", "-Fp").CombinedOutput()
	if err != nil {
		t.Logf("lsof failed: %v", err)
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "p")); err == nil {
				return n
			}
		}
	}
	return 0
}
