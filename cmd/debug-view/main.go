// debug-view launches a local Chrome via the computer/local backend
// and serves a tiny self-contained mini-site at http://localhost:<port>
// that renders the live screencast in a canvas and forwards mouse/key
// events back to Chrome over CDP. No DevTools UI; this is a branded
// viewer talking CDP directly.
//
// Architecture:
//
//	your browser ──http──▶ debug-view server ──serves──▶ index.html
//	      │                                                  │
//	      └─────────── ws://127.0.0.1:<chromePort>/devtools/page/<id>
//	                                  │
//	                                 CDP
//	                                  │
//	                                  ▼
//	                          local Chrome (the agent's)
//
// The page connects WS → Chrome directly (both on loopback), so no
// WebSocket proxy is needed. Mouse moves/clicks/wheel and keyboard
// events are translated to Input.dispatchMouseEvent / .dispatchKeyEvent
// and Input.insertText.
//
// Usage:
//
//	go run ./cmd/debug-view                        # blank tab
//	go run ./cmd/debug-view -url https://wikipedia.org
//	go run ./cmd/debug-view -open=false            # print URL, don't auto-open
//	go run ./cmd/debug-view -port 7878             # pin viewer port
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/apteva/computer/local"
	"github.com/apteva/core/pkg/computer"
)

