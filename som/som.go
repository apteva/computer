// Package som implements Set-of-Mark annotation for screenshots.
//
// SoM is an opt-in grounding aid for vision-language models that aren't
// trained for pixel-precise GUI understanding. Instead of asking the
// model "what are the x,y coordinates of the login button?", we:
//
//  1. Enumerate interactive DOM elements in the viewport via read-only JS.
//  2. Paint a small colored numeric badge at the top-left of each
//     element's bounding box on the screenshot (server-side composite;
//     the page DOM is never modified).
//  3. Let the agent say "click label 7" — we look up bbox #7 and
//     dispatch the click at its center.
//
// The model reads labels (text) instead of estimating pixels. No
// coordinate guessing, no positional priors biting us.
//
// Activated per screenshot via APTEVA_SOM=1. Zero effect when off —
// behaviour is byte-identical to the pre-SoM pipeline.
package som

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"strconv"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Element is one enumerated interactive target on the current page.
// Populated by running EnumScript inside the page's main world; the
// coordinates are in viewport space (same space clicks dispatch in).
type Element struct {
	Label int    `json:"label"` // assigned by Enumerate in order
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Tag   string `json:"tag"`
	Role  string `json:"role,omitempty"`
	Text  string `json:"text,omitempty"`
	Type  string `json:"type,omitempty"`
}

// Center returns the pixel at the center of the element's bbox —
// the point a click(label=N) should dispatch to.
func (e Element) Center() (int, int) {
	return e.X + e.W/2, e.Y + e.H/2
}

