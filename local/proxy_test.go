package local

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
)

// TestParseProxyURL pins the URL-splitting helper that feeds Chrome's
// --proxy-server flag and the auth handler. The contract is: server
// goes to Chrome, creds go to CDP. Mixing them produces a 407 loop.
func TestParseProxyURL(t *testing.T) {
	cases := []struct {
		raw     string
		server  string
		user    string
		pass    string
		wantErr bool
	}{
		{"http://proxy.example.com:8080", "http://proxy.example.com:8080", "", "", false},
		{"http://user:pass@proxy.example.com:8080", "http://proxy.example.com:8080", "user", "pass", false},
		{"socks5://10.0.0.1:1080", "socks5://10.0.0.1:1080", "", "", false},
		{"proxy.example.com:8080", "http://proxy.example.com:8080", "", "", false}, // bare host:port → http://
		{"https://u:p@proxy.example.com:443", "https://proxy.example.com:443", "u", "p", false},
		{"http://", "", "", "", true}, // missing host
	}
	for _, tc := range cases {
		s, u, p, err := parseProxyURL(tc.raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseProxyURL(%q): got err=%v wantErr=%v", tc.raw, err, tc.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if s != tc.server || u != tc.user || p != tc.pass {
			t.Errorf("parseProxyURL(%q) = (%q,%q,%q), want (%q,%q,%q)",
				tc.raw, s, u, p, tc.server, tc.user, tc.pass)
		}
	}
}

// TestOpenSession_ProxyTrueWithoutConfig pins the "no ProxyURL,
// proxy=true" error path. The agent gets a clear configuration
// message instead of silently going direct (the latter is what we
// had before the refactor and what the agent had no way to detect).
func TestOpenSession_ProxyTrueWithoutConfig(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}
	c, err := New(computer.DisplaySize{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	want := true
	err = c.OpenSession(computer.OpenOptions{Proxy: &want})
	if err == nil {
		t.Fatal("expected error when proxy=true but no ProxyURL configured, got nil")
	}
	if !strings.Contains(err.Error(), "ProxyURL") {
		t.Errorf("error should mention ProxyURL, got: %v", err)
	}
}

// TestOpenSession_ProxyTrueRoutesEgress is the meat. Stand up a
// throwaway HTTP proxy in-process, point local Chrome at it via
// Options.ProxyURL, ask the agent to flip on proxy via OpenSession,
// then navigate and assert the proxy actually saw the request.
//
// Without the launch->relaunch path implemented, OpenSession would
// silently no-op and Chrome would dial the target directly — the
// proxy's atomic counter would stay at zero and this test would fail.
func TestOpenSession_ProxyTrueRoutesEgress(t *testing.T) {
	if os.Getenv("RUN_BROWSER_TEST") == "" {
		t.Skip("set RUN_BROWSER_TEST=1 to run (launches Chrome)")
	}

	// Origin server: returns a tiny HTML payload so the test has
	// something to look for.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>origin-served</body></html>")
	}))
	defer origin.Close()

	// Tiny forward proxy. Counts every CONNECT or absolute-URL
	// proxied GET. We don't need real HTTPS support — Chrome will
	// hit our http origin via plain GET through the proxy.
	var hits int64
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyLn.Close()
	proxyURL := "http://" + proxyLn.Addr().String()
	proxySrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&hits, 1)
			// Forward absolute-URL GET requests to the origin. For the
			// shape this test exercises (http origin), Chrome sends
			// "GET http://127.0.0.1:NNNN/ HTTP/1.1" through the proxy
			// — exactly what we want.
			if r.Method == http.MethodConnect {
				http.Error(w, "CONNECT not supported in test proxy", http.StatusNotImplemented)
				return
			}
			fwd, _ := http.NewRequest(r.Method, r.URL.String(), r.Body)
			fwd.Header = r.Header.Clone()
			resp, err := http.DefaultTransport.RoundTrip(fwd)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, v := range resp.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}),
	}
	go proxySrv.Serve(proxyLn)
	defer proxySrv.Close()

	c, err := NewWithOptions(
		computer.DisplaySize{Width: 800, Height: 600},
		Options{ProxyURL: proxyURL},
	)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()

	// Verify the eager launch was DIRECT, not proxied — the agent
	// gets to choose at OpenSession time, not at construction.
	if c.proxyActive {
		t.Fatalf("eager launch should be direct (proxyActive=false), got proxyActive=true")
	}
	preHits := atomic.LoadInt64(&hits)
	if preHits != 0 {
		t.Fatalf("proxy saw %d hits before any nav — eager launch leaked through it", preHits)
	}

	// Now agent flips proxy on. This relaunches Chrome through the
	// proxy and navigates to origin.
	want := true
	if err := c.OpenSession(computer.OpenOptions{Proxy: &want, URL: origin.URL}); err != nil {
		t.Fatalf("OpenSession with proxy=true: %v", err)
	}
	if !c.proxyActive {
		t.Fatalf("after OpenSession(proxy=true), proxyActive should be true")
	}

	// Allow a beat for the navigation to fully settle through the
	// proxy. Chrome may also request favicons, etc — any of those
	// passing through the proxy proves the wire-up.
	time.Sleep(500 * time.Millisecond)

	if atomic.LoadInt64(&hits) == 0 {
		t.Fatalf("proxy received zero requests after OpenSession(proxy=true) and nav — proxy flag did not take effect")
	}
}
