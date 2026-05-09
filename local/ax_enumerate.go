// AX-tree based element enumeration. Complements the JS-injected
// SoM enumerator (in som/som.go) by traversing the accessibility
// tree, which crosses CLOSED shadow DOM boundaries that injected
// JavaScript fundamentally cannot reach.
//
// Why this matters: cookie consent platforms (Transcend, OneTrust,
// some Cookiebot variants) render their banners inside closed
// shadow roots — `host.attachShadow({mode:'closed'})`. This is by
// design: the host page can't tamper with consent UI. Side effect:
// our SoM enumerator running in the page context gets back ZERO
// candidates from inside the banner, leaving the agent unable to
// see (or click) the Accept/Reject buttons. CDP's accessibility
// domain renders the WAI-ARIA tree from the browser's internal
// representation, which sees through closed shadow roots the same
// way a screen reader does.
//
// Performance note: GetFullAXTree returns every accessible node on
// the page (potentially thousands). We filter by interactive role
// before paying the per-node bounding box round trip, which keeps
// this cheap enough to run alongside the JS enumerator on every
// screenshot.

package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/apteva/computer/som"
	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// axInteractiveRoles is the WAI-ARIA roles the agent treats as
// real click/type targets. Mirrors the priority scoring in the JS
// enumerator so the merged list ranks consistently.
//
// Excluded by intent (NOT clickable in any meaningful sense):
//   region, group, list, listitem, paragraph, heading, image,
//   text, generic, none, presentation, banner, navigation,
//   main, complementary, contentinfo
var axInteractiveRoles = map[string]int{
	// Inputs / typeable controls (priority 5).
	"textbox":    5,
	"searchbox":  5,
	"combobox":   5,
	"spinbutton": 5,
	// Buttons (priority 4).
	"button": 4,
	// Links (priority 3).
	"link": 3,
	// Toggleable / selectable widgets (priority 2).
	"checkbox":         2,
	"radio":            2,
	"switch":           2,
	"menuitem":         2,
	"menuitemcheckbox": 2,
	"menuitemradio":    2,
	"option":           2,
	"tab":              2,
	"treeitem":         2,
	"slider":           2,
}

// enumerateViaAX walks Chrome's accessibility tree and returns
// interactive elements with bounding boxes. Returns nil + nil on
// any error — this is a complement to the JS enumerator, NEVER a
// failure path. Callers merge the result with the JS-enumerated
// list; see mergeAXIntoJS.
func (c *Computer) enumerateViaAX() []som.Element {
	var nodes []*accessibility.Node
	if err := chromedp.Run(c.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		// The Accessibility domain is disabled by default. Without
		// Enable() first, GetFullAXTree returns an empty list (no
		// error, no warning) — exactly the silent-zero case that
		// made our closed-shadow test fail until we found this.
		// Cheap to call repeatedly; no-op if already enabled.
		if err := accessibility.Enable().Do(ctx); err != nil {
			return fmt.Errorf("accessibility.Enable: %w", err)
		}
		var err error
		nodes, err = accessibility.GetFullAXTree().Do(ctx)
		if err != nil {
			return fmt.Errorf("GetFullAXTree: %w", err)
		}
		return nil
	})); err != nil {
		if os.Getenv("APTEVA_AX_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "[AX] error: %v\n", err)
		}
		return nil
	}

	debug := os.Getenv("APTEVA_AX_DEBUG") == "1"
	if debug {
		fmt.Fprintf(os.Stderr, "[AX] GetFullAXTree returned %d nodes\n", len(nodes))
	}
	var out []som.Element
	for _, n := range nodes {
		if n == nil {
			continue
		}
		role := ""
		if n.Role != nil {
			role = axStringValue(n.Role)
		}
		name := ""
		if n.Name != nil {
			name = axStringValue(n.Name)
		}
		if debug {
			fmt.Fprintf(os.Stderr, "[AX] role=%q name=%q ignored=%v backendId=%d\n",
				role, name, n.Ignored, n.BackendDOMNodeID)
		}
		if n.Ignored {
			continue
		}
		if n.Role == nil || n.BackendDOMNodeID == 0 {
			continue
		}
		if role == "" {
			continue
		}
		if _, ok := axInteractiveRoles[role]; !ok {
			continue
		}
		// Per-node bounding box. Failures here are expected for
		// off-screen / display:none / detached nodes — silently skip.
		var box *dom.BoxModel
		_ = chromedp.Run(c.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			b, err := dom.GetBoxModel().WithBackendNodeID(n.BackendDOMNodeID).Do(ctx)
			if err == nil {
				box = b
			}
			return nil
		}))
		if box == nil || len(box.Border) < 8 {
			continue
		}
		// Border quad is [x1,y1, x2,y2, x3,y3, x4,y4] — top-left,
		// top-right, bottom-right, bottom-left in CSS pixels (we
		// pinned deviceScaleFactor=1).
		x := int(box.Border[0])
		y := int(box.Border[1])
		w := int(box.Border[2] - box.Border[0])
		h := int(box.Border[5] - box.Border[1])
		if w < 4 || h < 4 {
			continue
		}
		// Skip "screen-reader-only" elements — many sites position
		// accessibility helpers at extreme negative coordinates
		// (clip-path / off-screen text patterns: top:-9999px, left:-9999px).
		// These are in the AX tree as not-ignored but are NOT
		// clickable for a sighted agent. We saw e.g. (-499875, -999803)
		// for "Text size" on Patreon's composer. Cull anything whose
		// bbox is fully outside the viewport.
		vw, vh := c.display.Width, c.display.Height
		if x+w < 0 || y+h < 0 || x > vw || y > vh {
			continue
		}
		if len(name) > 40 {
			name = name[:40]
		}
		out = append(out, som.Element{
			X:    x,
			Y:    y,
			W:    w,
			H:    h,
			Tag:  role, // AX surface uses the role as both tag and role
			Role: role,
			Text: strings.TrimSpace(name),
		})
	}
	// Apply the same lenient-occlusion check the JS enumerator uses,
	// so a button that's visually behind a modal (and the modal has
	// its own interactive button) doesn't sneak back in via the AX
	// tree just because the screen reader can still see it.
	return c.filterAXByOcclusion(out)
}