// EnumScript is injected into the page's main world via
// chromedp.Evaluate. It returns a JSON array of visible interactive
// elements, capped at 50, ranked by an importance score (element
// type weight × area-tiebreaker) so the most likely click targets
// get the lowest labels.
//
// Smarts beyond a flat selector list — these matter on
// component-heavy UIs (Patreon, Notion, Linear, Twitter):
//
//  Nested-clickable dedup. <div onclick><input/></div> emits one
//  label (the input), not two — agents historically picked the
//  wrong one. Same for <button><svg/></button>: just the button.
//
//  Occlusion-aware. Modal overlays (Patreon's GDPR popup, Twitter's
//  "What's happening" toast) hide elements behind them at click-time
//  but the DOM still enumerates them. We sample
//  document.elementFromPoint at each candidate's center; if a
//  different element is on top, the candidate is hidden and we
//  drop it. The agent sees only what it can actually click.
//
//  Type-weighted ranking. Pure area-DESC put gigantic background
//  containers at label=1. Now: inputs/selects/textareas (5) >
//  buttons (4) > anchors (3) > role=button/link (2) > generic
//  onclick/tabindex (1). Area is the within-tier tiebreaker.
//
// Read-only: queries DOM, reads layout, reads computed style. No
// mutations, no listeners, no globals. Safe against MutationObserver.
const EnumScript = `
(function() {
  var selectors = [
    'a[href]','button','input:not([type=hidden])','select','textarea',
    '[role=button]','[role=link]','[role=menuitem]','[role=tab]',
    '[role=checkbox]','[role=radio]','[role=switch]','[role=combobox]',
    '[role=option]','[role=treeitem]','[role=textbox]','[role=searchbox]',
    // contenteditable catches Slate.js / Lexical / ProseMirror /
    // TinyMCE / Quill / etc. rich-text editors. Patreon's body
    // editor in particular is a contenteditable div with no role.
    '[contenteditable=true]','[contenteditable=""]',
    '[onclick]','[tabindex]:not([tabindex="-1"])'
  ];
  var vw = window.innerWidth, vh = window.innerHeight;

  // priority: lower number = wrapper / generic, higher = real input
  // element. Used for both sort ranking and contains-dedup.
  function priority(tag, role, el) {
    // contenteditable counts as a top-tier text input — it's the
    // body of rich editors (Slate/Lexical/ProseMirror).
    if (el && el.isContentEditable) return 5;
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return 5;
    if (role === 'textbox' || role === 'searchbox') return 5;
    if (tag === 'button') return 4;
    if (tag === 'a') return 3;
    if (role === 'button' || role === 'link' || role === 'menuitem' ||
        role === 'tab' || role === 'checkbox' || role === 'radio' ||
        role === 'switch' || role === 'combobox' || role === 'option' ||
        role === 'treeitem') return 2;
    return 1; // bare onclick / tabindex
  }

  // ─── Pass 1: gather all visible candidates ───────────────────
  // Walks: main document + every same-origin iframe + open shadow
  // roots reachable from the main document. Cookie banners
  // (Cookiebot, OneTrust, Patreon's own banner) frequently render
  // inside iframes/shadow trees; without this walk their buttons
  // are invisible to the agent.
  var candidates = [];
  var seen = new WeakSet();

  // gatherFrom — collect candidates from a Document or ShadowRoot.
  // Coordinates returned by getBoundingClientRect on elements
  // INSIDE a same-origin iframe are LOCAL to that iframe's
  // viewport; offsetX/offsetY translate them into main-viewport
  // pixels so the agent's click coordinates map correctly.
  // styleWin is the window scope used for getComputedStyle —
  // matters because the iframe's own window has its own CSSOM.
  function gatherFrom(root, offsetX, offsetY, styleWin) {
    for (var si = 0; si < selectors.length; si++) {
      var els;
      try { els = root.querySelectorAll(selectors[si]); } catch (e) { continue; }
      for (var ei = 0; ei < els.length; ei++) {
        var el = els[ei];
        if (seen.has(el)) continue;
        seen.add(el);
        var r;
        try { r = el.getBoundingClientRect(); } catch (e) { continue; }
        if (r.width < 4 || r.height < 4) continue;
        // Cull post-translation against main viewport.
        var rLeft = r.left + offsetX;
        var rTop = r.top + offsetY;
        var rRight = r.right + offsetX;
        var rBottom = r.bottom + offsetY;
        if (rRight <= 0 || rBottom <= 0) continue;
        if (rLeft >= vw || rTop >= vh) continue;
        var style;
        try { style = styleWin.getComputedStyle(el); } catch (e) { continue; }
        if (style.visibility === 'hidden' || style.display === 'none') continue;
        if (parseFloat(style.opacity) < 0.1) continue;
        if (el.disabled) continue;
        var x = Math.max(0, Math.round(rLeft));
        var y = Math.max(0, Math.round(rTop));
        var w = Math.min(vw, Math.round(rRight)) - x;
        var h = Math.min(vh, Math.round(rBottom)) - y;
        var text = (el.innerText || el.value ||
                    el.getAttribute('aria-label') ||
                    el.getAttribute('aria-placeholder') ||
                    el.getAttribute('placeholder') ||
                    el.getAttribute('data-placeholder') ||
                    el.getAttribute('data-text') ||
                    '').trim();
        // Rich-text editors (Slate.js, Lexical, ProseMirror) render
        // their placeholder via a CSS ::before pseudo-element instead
        // of any DOM attribute — el.innerText is empty until the
        // user types. Read the computed pseudo-element content so the
        // agent sees "Start writing..." / "Type here" / etc. on the
        // body-editor label and can recognise it as a textbox.
        if (!text && (el.isContentEditable || (el.getAttribute && el.getAttribute('role') === 'textbox'))) {
          try {
            var pseudo = styleWin.getComputedStyle(el, '::before');
            var content = pseudo && pseudo.content;
            if (content && content !== 'none' && content !== 'normal' && content !== '""' && content !== "''") {
              // CSS content values are quoted ("Start writing…"). Strip
              // the surrounding quotes; ignore counter/var() shapes.
              var stripped = content.replace(/^attr\(.+\)$/, '');
              if (/^["'][\s\S]*["']$/.test(stripped)) {
                text = stripped.slice(1, -1).trim();
              }
            }
          } catch (e) { /* cross-origin or detached */ }
        }
        if (text.length > 40) text = text.substr(0, 40);
        var tag = el.tagName.toLowerCase();
        var role = el.getAttribute('role') || '';
        candidates.push({
          el: el, x: x, y: y, w: w, h: h,
          tag: tag, role: role, text: text,
          type: el.type || '',
          prio: priority(tag, role, el)
        });
      }
    }
  }

  // Main document.
  gatherFrom(document, 0, 0, window);

  // Same-origin iframes. Cross-origin throws on contentDocument
  // access — we silently skip those (and label the iframe element
  // itself if it matched a selector, which it doesn't by default;
  // future improvement: add iframe to selector list as a fallback).
  var iframes = document.querySelectorAll('iframe');
  for (var fi = 0; fi < iframes.length; fi++) {
    var ifr = iframes[fi];
    var ifrRect;
    try { ifrRect = ifr.getBoundingClientRect(); } catch (e) { continue; }
    if (ifrRect.width < 4 || ifrRect.height < 4) continue;
    if (ifrRect.right <= 0 || ifrRect.bottom <= 0) continue;
    if (ifrRect.left >= vw || ifrRect.top >= vh) continue;
    var doc, win;
    try {
      doc = ifr.contentDocument;
      win = ifr.contentWindow;
    } catch (e) { continue; }
    if (!doc || !win) continue;
    gatherFrom(doc, ifrRect.left, ifrRect.top, win);
  }

  // Open shadow roots reachable from the main document. Closed
  // shadow roots are inaccessible by design — those stay invisible.
  // Coordinates are in main-viewport space (shadow DOM renders
  // within the host's box), so no offset translation needed.
  var hosts = document.querySelectorAll('*');
  for (var hi = 0; hi < hosts.length; hi++) {
    var host = hosts[hi];
    var sr;
    try { sr = host.shadowRoot; } catch (e) { continue; }
    if (!sr) continue;
    gatherFrom(sr, 0, 0, window);
  }

  // ─── Pass 1.5: modal-aware suppression ───────────────────────
  // When a modal/dialog is open, the page-behind-it is visually
  // covered but the DOM still enumerates it. Sidebar buttons and
  // background controls are technically still clickable (a click
  // there closes most modals via outside-click) but they're NEVER
  // the right next action for an agent navigating the modal flow.
  //
  // Concrete bug we hit: agent opening Patreon's video-embed
  // dialog typed the URL into a sidebar's "Paid access" radio
  // (which has the same input-tier badge color) instead of the
  // dialog's URL field, because both labels were equally available
  // and the dialog one wasn't visually privileged.
  //
  // Detection: look for an explicit dialog container. If found,
  // drop candidates whose center isn't inside its bbox. Heuristics:
  //   1. [role=dialog] — the canonical signal
  //   2. [aria-modal="true"] — same intent, different attr
  //   3. <dialog open> — native HTML dialog element
  //
  // We deliberately do NOT use a generic "fixed-position big-box"
  // heuristic — it false-positives on toolbars, footers, sidebars.
  // If the page doesn't expose a real dialog role, we leave the
  // map untouched (agents still recover via skill / coordinate
  // fallback).
  function findActiveModal() {
    var candidates = [];
    var nodes = document.querySelectorAll('[role=dialog],[aria-modal="true"],dialog[open]');
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      var r = n.getBoundingClientRect();
      // Visible + non-trivial size + on-screen
      if (r.width < 100 || r.height < 80) continue;
      if (r.right <= 0 || r.bottom <= 0 || r.left >= vw || r.top >= vh) continue;
      var s = window.getComputedStyle(n);
      if (s.visibility === 'hidden' || s.display === 'none' || parseFloat(s.opacity) < 0.1) continue;
      candidates.push({el: n, rect: r, area: r.width * r.height});
    }
    if (candidates.length === 0) return null;
    // Multiple modals stacked? Prefer the one with HIGHEST z-index
    // (last in DOM order is also a fine tiebreaker; CSS painters use
    // both).
    candidates.sort(function(a, b){
      var za = parseInt(window.getComputedStyle(a.el).zIndex, 10) || 0;
      var zb = parseInt(window.getComputedStyle(b.el).zIndex, 10) || 0;
      return zb - za;
    });
    return candidates[0];
  }
  var activeModal = findActiveModal();
  if (activeModal) {
    var mb = activeModal.rect;
    candidates = candidates.filter(function(c) {
      // Keep candidates whose center is inside the modal box. Also
      // keep the modal element's descendants explicitly (defensive
      // against tight-fitting modals where the math is off-by-a-pixel).
      var cx = c.x + c.w / 2, cy = c.y + c.h / 2;
      var insideBox = cx >= mb.left && cx <= mb.right && cy >= mb.top && cy <= mb.bottom;
      var insideTree = activeModal.el.contains(c.el);
      return insideBox || insideTree;
    });
  }

  // ─── Pass 2: nested-clickable dedup ──────────────────────────
  // Drop a candidate if it CONTAINS another candidate of equal or
  // higher priority (the contained one is the more specific target,
  // so the wrapper is redundant). Also drop a candidate if it is
  // CONTAINED in another candidate of strictly higher priority (the
  // outer is the real target; the inner is decorative — e.g. a
  // tabindex span inside a button).
  var keep = [];
  for (var i = 0; i < candidates.length; i++) {
    var ci = candidates[i];
    var dominated = false;
    for (var j = 0; j < candidates.length && !dominated; j++) {
      if (i === j) continue;
      var cj = candidates[j];
      if (ci.el.contains(cj.el) && cj.prio >= ci.prio) {
        dominated = true; break;  // ci is a wrapper
      }
      if (cj.el.contains(ci.el) && cj.prio > ci.prio) {
        dominated = true; break;  // ci is decorative inside a stronger target
      }
    }
    if (!dominated) keep.push(ci);
  }

  // ─── Pass 3: occlusion check (lenient — false positives hurt) ─
  // Modal overlays cover elements; the DOM still lists them, but
  // they're not clickable. We sample elementFromPoint at three
  // points along the candidate's horizontal centerline.
  //
  // CRITICAL: cost asymmetry. A false-positive (pruning a real
  // clickable) is much worse than a false-negative (keeping an
  // un-clickable one). The agent loops and gets stuck on the first;
  // recovers by trying another label on the second. So this check
  // is intentionally LENIENT.
  //
  // We only prune a candidate when the topmost element at its
  // center sample IS ITSELF a meaningful interactive (button,
  // input, [role=button], onclick handler, etc.) AND is not the
  // candidate's ancestor/descendant. A non-interactive wrapper
  // div (decorative dimmer, layout container) lets the candidate
  // through — clicks reach the candidate via pointer-events
  // bubbling in most cases. We bias toward labeling, not toward
  // pruning.
  function isUsefulInteractive(el) {
    if (!el) return false;
    var t = el.tagName;
    if (t === 'A' || t === 'BUTTON' || t === 'INPUT' ||
        t === 'TEXTAREA' || t === 'SELECT') return true;
    if (el.getAttribute('role')) return true;
    if (el.hasAttribute('onclick')) return true;
    var ti = el.getAttribute('tabindex');
    if (ti !== null && ti !== '-1') return true;
    return false;
  }
  var visible = [];
  for (var i = 0; i < keep.length; i++) {
    var c = keep[i];
    var probes = [
      [c.x + c.w / 2, c.y + c.h / 2],
      [c.x + Math.max(2, Math.min(c.w - 2, c.w * 0.25)), c.y + c.h / 2],
      [c.x + Math.max(2, Math.min(c.w - 2, c.w * 0.75)), c.y + c.h / 2]
    ];
    var pruned = false;
    for (var p = 0; p < probes.length && !pruned; p++) {
      var px = probes[p][0], py = probes[p][1];
      if (px < 0 || py < 0 || px >= vw || py >= vh) continue;
      var top = document.elementFromPoint(px, py);
      if (!top) continue;
      // Topmost relates to the candidate (self/descendant/ancestor)
      // → not occluded.
      if (top === c.el || c.el.contains(top) || top.contains(c.el)) {
        continue;
      }
      // Topmost is unrelated. Only prune if it's a real interactive
      // — otherwise treat as decorative pass-through and KEEP the
      // candidate.
      if (isUsefulInteractive(top)) {
        pruned = true;
      }
    }
    if (!pruned) visible.push(c);
  }

  // ─── Pass 4: rank, cap, label ────────────────────────────────
  // Score = priority × big-multiplier + log(area). Priority dominates
  // strictly; area is the tiebreaker so same-tier elements stay in
  // a sensible order (Publish button beats hidden secondary actions).
  function score(c) { return c.prio * 1e6 + Math.sqrt(c.w * c.h); }
  visible.sort(function(a, b) { return score(b) - score(a); });
  if (visible.length > 50) visible = visible.slice(0, 50);

  // Strip the el reference (not JSON-encodable + serializing DOM
  // nodes hangs chromedp.Evaluate) and assign final labels.
  var out = [];
  for (var k = 0; k < visible.length; k++) {
    var c = visible[k];
    out.push({
      x: c.x, y: c.y, w: c.w, h: c.h,
      tag: c.tag, role: c.role, text: c.text, type: c.type,
      label: k + 1
    });
  }
  return out;
})()
`

