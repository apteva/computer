package local

import (
	"os"
	"testing"
	"time"

	"github.com/apteva/core/pkg/computer"
	"github.com/chromedp/chromedp"
)

// TestProbe_PatreonCookieBanner is a one-shot DOM-archaeology probe:
// open Patreon with the alexa-resume context, wait for the cookie
// banner to render, then run JS that hunts for the "Accept all"
// button via every plausible mechanism (querySelectorAll, iframes,
// open shadow roots, raw text search) and reports where it actually
// lives. Used to figure out why the SoM enumerator can't see it.
//
//	RUN_PATREON_PROBE=1 go test -v -run TestProbe_PatreonCookieBanner -timeout 60s ./local
func TestProbe_PatreonCookieBanner(t *testing.T) {
	if os.Getenv("RUN_PATREON_PROBE") == "" {
		t.Skip("set RUN_PATREON_PROBE=1 to run (uses alexa-resume context)")
	}
	c, err := New(computer.DisplaySize{Width: 1280, Height: 800})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.OpenSession(computer.OpenOptions{
		ContextID: "alexa-resume",
		URL:       "https://www.patreon.com/posts/new",
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	time.Sleep(6 * time.Second)

	const probe = `(function(){
  var out = [];
  function pad(n){return Array(n+1).join(' ');}
  function describe(el){
    if (!el || !el.tagName) return String(el);
    var s = el.tagName.toLowerCase();
    if (el.id) s += '#' + el.id;
    if (el.className && typeof el.className === 'string' && el.className) {
      s += '.' + el.className.split(/\s+/).slice(0,2).join('.');
    }
    var role = el.getAttribute && el.getAttribute('role');
    if (role) s += '[role=' + role + ']';
    return s;
  }
  function chain(el){
    var c = [];
    while (el) {
      c.unshift(describe(el));
      if (el.parentNode) {
        el = el.parentNode;
        if (el && el.host) { c.unshift('[SHADOW-ROOT-host]'); el = el.host; }
      } else { break; }
    }
    return c.slice(-8).join(' > ');
  }

  // Strategy 1: classic querySelectorAll
  var btns = document.querySelectorAll('button,[role=button],input[type=submit]');
  out.push('STRATEGY 1 — document.querySelectorAll(button,[role=button]): ' + btns.length);
  var matched1 = 0;
  for (var i = 0; i < btns.length; i++) {
    var t = (btns[i].innerText || btns[i].value || '').trim();
    if (/accept|reject|cookies|choices/i.test(t)) {
      matched1++;
      out.push('  MATCH: ' + t.substring(0,40) + ' :: ' + chain(btns[i]));
    }
  }
  out.push('  matches: ' + matched1);

  // Strategy 2: ALL elements anywhere with text matching
  var all = document.querySelectorAll('*');
  out.push('STRATEGY 2 — every element in main doc: ' + all.length);
  var matched2 = [];
  for (var i = 0; i < all.length && matched2.length < 8; i++) {
    var t = (all[i].innerText || all[i].textContent || '').trim();
    if (t === 'Accept all' || t === 'Reject non-essential' || t === 'More choices') {
      matched2.push(describe(all[i]) + '   chain: ' + chain(all[i]));
    }
  }
  out.push('  matches: ' + matched2.length);
  matched2.forEach(function(m){ out.push('  ' + m); });

  // Strategy 3: iframes
  var ifs = document.querySelectorAll('iframe');
  out.push('STRATEGY 3 — iframes: ' + ifs.length);
  for (var i = 0; i < ifs.length; i++) {
    var f = ifs[i];
    var src = (f.src || '(no src)').substring(0, 80);
    var rect = f.getBoundingClientRect();
    var ok = false, doc = null, err2 = '';
    try { doc = f.contentDocument; ok = !!doc; } catch (e) { err2 = e.message; }
    out.push('  [' + i + '] src=' + src + ' rect=' + Math.round(rect.left)+','+Math.round(rect.top)+' '+Math.round(rect.width)+'x'+Math.round(rect.height) +
             ' accessible=' + ok + (err2 ? ' err=' + err2 : ''));
    if (ok && doc) {
      var ibtns = doc.querySelectorAll('button,[role=button]');
      for (var j = 0; j < ibtns.length; j++) {
        var t = (ibtns[j].innerText || '').trim();
        if (/accept|reject|cookies|choices/i.test(t)) {
          out.push('    INSIDE: ' + t.substring(0,40) + ' :: ' + describe(ibtns[j]));
        }
      }
    }
  }

  // Strategy 4: open shadow roots
  var shadowHosts = 0, shadowMatches = 0;
  var hostList = document.querySelectorAll('*');
  for (var i = 0; i < hostList.length; i++) {
    var sr = null;
    try { sr = hostList[i].shadowRoot; } catch (e) {}
    if (sr) {
      shadowHosts++;
      var sb = sr.querySelectorAll('button,[role=button]');
      for (var j = 0; j < sb.length; j++) {
        var t = (sb[j].innerText || '').trim();
        if (/accept|reject|cookies|choices/i.test(t)) {
          shadowMatches++;
          out.push('  SHADOW match in ' + describe(hostList[i]) + ': ' + t);
        }
      }
    }
  }
  out.push('STRATEGY 4 — open shadow roots: ' + shadowHosts + ' hosts, ' + shadowMatches + ' matches');

  // Strategy 5: try the most likely cookie-banner selectors
  var selectorTry = [
    '[id*=cookie i]', '[class*=cookie i]', '[id*=consent i]',
    '[class*=consent i]', '[aria-label*=cookie i]', '[role=dialog]',
    'dialog'
  ];
  out.push('STRATEGY 5 — cookie-likely selectors:');
  for (var s = 0; s < selectorTry.length; s++) {
    var matches;
    try { matches = document.querySelectorAll(selectorTry[s]); } catch (e) { continue; }
    if (matches.length > 0) {
      out.push('  ' + selectorTry[s] + ' matches: ' + matches.length);
      for (var m = 0; m < Math.min(matches.length, 3); m++) {
        out.push('    ' + describe(matches[m]) + ' text=' + (matches[m].innerText || '').substring(0,40).replace(/\n/g,' ') );
      }
    }
  }

  return out.join('\n');
})()`
	var report string
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(probe, &report)); err != nil {
		t.Fatalf("evaluate probe: %v", err)
	}
	t.Logf("\n========= PATREON COOKIE BANNER PROBE =========\n%s\n========= END PROBE =========", report)
}