// filterAXByOcclusion runs a single JS round-trip that passes each
// AX-candidate's bounding rect through the same elementFromPoint
// check used by the JS enumerator. Lenient: only prunes when the
// topmost element at the candidate's center is ITSELF a meaningful
// interactive AND isn't the AX candidate itself (approximated by
// matching getBoundingClientRect within a few pixels).
//
// Without this, AX would re-introduce visually-occluded elements
// the JS path correctly hid.
func (c *Computer) filterAXByOcclusion(candidates []som.Element) []som.Element {
	if len(candidates) == 0 {
		return candidates
	}
	type rect struct{ X, Y, W, H int }
	rects := make([]rect, len(candidates))
	for i, c := range candidates {
		rects[i] = rect{c.X, c.Y, c.W, c.H}
	}
	payload, err := json.Marshal(rects)
	if err != nil {
		return candidates
	}
	js := fmt.Sprintf(`(function(){
  var rs = %s;
  function isUsefulInteractive(el) {
    if (!el) return false;
    var t = el.tagName;
    if (t === 'A' || t === 'BUTTON' || t === 'INPUT' || t === 'TEXTAREA' || t === 'SELECT') return true;
    if (el.getAttribute('role')) return true;
    if (el.hasAttribute('onclick')) return true;
    var ti = el.getAttribute('tabindex');
    if (ti !== null && ti !== '-1') return true;
    return false;
  }
  var keep = [];
  for (var i = 0; i < rs.length; i++) {
    var r = rs[i];
    var cx = r.X + r.W / 2, cy = r.Y + r.H / 2;
    var top = document.elementFromPoint(cx, cy);
    if (!top) { keep.push(i); continue; }
    if (!isUsefulInteractive(top)) { keep.push(i); continue; }
    // Topmost IS an interactive. Approximate self-match by bbox:
    // if the topmost's rect overlaps ours within a few px, accept
    // (it's probably the AX candidate itself or a child of it).
    var tr = top.getBoundingClientRect();
    if (Math.abs(tr.left - r.X) < 6 && Math.abs(tr.top - r.Y) < 6) {
      keep.push(i); continue;
    }
    // Otherwise the topmost is an unrelated interactive — the AX
    // candidate is genuinely behind it. Prune.
  }
  return keep;
})()`, string(payload))
	var keepIdx []int
	if err := chromedp.Run(c.ctx, chromedp.Evaluate(js, &keepIdx)); err != nil {
		return candidates // can't filter, return as-is
	}
	out := make([]som.Element, 0, len(keepIdx))
	for _, i := range keepIdx {
		if i >= 0 && i < len(candidates) {
			out = append(out, candidates[i])
		}
	}
	return out
}

// axStringValue extracts a string from an accessibility.Value's
// raw JSON payload. The CDP type is jsontext.Value (raw bytes), so
// we unmarshal — handles both bare-string ("button") and other
// shapes safely.
func axStringValue(v *accessibility.Value) string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Value, &s); err != nil {
		return ""
	}
	return s
}

// mergeAXIntoJS appends AX-discovered elements to the JS-enumerated
// list, skipping any whose center is within `dedupRadius` pixels of
// an existing element. The JS list is the source of truth (it had
// proper dedup, occlusion, and ranking already applied); AX adds
// elements that JS couldn't see (closed shadow DOM, mostly).
//
// After merge, labels are reassigned 1..N in the order the merged
// list arrives — JS elements keep their original priority order;
// AX additions land at the end.
func mergeAXIntoJS(jsEls, axEls []som.Element) []som.Element {
	const dedupRadius = 12 // px — generous; AX bbox can disagree with JS by a few px
	seenCenters := make([][2]int, 0, len(jsEls))
	for _, e := range jsEls {
		seenCenters = append(seenCenters, [2]int{e.X + e.W/2, e.Y + e.H/2})
	}
	merged := make([]som.Element, len(jsEls), len(jsEls)+len(axEls))
	copy(merged, jsEls)
	for _, ax := range axEls {
		cx, cy := ax.X+ax.W/2, ax.Y+ax.H/2
		dup := false
		for _, sc := range seenCenters {
			dx := cx - sc[0]
			if dx < 0 {
				dx = -dx
			}
			dy := cy - sc[1]
			if dy < 0 {
				dy = -dy
			}
			if dx <= dedupRadius && dy <= dedupRadius {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		merged = append(merged, ax)
		seenCenters = append(seenCenters, [2]int{cx, cy})
	}
	// Re-label to a clean 1..N sequence after merge.
	for i := range merged {
		merged[i].Label = i + 1
	}
	return merged
}