// Color by element family. Matches the tool-def description so the
// agent knows what each color means. Colors chosen for contrast
// against typical page backgrounds and readability in JPEG q60.
var (
	colorLink   = color.RGBA{R: 59, G: 130, B: 246, A: 255}  // blue  — <a>
	colorButton = color.RGBA{R: 34, G: 197, B: 94, A: 255}   // green — <button>, [role=button]
	colorInput  = color.RGBA{R: 249, G: 115, B: 22, A: 255}  // orange — <input>, <textarea>, <select>
	colorOther  = color.RGBA{R: 107, G: 114, B: 128, A: 255} // gray  — generic [onclick] / [tabindex]
	colorBorder = color.RGBA{R: 255, G: 255, B: 255, A: 255} // white badge border for contrast
	colorText   = color.White
)

func ColorFor(e Element) color.RGBA {
	switch e.Tag {
	case "a":
		return colorLink
	case "button":
		return colorButton
	case "input", "textarea", "select":
		return colorInput
	}
	if e.Role == "button" || e.Role == "switch" || e.Role == "checkbox" || e.Role == "radio" {
		return colorButton
	}
	if e.Role == "link" {
		return colorLink
	}
	if e.Role == "combobox" || e.Role == "option" {
		return colorInput
	}
	return colorOther
}

