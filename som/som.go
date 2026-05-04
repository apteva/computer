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
// elements, capped at 50, sorted by area descending so the most
// prominent UI remains labeled if the cap trims anything.
//
// Read-only: queries DOM, reads layout, reads computed style. No
// mutations, no listeners, no globals. Safe against MutationObserver.
const EnumScript = `
(function() {
  var selectors = [
    'a[href]','button','input:not([type=hidden])','select','textarea',
    '[role=button]','[role=link]','[role=menuitem]','[role=tab]',
    '[role=checkbox]','[role=radio]','[role=switch]','[role=combobox]',
    '[role=option]','[role=treeitem]',
    '[onclick]','[tabindex]:not([tabindex="-1"])'
  ];
  var vw = window.innerWidth, vh = window.innerHeight;
  var out = [];
  var seen = new WeakSet();
  for (var i = 0; i < selectors.length; i++) {
    var els = document.querySelectorAll(selectors[i]);
    for (var j = 0; j < els.length; j++) {
      var el = els[j];
      if (seen.has(el)) continue;
      seen.add(el);
      var r = el.getBoundingClientRect();
      if (r.width < 4 || r.height < 4) continue;
      if (r.right <= 0 || r.bottom <= 0) continue;
      if (r.left >= vw || r.top >= vh) continue;
      var style = window.getComputedStyle(el);
      if (style.visibility === 'hidden' || style.display === 'none') continue;
      if (parseFloat(style.opacity) < 0.1) continue;
      if (el.disabled) continue;
      var x = Math.max(0, Math.round(r.left));
      var y = Math.max(0, Math.round(r.top));
      var w = Math.min(vw, Math.round(r.right)) - x;
      var h = Math.min(vh, Math.round(r.bottom)) - y;
      var text = (el.innerText || el.value || el.getAttribute('aria-label') || el.getAttribute('placeholder') || '').trim();
      if (text.length > 40) text = text.substr(0, 40);
      out.push({
        x: x, y: y, w: w, h: h,
        tag: el.tagName.toLowerCase(),
        role: el.getAttribute('role') || '',
        text: text,
        type: el.type || ''
      });
    }
  }
  out.sort(function(a, b) { return (b.w * b.h) - (a.w * a.h); });
  if (out.length > 50) out = out.slice(0, 50);
  // Assign stable labels AFTER sort — this is the label the agent
  // sees on the screenshot.
  for (var k = 0; k < out.length; k++) out[k].label = k + 1;
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
