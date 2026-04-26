package render

import (
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// renderStarfield returns SVG `<circle>` elements approximating xAI's
// stationary starfield motif. Static — Stream Deck buttons render once
// per poll, so the per-frame flicker / shooting-star animation from
// upstream HTML5-canvas implementations isn't reproducible here.
//
// Visual borrowed (positioning logic + opacity-flicker varied across
// stars) from UsmanDevCraft/grok-shooting-stars (MIT) — see
// THIRD_PARTY_LICENSES.md.
//
// Pattern is deterministic so positions don't shuffle between polls.
// Different layers (back / front of glyph) call this with different
// seeds when a layered effect is desired; for now the starfield only
// renders behind the glyph (paint order is bg → starfield → glyph →
// fill → text), so a single seed suffices.
// renderStarfield emits the animated Grok background: a slowly drifting
// star field plus a periodic shooting-star streak across the canvas.
// Stream Deck rasterizes the SVG to a static PNG before display, so SMIL
// `<animate>` doesn't tick — animation has to come from re-rendering the
// SVG every frame and re-sending it via SetImage. Each call samples
// time.Now(), so a driver loop calling RenderButton at e.g. 8 Hz
// produces smooth motion without needing to thread a phase parameter.
//
// Star positions, velocities, periods, and offsets are deterministic
// from a fixed PCG seed so the same star drifts on the same trajectory
// frame to frame; only the time argument changes between calls.
//
// Visual borrowed from UsmanDevCraft/grok-shooting-stars (MIT) — the
// upstream HTML5-canvas implementation does the same dim/bright sin
// flicker, slow drift, and periodic streak. See THIRD_PARTY_LICENSES.md.
func renderStarfield() string {
	const count = 45
	r := rand.New(rand.NewPCG(0xfeed, 0xface))
	now := float64(time.Now().UnixMilli()) / 1000.0
	canvas := float64(Canvas)
	parts := make([]string, 0, count+3)

	// Rotating starfield: each star orbits the canvas center on its
	// own radius, at its own angular velocity, with its own initial
	// phase and opacity flicker. The whole field reads as a slow
	// galactic swirl rather than independent linear drift.
	cx, cy := canvas/2, canvas/2
	for i := 0; i < count; i++ {
		// Polar seed: pick a radius from the center and an initial
		// angle. Radii bias toward the canvas extents so stars cover
		// the corners (sqrt skews uniform sampling outward).
		orbitR := math.Sqrt(r.Float64()) * (canvas * 0.7)
		theta0 := r.Float64() * 2 * math.Pi
		// Angular velocity (rad/sec). Sign per star so neighbors can
		// drift opposite directions; magnitude small (0.05..0.18 rad/s,
		// i.e. ~3..10 deg/s) so the swirl reads as gentle motion.
		omega := (0.05 + r.Float64()*0.13) * (1.0 - 2.0*float64(i%2))
		angle := theta0 + omega*now
		x := cx + orbitR*math.Cos(angle)
		y := cy + orbitR*math.Sin(angle)

		// Star draw parameters — radius / opacity flicker.
		dotR := 0.9 + r.Float64()*1.5
		dim := 0.30 + r.Float64()*0.20
		bright := 0.70 + r.Float64()*0.25
		period := 1.6 + r.Float64()*2.4
		offset := r.Float64() * period
		wave := (math.Sin(2*math.Pi*(now+offset)/period) + 1) / 2
		opacity := dim + wave*(bright-dim)
		parts = append(parts, fmt.Sprintf(
			`<circle cx="%.1f" cy="%.1f" r="%.2f" fill="#ffffff" opacity="%.2f"/>`,
			x, y, dotR, opacity))
	}

	// Shooting star: every shootCycle seconds a streak fires along a
	// random direction for shootDur seconds, then disappears. Direction
	// + start position are seeded from the cycle index so each cycle
	// gets a different streak but every render of the same cycle picks
	// the same one (deterministic).
	const (
		shootCycle = 9.0 // one streak every 9s
		shootDur   = 1.2 // streak visible for 1.2s
		shootSpeed = 220.0
		tailLen    = 28.0
	)
	cycleProgress := math.Mod(now, shootCycle)
	if cycleProgress < shootDur {
		cycleIdx := uint64(now / shootCycle)
		sr := rand.New(rand.NewPCG(cycleIdx, 0xbabe))
		// Start somewhere in the upper-left half so most streaks travel
		// across the visible canvas before exiting.
		startX := sr.Float64() * canvas * 0.6
		startY := sr.Float64() * canvas * 0.6
		// Angle biased toward down-right (the canonical "shooting star"
		// streak direction in xAI's branding).
		angle := math.Pi/4 + (sr.Float64()-0.5)*math.Pi/3
		dx := math.Cos(angle) * shootSpeed * cycleProgress
		dy := math.Sin(angle) * shootSpeed * cycleProgress
		hx := startX + dx
		hy := startY + dy
		tx := hx - math.Cos(angle)*tailLen
		ty := hy - math.Sin(angle)*tailLen
		// Fade in/out at the streak's start and end so it doesn't pop.
		alpha := 1.0
		if cycleProgress < 0.15 {
			alpha = cycleProgress / 0.15
		} else if cycleProgress > shootDur-0.25 {
			alpha = (shootDur - cycleProgress) / 0.25
		}
		parts = append(parts, fmt.Sprintf(
			`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#ffffff" stroke-width="1.4" stroke-linecap="round" opacity="%.2f"/>`,
			tx, ty, hx, hy, alpha*0.7))
		parts = append(parts, fmt.Sprintf(
			`<circle cx="%.1f" cy="%.1f" r="1.8" fill="#ffffff" opacity="%.2f"/>`,
			hx, hy, alpha))
	}

	return strings.Join(parts, "")
}

// Canvas is the edge length (in SVG user units) of a Stream Deck button face.
const Canvas = 144

// ProviderGlyph holds an SVG path for a provider logo.
type ProviderGlyph struct {
	// ViewBox is the SVG viewBox attribute for the glyph path.
	ViewBox string
	// Markup holds raw inner SVG elements for glyphs that are not plain
	// path-only marks. Elements should use currentColor for brand-colored
	// fills/strokes so each render layer can recolor them safely.
	Markup string
	// D is the SVG path `d` attribute for the glyph geometry.
	D string
	// Paths holds multi-path glyph geometry. When set, Paths is rendered
	// instead of D so mixed fill/stroke logos can keep their source shape.
	Paths []GlyphPath
	// Stroke renders the glyph as an outline (stroke + fill:none +
	// vector-effect:non-scaling-stroke) instead of the default filled
	// silhouette — lets outline marks (Tabler, Lucide, etc.) sit
	// alongside solid brand glyphs without reshaping each path to a
	// closed fill region.
	Stroke bool
}

// GlyphPath is one path in a provider glyph.
type GlyphPath struct {
	// D is the SVG path data for this glyph element.
	D string
	// Stroke renders this path as a stroked outline instead of a fill.
	Stroke bool
	// StrokeWidth preserves source SVG stroke widths for stroked paths.
	StrokeWidth float64
}

// ButtonInput configures a button face render.
type ButtonInput struct {
	Label        string
	Value        string
	Subvalue     string
	Ratio        *float64 // nil = reference card
	Direction    string   // "up"|"down"|"right"|"left"
	Fill         string   // hex — meter fill
	Bg           string   // hex
	Fg           string   // hex
	Border       *bool    // nil = true
	ValueSize    string   // "small"|"medium"|"large"
	SubvalueSize string
	Stale        *bool
	Glyph        *ProviderGlyph
	GlyphMode    string // "watermark"|"centered"|"corner"|"none"
	ShowGlyph    *bool  // nil = true
	// SmartContrast enables the dual-layer contrast auto-flip for text
	// and the watermark glyph's back layer. The zero value (false)
	// renders fg exactly as provided — callers opt in per-render.
	// Application-level defaults live in GlobalSettings.SmartContrast
	// (default on), and main.go threads that runtime decision into
	// this field at each render site.
	SmartContrast bool
	// Starfield, when true, paints a fixed white-dot starfield over the
	// bg rect, sitting BEHIND the watermark glyph and text layers
	// (paint order: bg → starfield → glyph → meter fill → text). Used
	// by Grok to echo the xAI / grok.com on-page starfield motif.
	// Static — Stream Deck buttons render once per poll, so the
	// per-frame flicker / shooting-star animation from the upstream
	// canvas implementation isn't reproducible at this layer.
	Starfield bool
}

// valueFontSizes maps a ButtonInput.ValueSize to a starting pixel size.
var valueFontSizes = map[string]int{"small": 26, "medium": 34, "large": 40}

// subvalueFontSizes maps a ButtonInput.SubvalueSize to a starting pixel size.
var subvalueFontSizes = map[string]int{"small": 14, "medium": 18, "large": 22}

const (
	valueFontMin = 22
	valueEmWidth = 0.56
	// labelFontMax matches the "large" subvalueFontSize so a short
	// label can visually dominate the subtext — the category ("SESSION",
	// "LIMIT", "TODAY") should read at least as prominently as the
	// secondary line ("Cost (local)", "4h 20m"), not smaller than it.
	labelFontMax = 22
	labelFontMin = 12
	subvalueMin  = 10
)

// fitFontSize picks the largest font size at or below preferredSize that
// keeps text within maxWidth, clamped to minSize as a floor.
func fitFontSize(text string, maxWidth float64, preferredSize, minSize int) int {
	if text == "" {
		return preferredSize
	}
	if float64(len(text))*float64(preferredSize)*valueEmWidth <= maxWidth {
		return preferredSize
	}
	solved := int(math.Floor(maxWidth / (float64(len(text)) * valueEmWidth)))
	return max(minSize, min(preferredSize, solved))
}

// RenderButton produces an SVG string for a Stream Deck key face.
func RenderButton(in ButtonInput) string {
	fg := def(in.Fg, "#f9fafb")
	bg := def(in.Bg, "#111827")
	fill := def(in.Fill, "#3b82f6")
	showBorder := in.Border == nil || *in.Border
	opacity := "1"
	if in.Stale != nil && *in.Stale {
		opacity = "0.75" // match TS: 0.75 not 0.45
	}

	// Font sizes with auto-fit
	preferredValueFont := valueFontSizes[in.ValueSize]
	if preferredValueFont == 0 {
		preferredValueFont = valueFontSizes["large"]
	}
	valueFontSize := fitFontSize(in.Value, float64(Canvas-16), preferredValueFont, valueFontMin)

	preferredSubFont := subvalueFontSizes[in.SubvalueSize]
	if preferredSubFont == 0 {
		preferredSubFont = subvalueFontSizes["large"]
	}

	// Label layout — auto-fit font and dynamic positioning
	labelLinesRaw := []string{}
	if in.Label != "" {
		labelLinesRaw = strings.Split(in.Label, "\n")
	}
	hasLabel := len(labelLinesRaw) > 0
	value := xmlEscape(in.Value)
	subvalue := ""
	if in.Subvalue != "" {
		subvalue = xmlEscape(in.Subvalue)
	}
	hasSub := subvalue != ""

	// Auto-fit label font
	longestLabel := 0
	for _, line := range labelLinesRaw {
		if len(line) > longestLabel {
			longestLabel = len(line)
		}
	}
	labelFontSize := labelFontMax
	if longestLabel > 0 {
		labelFontSize = fitFontSize(
			strings.Repeat("M", longestLabel),
			float64(Canvas-20), labelFontMax, labelFontMin)
	}
	labelLineHeight := int(math.Round(float64(labelFontSize) * 1.08))

	// Vertical layout: always computed as if label AND subvalue are
	// present. This prevents the value and glyph from shifting when
	// content is toggled (hide subtext, show native title, etc.).

	// Subvalue baseline: leave subvalueFontSize*0.35 pixels of bottom padding
	subvalueBaselineY := float64(Canvas) - math.Round(float64(preferredSubFont)*0.35)
	subvalueTop := subvalueBaselineY - math.Round(float64(preferredSubFont)*0.85)

	// Label block height for one line at the default label size.
	defaultLabelH := float64(labelFontMax) * 1.08
	labelBottom := 14.0 + defaultLabelH

	// Value Y: centered between label bottom and subvalue top
	top := labelBottom + float64(valueFontSize)*0.75
	bot := subvalueTop - float64(valueFontSize)*0.15
	valueY := math.Round((top + bot) / 2)

	// Label elements — render helper takes a single color so the
	// caller can paint the same lines twice: once full-canvas in
	// fgBack, once clipped to the fill rect in fgFill. That gives
	// a clean visual split at the fill line so half a letter over
	// the dark bg reads against fgBack while the other half over
	// the fill reads against fgFill — matches how the watermark
	// glyph splits naturally via its layered composition.
	renderLabels := func(color string) string {
		if !hasLabel {
			return ""
		}
		var parts []string
		for i, line := range labelLinesRaw {
			y := 14.0 + float64(labelFontSize) + float64(i)*float64(labelLineHeight)
			parts = append(parts, fmt.Sprintf(
				`<text x="%d" y="%.0f" font-family="Helvetica,Arial,sans-serif" font-size="%d" font-weight="700" text-anchor="middle" fill="%s" fill-opacity="0.85">%s</text>`,
				Canvas/2, y, labelFontSize, color, xmlEscape(line)))
		}
		return strings.Join(parts, "")
	}

	// Border
	borderElement := ""
	if showBorder {
		borderElement = fmt.Sprintf(
			`<rect x="0.75" y="0.75" width="%.1f" height="%.1f" rx="16" ry="16" fill="none" stroke="%s" stroke-opacity="0.18" stroke-width="1.5"/>`,
			float64(Canvas)-1.5, float64(Canvas)-1.5, fg)
	}

	// Auto-fit subvalue text
	subvalueFitSize := preferredSubFont
	if hasSub {
		subvalueFitSize = fitFontSize(subvalue, float64(Canvas-16), preferredSubFont, subvalueMin)
	}
	renderSubvalue := func(color string) string {
		if !hasSub {
			return ""
		}
		return fmt.Sprintf(
			`<text x="%d" y="%.0f" font-family="Helvetica,Arial,sans-serif" font-size="%d" font-weight="700" text-anchor="middle" fill="%s" fill-opacity="0.85">%s</text>`,
			Canvas/2, subvalueBaselineY, subvalueFitSize, color, subvalue)
	}

	// Fill rect
	ratio := 0.0
	if in.Ratio != nil {
		ratio = *in.Ratio
	}
	ratio = math.Max(0, math.Min(1, ratio))
	rect := fillRect(in.Direction, ratio)

	// Glyph
	showGlyph := (in.ShowGlyph == nil || *in.ShowGlyph) && in.Glyph != nil && in.GlyphMode != "none"
	glyphMode := def(in.GlyphMode, "watermark")
	glyphBack := ""
	glyphFront := ""

	// Back glyph color: when SmartContrast is on, auto-contrast with bg
	// so a user-chosen dark fg (e.g. black) doesn't disappear against a
	// dark brand bg. Off, it uses fg verbatim — matches pre-SmartContrast
	// behavior. The front layer always uses the original knockout-via-bg
	// trick, which just produces the duotone watermark.
	glyphBg := fg
	if in.SmartContrast {
		glyphBg = contrastOver(fg, bg)
	}

	if showGlyph && in.Glyph != nil {
		switch glyphMode {
		case "watermark":
			// Fixed glyph: sized and centered within the label-to-
			// subvalue zone. Positions never change regardless of
			// whether label/subvalue are actually rendered.
			zoneTop := labelBottom + 6
			zoneBot := subvalueTop - 6
			zoneH := zoneBot - zoneTop
			gSize := math.Max(44, math.Min(72, zoneH))
			gxOff := (float64(Canvas) - gSize) / 2
			gyOff := math.Round(zoneTop + (zoneH-gSize)/2)
			xf := ContentFitGlyphTransform(in.Glyph, gxOff, gyOff, gSize, gSize)

			fillLum := hexLuminance(fill)
			var frontColor string
			var frontOpacity float64
			if fillLum > 0.3 {
				// Bright fill (brand colors like Claude orange) — dark
				// knockout so the glyph doesn't wash out the color.
				frontColor = bg
				frontOpacity = 0.30
			} else {
				// Dark fill (reference cards, empty meters) — light
				// glyph so it's actually visible against the dark bg.
				frontColor = glyphBg
				frontOpacity = 0.40
			}
			// SmartContrast: pick the front layer color that actually
			// contrasts with the fill (not just with bg). Fixes the
			// inverse-Ollama case (white bg + dark fill, e.g. Grok)
			// where glyphBg had been auto-flipped dark to contrast bg
			// — and ended up identical to the dark fill, making the
			// front layer invisible. contrastOver picks black or white
			// against the fill, so it's always readable.
			if in.SmartContrast {
				frontColor = contrastOver(glyphBg, fill)
			}

			glyphBack = glyphPathMarkup(xf, glyphBg, 0.70, in.Glyph)
			glyphFront = glyphPathMarkup(xf, frontColor, frontOpacity, in.Glyph)

		case "centered":
			gSize := 60.0
			gOff := (float64(Canvas) - gSize) / 2
			xf := ContentFitGlyphTransform(in.Glyph, gOff, gOff, gSize, gSize)
			glyphFront = glyphPathMarkup(xf, glyphBg, 0.92, in.Glyph)

		case "corner":
			gSize := 20.0
			gx := float64(Canvas) - gSize - 6
			gy := 6.0
			xf := ContentFitGlyphTransform(in.Glyph, gx, gy, gSize, gSize)
			glyphFront = glyphPathMarkup(xf, glyphBg, 0.70, in.Glyph)
		}
	}

	showValueText := !(glyphMode == "centered" && showGlyph)

	renderValue := func(color string) string {
		if !showValueText {
			return ""
		}
		return fmt.Sprintf(
			`<text x="%d" y="%.0f" font-family="Helvetica,Arial,sans-serif" font-size="%d" font-weight="800" text-anchor="middle" fill="%s">%s</text>`,
			Canvas/2, valueY, valueFontSize, color, value)
	}

	// Text color: when SmartContrast is on, the back layer uses a
	// bg-contrasted variant and a separate front layer clipped to
	// the fill rect uses a fill-contrasted variant — so a character
	// straddling the fill line gets painted half in each color and
	// visually splits at the exact pixel where the fill boundary
	// crosses it. Matches the watermark glyph's natural split. When
	// the two contrast picks collapse to the same color (typical
	// outside collision zones under the new contrastOver rule), we
	// skip the front layer to keep the SVG minimal.
	fgBack := fg
	fgFill := fg
	if in.SmartContrast {
		fgBack = contrastOver(fg, bg)
		fgFill = contrastOver(fg, fill)
	}
	textBack := renderLabels(fgBack) + renderValue(fgBack) + renderSubvalue(fgBack)
	textFill := ""
	if fgFill != fgBack && ratio > 0 {
		textFill = renderLabels(fgFill) + renderValue(fgFill) + renderSubvalue(fgFill)
	}

	starfield := ""
	if in.Starfield {
		starfield = renderStarfield()
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" opacity="%s">
  <defs>
    <clipPath id="card">
      <rect width="%d" height="%d" rx="16" ry="16"/>
    </clipPath>
    <clipPath id="fillArea">
      <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>
    </clipPath>
    <filter id="ts" x="-5%%" y="-5%%" width="110%%" height="110%%">
      <feDropShadow dx="0" dy="1" stdDeviation="1.5" flood-color="#000" flood-opacity="0.55"/>
    </filter>
  </defs>
  <g clip-path="url(#card)">
    <rect width="%d" height="%d" fill="%s"/>
    %s
    %s
    <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>
  </g>
  %s
  %s
  <g clip-path="url(#card)">
    <g filter="url(#ts)">
      %s
    </g>
    <g clip-path="url(#fillArea)">
      <g filter="url(#ts)">
        %s
      </g>
    </g>
  </g>
</svg>`,
		Canvas, Canvas, opacity,
		Canvas, Canvas,
		rect.X, rect.Y, rect.W, rect.H,
		Canvas, Canvas, bg,
		starfield,
		glyphBack,
		rect.X, rect.Y, rect.W, rect.H, fill,
		borderElement,
		glyphFront,
		textBack,
		textFill,
	)
}

// RenderLoading produces a loading face with just the provider glyph.
func RenderLoading(glyph *ProviderGlyph, fillColor, bgColor, fgColor string, showBorder *bool, starfield bool) string {
	fg := def(fgColor, "#f9fafb")
	bg := def(bgColor, "#111827")
	border := showBorder == nil || *showBorder
	glyphColor := fillColor
	if glyphColor == "" {
		glyphColor = fg
	}
	stars := ""
	if starfield {
		stars = renderStarfield()
	}

	// Use the same glyph zone as RenderButton's watermark so the
	// glyph never shifts between loading and data states.
	defaultLabelH := float64(labelFontMax) * 1.08
	loadLabelBottom := 14.0 + defaultLabelH
	prefSub := subvalueFontSizes["large"]
	loadSubTop := float64(Canvas) - math.Round(float64(prefSub)*0.35) - math.Round(float64(prefSub)*0.85)
	lzTop := loadLabelBottom + 6
	lzBot := loadSubTop - 6
	lzH := lzBot - lzTop
	loadGlyphSize := math.Max(44, math.Min(72, lzH))

	glyphElement := ""
	if glyph != nil {
		gxOff := (float64(Canvas) - loadGlyphSize) / 2
		gyOff := math.Round(lzTop + (lzH-loadGlyphSize)/2)
		xf := ContentFitGlyphTransform(glyph, gxOff, gyOff, loadGlyphSize, loadGlyphSize)
		// Match the loaded watermark's back-layer opacity so the
		// glyph doesn't read bolder on load than it will after data
		// arrives. Same helper that renders stroke/outline glyphs.
		glyphElement = glyphPathMarkup(xf, glyphColor, 0.70, glyph)
	} else {
		glyphElement = fmt.Sprintf(
			`<circle cx="%d" cy="%d" r="4" fill="%s" fill-opacity="0.4"/>`,
			Canvas/2, Canvas/2, fg)
	}

	borderEl := ""
	if border {
		borderEl = fmt.Sprintf(
			`<rect x="0.75" y="0.75" width="%.1f" height="%.1f" rx="16" ry="16" fill="none" stroke="%s" stroke-opacity="0.18" stroke-width="1.5"/>`,
			float64(Canvas)-1.5, float64(Canvas)-1.5, fg)
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d">
  <defs>
    <clipPath id="card-loading">
      <rect width="%d" height="%d" rx="16" ry="16"/>
    </clipPath>
  </defs>
  <g clip-path="url(#card-loading)">
    <rect width="%d" height="%d" fill="%s"/>
    %s
    %s
  </g>
  %s
</svg>`,
		Canvas, Canvas,
		Canvas, Canvas,
		Canvas, Canvas, bg,
		stars,
		glyphElement,
		borderEl,
	)
}

// --- Helpers ---

// fillRectResult is the geometry of the ratio-filled rectangle drawn behind
// the button content.
type fillRectResult struct {
	X, Y, W, H float64
}

// fillRect computes the fill rectangle for the given direction and ratio.
func fillRect(direction string, ratio float64) fillRectResult {
	c := float64(Canvas)
	switch direction {
	case "down":
		h := c * ratio
		return fillRectResult{0, 0, c, h}
	case "right":
		w := c * ratio
		return fillRectResult{0, 0, w, c}
	case "left":
		w := c * ratio
		return fillRectResult{c - w, 0, w, c}
	default: // "up"
		h := c * ratio
		return fillRectResult{0, c - h, c, h}
	}
}

// def returns val if non-empty, otherwise fallback.
func def(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

// hexColorRe matches a #RGB / #RGBA / #RRGGBB / #RRGGBBAA hex color literal.
// Lengths 5 and 7 are rejected — the old `{3,8}` bound let malformed
// colors pass, breaking downstream color-math assumptions.
var hexColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// IsValidHexColor checks if a string is a valid hex color.
func IsValidHexColor(s string) bool {
	return hexColorRe.MatchString(s)
}

// expandHexColor expands shorthand hex colors (#RGB -> #RRGGBB, #RGBA -> #RRGGBBAA)
// to their full forms. Returns the expanded hex string without the # prefix and a
// bool indicating success. Returns ("", false) if the input length is invalid.
func expandHexColor(s string) (string, bool) {
	hex := strings.TrimPrefix(s, "#")
	switch len(hex) {
	case 3:
		// RGB -> RRGGBB
		return string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]}), true
	case 4:
		// RGBA -> RRGGBBAA
		return string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2], hex[3], hex[3]}), true
	case 6, 8:
		// Already 6 or 8 digits, return as-is
		return hex, true
	default:
		// Invalid length
		return "", false
	}
}