// Annotate composites SoM badges onto a raw screenshot. Accepts JPEG
// or PNG; returns the same format, same dimensions. Badges are small
// filled rects at each element's top-left corner with the label
// number in white. If APTEVA_SOM_BOX is set, a 1-px outline is also
// drawn around each element's full bbox (helps the model associate
// label with region, at the cost of visual noise).
func Annotate(raw []byte, elements []Element) ([]byte, error) {
	src, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("som: decode: %w", err)
	}
	b := src.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, src, b.Min, draw.Src)

	drawBox := os.Getenv("APTEVA_SOM_BOX") != ""

	for _, e := range elements {
		col := ColorFor(e)
		drawBadge(dst, e, col)
		if drawBox {
			drawOutline(dst, e, col)
		}
	}

	var out bytes.Buffer
	switch format {
	case "png":
		err = png.Encode(&out, dst)
	default:
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 75})
	}
	if err != nil {
		return nil, fmt.Errorf("som: encode: %w", err)
	}
	return out.Bytes(), nil
}

// drawBadge paints one numeric badge at element (e.X, e.Y). Badge
// size depends on label digit count so two-digit labels stay readable.
// If the badge would fall off the viewport's top/left, it's nudged
// inward so it stays visible.
func drawBadge(dst *image.RGBA, e Element, col color.RGBA) {
	label := strconv.Itoa(e.Label)
	// 7x13 basic font. Badge = label_width + 8 horizontal padding,
	// 16 tall. One or two digits fits 14+8=22 or 7+8=15 wide.
	bw := len(label)*7 + 8
	bh := 16
	bx, by := e.X, e.Y
	// Nudge to stay inside the destination.
	if bx < 0 {
		bx = 0
	}
	if by < 0 {
		by = 0
	}
	maxX := dst.Bounds().Dx()
	maxY := dst.Bounds().Dy()
	if bx+bw > maxX {
		bx = maxX - bw
	}
	if by+bh > maxY {
		by = maxY - bh
	}

	// White 1-px border for contrast against any background.
	border := image.Rect(bx-1, by-1, bx+bw+1, by+bh+1)
	draw.Draw(dst, border, &image.Uniform{colorBorder}, image.Point{}, draw.Src)
	// Filled rect.
	fill := image.Rect(bx, by, bx+bw, by+bh)
	draw.Draw(dst, fill, &image.Uniform{col}, image.Point{}, draw.Src)
	// Label text, white, centered.
	face := basicfont.Face7x13
	tx := bx + (bw-len(label)*7)/2
	ty := by + 12 // baseline for 13-px font in 16-px box
	(&font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{colorText},
		Face: face,
		Dot:  fixed.P(tx, ty),
	}).DrawString(label)
}