func main() {
	var (
		initURL = flag.String("url", "", "initial URL to navigate to (optional)")
		width   = flag.Int("w", 1280, "viewport width")
		height  = flag.Int("h", 800, "viewport height")
		doOpen  = flag.Bool("open", true, "open the viewer in the default browser")
		port    = flag.Int("port", 0, "viewer HTTP port (0 = random)")
	)
	flag.Parse()

	comp, err := local.New(computer.DisplaySize{Width: *width, Height: *height})
	if err != nil {
		fmt.Fprintf(os.Stderr, "launch failed: %v\n", err)
		os.Exit(1)
	}
	defer comp.Close()

	if *initURL != "" {
		if _, err := comp.Execute(computer.Action{Type: "navigate", URL: *initURL}); err != nil {
			fmt.Fprintf(os.Stderr, "navigate %s: %v\n", *initURL, err)
		}
	}

	// Extract Chrome's CDP WebSocket from the DevTools frontend URL
	// chromedp gave us. The "ws=" query param holds host:port/path
	// without a scheme — we add ws:// back.
	wsEndpoint, err := extractCDPWebSocketURL(comp.DebugURL())
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not derive CDP WS URL: %v\n", err)
		os.Exit(1)
	}

	// Bind the viewer HTTP server.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bind viewer port: %v\n", err)
		os.Exit(1)
	}
	viewerPort := listener.Addr().(*net.TCPAddr).Port
	viewerURL := fmt.Sprintf("http://127.0.0.1:%d/?ws=%s", viewerPort, url.QueryEscape(wsEndpoint))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(viewerHTML))
	})

	go func() {
		_ = http.Serve(listener, mux)
	}()

	fmt.Println("==============================================================")
	fmt.Println("apteva live view")
	fmt.Println("==============================================================")
	fmt.Printf("Viewer:    %s\n", viewerURL)
	fmt.Printf("CDP WS:    %s\n", wsEndpoint)
	fmt.Println("==============================================================")
	fmt.Println("Press Ctrl-C to stop the browser and exit.")

	if *doOpen {
		if err := openInBrowser(viewerURL); err != nil {
			fmt.Fprintf(os.Stderr, "could not auto-open browser: %v (open the URL above manually)\n", err)
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	fmt.Println("\nshutting down…")
}

// extractCDPWebSocketURL pulls the ws= query param out of a Chrome
// DevTools frontend URL like
// https://chrome-devtools-frontend.appspot.com/.../inspector.html?ws=127.0.0.1:62507/devtools/page/<id>
// and reconstructs it as a proper ws:// URL.
func extractCDPWebSocketURL(debugURL string) (string, error) {
	if debugURL == "" {
		return "", fmt.Errorf("empty DebugURL")
	}
	u, err := url.Parse(debugURL)
	if err != nil {
		return "", err
	}
	wsParam := u.Query().Get("ws")
	if wsParam == "" {
		return "", fmt.Errorf("no ws= query param in %s", debugURL)
	}
	if !strings.HasPrefix(wsParam, "ws://") && !strings.HasPrefix(wsParam, "wss://") {
		wsParam = "ws://" + wsParam
	}
	return wsParam, nil
}

// openInBrowser opens url in the platform's default browser. Returns
// an error if the OS-specific opener isn't available or fails.
func openInBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "linux":
		cmd = exec.Command("xdg-open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return cmd.Start()
}

const viewerHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<title>apteva · live view</title>
<style>
  :root {
    --bg: #0a0a0c;
    --panel: #14141a;
    --border: #2a2a33;
    --fg: #e8e8ee;
    --muted: #6b6b78;
    --accent: #6ee7a3;
    --accent-bg: #1a4d2e;
    --bad: #e76e6e;
    --bad-bg: #4d1a1a;
  }
  html, body { margin: 0; padding: 0; background: var(--bg); color: var(--fg); font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", ui-sans-serif, system-ui, sans-serif; }
  body { padding: 16px; }
  .header { display: flex; justify-content: space-between; align-items: center; padding: 10px 14px; margin-bottom: 14px; border: 1px solid var(--border); border-radius: 8px; background: var(--panel); }
  .brand { font-weight: 600; letter-spacing: 0.4px; font-size: 13px; }
  .brand .dot { display: inline-block; width: 6px; height: 6px; border-radius: 99px; background: var(--accent); margin-right: 8px; vertical-align: middle; box-shadow: 0 0 8px var(--accent); }
  .badge { padding: 3px 10px; border-radius: 99px; font-size: 11px; font-family: ui-monospace, "SF Mono", Menlo, monospace; background: var(--accent-bg); color: var(--accent); letter-spacing: 0.3px; }
  .badge.disconnected { background: var(--bad-bg); color: var(--bad); }
  .stage { display: flex; justify-content: center; }
  canvas { display: block; background: #000; border: 1px solid var(--border); border-radius: 8px; max-width: 100%; height: auto; outline: none; cursor: crosshair; }
  canvas:focus { border-color: var(--accent); box-shadow: 0 0 0 3px rgba(110, 231, 163, 0.15); }
  .footer { margin-top: 14px; color: var(--muted); font-size: 11px; font-family: ui-monospace, "SF Mono", Menlo, monospace; text-align: center; }
  .footer code { color: var(--fg); background: var(--panel); padding: 2px 6px; border-radius: 4px; border: 1px solid var(--border); }
</style>
</head>
<body>
  <div class="header">
    <div class="brand"><span class="dot"></span>apteva · live view</div>
    <div id="status" class="badge">connecting…</div>
  </div>
  <div class="stage">
    <canvas id="screen" width="1280" height="800" tabindex="0"></canvas>
  </div>
  <div class="footer">click on the canvas, then mouse + keyboard route through CDP · endpoint <code id="endpoint">…</code></div>

<script>
(() => {
  const params = new URLSearchParams(location.search);
  const wsURL = params.get('ws');
  const $status = document.getElementById('status');
  const $endpoint = document.getElementById('endpoint');
  const canvas = document.getElementById('screen');
  const ctx = canvas.getContext('2d');
  $endpoint.textContent = wsURL || '(missing ws= query param)';

  if (!wsURL) {
    $status.textContent = 'no endpoint';
    $status.classList.add('disconnected');
    return;
  }

  let nextId = 1;
  function send(method, params) {
    const id = nextId++;
    ws.send(JSON.stringify({ id, method, params: params || {} }));
    return id;
  }

  const ws = new WebSocket(wsURL);

  ws.onopen = () => {
    $status.textContent = 'connected';
    $status.classList.remove('disconnected');
    send('Page.enable');
    send('Runtime.enable');
    send('Page.startScreencast', {
      format: 'jpeg',
      quality: 70,
      maxWidth: 1280,
      maxHeight: 800,
      everyNthFrame: 1,
    });
    canvas.focus();
  };

  ws.onclose = () => {
    $status.textContent = 'disconnected';
    $status.classList.add('disconnected');
  };

  ws.onerror = (e) => {
    $status.textContent = 'error';
    $status.classList.add('disconnected');
    console.error('WebSocket error', e);
  };

  ws.onmessage = (msg) => {
    let m;
    try { m = JSON.parse(msg.data); } catch { return; }
    if (m.method === 'Page.screencastFrame') {
      const img = new Image();
      img.onload = () => {
        if (img.naturalWidth !== canvas.width || img.naturalHeight !== canvas.height) {
          canvas.width = img.naturalWidth;
          canvas.height = img.naturalHeight;
        }
        ctx.drawImage(img, 0, 0);
      };
      img.src = 'data:image/jpeg;base64,' + m.params.data;
      send('Page.screencastFrameAck', { sessionId: m.params.sessionId });
    }
  };

  function canvasToPage(e) {
    const rect = canvas.getBoundingClientRect();
    const sx = canvas.width / rect.width;
    const sy = canvas.height / rect.height;
    return {
      x: Math.round((e.clientX - rect.left) * sx),
      y: Math.round((e.clientY - rect.top) * sy),
    };
  }

  let mouseDown = false;
  canvas.addEventListener('mousemove', (e) => {
    const p = canvasToPage(e);
    send('Input.dispatchMouseEvent', {
      type: 'mouseMoved',
      x: p.x, y: p.y,
      button: mouseDown ? 'left' : 'none',
    });
  });
  canvas.addEventListener('mousedown', (e) => {
    e.preventDefault();
    canvas.focus();
    mouseDown = true;
    const p = canvasToPage(e);
    send('Input.dispatchMouseEvent', {
      type: 'mousePressed',
      x: p.x, y: p.y,
      button: 'left',
      clickCount: 1,
    });
  });
  canvas.addEventListener('mouseup', (e) => {
    e.preventDefault();
    mouseDown = false;
    const p = canvasToPage(e);
    send('Input.dispatchMouseEvent', {
      type: 'mouseReleased',
      x: p.x, y: p.y,
      button: 'left',
      clickCount: 1,
    });
  });
  canvas.addEventListener('wheel', (e) => {
    e.preventDefault();
    const p = canvasToPage(e);
    send('Input.dispatchMouseEvent', {
      type: 'mouseWheel',
      x: p.x, y: p.y,
      deltaX: -e.deltaX,
      deltaY: -e.deltaY,
    });
  }, { passive: false });

  // Keyboard. Printable single-char keys go through Input.insertText
  // (handles IME, dead keys, modifiers cleanly). Everything else
  // (Enter, Backspace, arrows, etc.) goes through dispatchKeyEvent.
  function keyEvent(type, e) {
    return {
      type: type,
      key: e.key,
      code: e.code,
      windowsVirtualKeyCode: e.keyCode,
      modifiers:
        (e.altKey ? 1 : 0) |
        (e.ctrlKey ? 2 : 0) |
        (e.metaKey ? 4 : 0) |
        (e.shiftKey ? 8 : 0),
    };
  }
  canvas.addEventListener('keydown', (e) => {
    e.preventDefault();
    const printable = e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey;
    if (printable) {
      send('Input.insertText', { text: e.key });
    } else {
      send('Input.dispatchKeyEvent', keyEvent('keyDown', e));
    }
  });
  canvas.addEventListener('keyup', (e) => {
    e.preventDefault();
    send('Input.dispatchKeyEvent', keyEvent('keyUp', e));
  });
})();
</script>
</body>
</html>
`