// glyphPathMarkup returns the <g><path/></g> SVG markup for one glyph
// layer. Filled glyphs use fill + fill-opacity; outline glyphs use
// stroke + stroke-opacity with fill:none and vector-effect so the
// stroke width stays visually consistent regardless of the scale
// ContentFitTransform applied.
func glyphPathMarkup(xf, color string, opacity float64, g *ProviderGlyph) string {
	if g.Markup != "" {
		return fmt.Sprintf(
			`<g transform="%s" color="%s" opacity="%.2f">%s</g>`,
			xf, color, opacity, g.Markup)
	}
	if len(g.Paths) > 0 {
		var parts []string
		for _, p := range g.Paths {
			if p.Stroke {
				width := p.StrokeWidth
				if width <= 0 {
					width = 2
				}
				parts = append(parts, fmt.Sprintf(
					`<path d="%s" fill="none" stroke="%s" stroke-opacity="%.2f" stroke-width="%g" stroke-linecap="round" stroke-linejoin="round"/>`,
					p.D, color, opacity, width))
				continue
			}
			parts = append(parts, fmt.Sprintf(
				`<path d="%s" fill="%s" fill-opacity="%.2f"/>`,
				p.D, color, opacity))
		}
		return fmt.Sprintf(`<g transform="%s">%s</g>`, xf, strings.Join(parts, ""))
	}
	if g.Stroke {
		return fmt.Sprintf(
			`<g transform="%s" fill="none" stroke="%s" stroke-opacity="%.2f" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" vector-effect="non-scaling-stroke"><path d="%s"/></g>`,
			xf, color, opacity, g.D)
	}
	return fmt.Sprintf(
		`<g transform="%s" fill="%s" fill-opacity="%.2f"><path d="%s"/></g>`,
		xf, color, opacity, g.D)
}