// drawOutline strokes a 1-px rectangle around the element's full bbox.
// Helps the model associate label with region. Opt-in via APTEVA_SOM_BOX.
func drawOutline(dst *image.RGBA, e Element, col color.RGBA) {
	x0, y0 := e.X, e.Y
	x1, y1 := e.X+e.W, e.Y+e.H
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > dst.Bounds().Dx() {
		x1 = dst.Bounds().Dx()
	}
	if y1 > dst.Bounds().Dy() {
		y1 = dst.Bounds().Dy()
	}
	// Top + bottom lines
	draw.Draw(dst, image.Rect(x0, y0, x1, y0+1), &image.Uniform{col}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(x0, y1-1, x1, y1), &image.Uniform{col}, image.Point{}, draw.Src)
	// Left + right lines
	draw.Draw(dst, image.Rect(x0, y0, x0+1, y1), &image.Uniform{col}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(x1-1, y0, x1, y1), &image.Uniform{col}, image.Point{}, draw.Src)
}

// UnmarshalElements parses the JSON array returned by EnumScript.
// Separate from Evaluate-call site so we can unit-test it.
func UnmarshalElements(data []byte) ([]Element, error) {
	var els []Element
	if err := json.Unmarshal(data, &els); err != nil {
		return nil, err
	}
	return els, nil
}

// Enabled reads the APTEVA_SOM env gate. Single source of truth so
// every caller can't disagree about on/off.
func Enabled() bool {
	v := os.Getenv("APTEVA_SOM")
	return v != "" && v != "0" && v != "false" && v != "off"
}