// srgbToLinear undoes the sRGB gamma curve for one channel value in
// [0, 1], producing the linear-light value WCAG's relative luminance
// formula expects.
func srgbToLinear(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// hexRelativeLuminance returns the WCAG 2.x relative luminance of a
// hex color — sRGB channels linearized, then Rec. 709 weighted. Use
// this for contrast-ratio math; the cheaper hexLuminance is kept for
// coarse "is this color bright or dark" checks that don't need to be
// colorimetrically accurate.
func hexRelativeLuminance(hex string) float64 {
	expanded, ok := expandHexColor(hex)
	if !ok {
		return 0.0
	}
	r8, _ := strconv.ParseInt(expanded[0:2], 16, 64)
	g8, _ := strconv.ParseInt(expanded[2:4], 16, 64)
	b8, _ := strconv.ParseInt(expanded[4:6], 16, 64)
	r := srgbToLinear(float64(r8) / 255.0)
	g := srgbToLinear(float64(g8) / 255.0)
	b := srgbToLinear(float64(b8) / 255.0)
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// contrastOver returns fg unchanged unless fg and over both sit in the
// near-white zone or both sit in the near-dark zone — the only real
// collision cases we care about (white text on a white fill; dark text
// on a dark bg). Mid-luminance pairs (e.g. white on Claude's terracotta,
// or a soft gray on a mid-purple) still read fine, so a strict WCAG
// 4.5:1 gate was over-flipping them to high-contrast black/white and
// making the tile feel off-brand. Zone thresholds sit above Ollama's
// #f7f7f7 fill + #141414 bg pair (the primary trigger) and below any
// mid-tone brand color in the current palette.
func contrastOver(fg, over string) string {
	const darkZone = 0.06
	const lightZone = 0.75
	const dark, light = "#0a0a0a", "#f9fafb"
	fgLum := hexRelativeLuminance(fg)
	overLum := hexRelativeLuminance(over)
	if fgLum <= darkZone && overLum <= darkZone {
		return light
	}
	if fgLum >= lightZone && overLum >= lightZone {
		return dark
	}
	return fg
}

// hexLuminance returns the perceived luminance (0..1) of a hex color using
// the Rec. 709 weighted RGB formula.
func hexLuminance(hex string) float64 {
	expanded, ok := expandHexColor(hex)
	if !ok {
		return 0.0 // Invalid color, return dark
	}
	r, _ := strconv.ParseInt(expanded[0:2], 16, 64)
	g, _ := strconv.ParseInt(expanded[2:4], 16, 64)
	b, _ := strconv.ParseInt(expanded[4:6], 16, 64)
	return (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 255
}

// xmlEscape escapes characters in s that are unsafe inside SVG text nodes
// or attribute values.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// FormatCountdown formats seconds into a human-readable countdown.
// Carries the next-smaller unit when it carries information, but skips
// trailing zeros so "1d 0h" / "1h 0m" never appear. When the immediate
// secondary unit is zero, falls through to the next non-zero adjacent
// unit so e.g. 1d 0h 30m → "1d 30m" (not "1d 0h" or just "1d").
//
//	30s, 5m, 5m 30s, 5h, 5h 30m, 1d, 1d 5h, 1d 30m, 4d 12h.
func FormatCountdown(seconds float64) string {
	s := int(seconds)
	if s < 0 {
		s = 0
	}
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	mins := s / 60
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours := mins / 60
	if hours < 24 {
		if m := mins % 60; m > 0 {
			return fmt.Sprintf("%dh %dm", hours, m)
		}
		return fmt.Sprintf("%dh", hours)
	}
	days := hours / 24
	if h := hours % 24; h > 0 {
		return fmt.Sprintf("%dd %dh", days, h)
	}
	if m := mins % 60; m > 0 {
		return fmt.Sprintf("%dd %dm", days, m)
	}
	return fmt.Sprintf("%dd", days)
}
