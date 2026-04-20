package render

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Canvas is the edge length (in SVG user units) of a Stream Deck button face.
const Canvas = 144

// ProviderGlyph holds an SVG path for a provider logo.
type ProviderGlyph struct {
	ViewBox string
	D       string
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
}

// valueFontSizes maps a ButtonInput.ValueSize to a starting pixel size.
var valueFontSizes = map[string]int{"small": 26, "medium": 34, "large": 40}

// subvalueFontSizes maps a ButtonInput.SubvalueSize to a starting pixel size.
var subvalueFontSizes = map[string]int{"small": 14, "medium": 18, "large": 22}

const (
	valueFontMin  = 22
	valueEmWidth  = 0.56
	labelFontMax  = 16
	labelFontMin  = 10
	subvalueMin   = 10
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

	// Label elements
	labelElements := ""
	if hasLabel {
		var parts []string
		for i, line := range labelLinesRaw {
			y := 14.0 + float64(labelFontSize) + float64(i)*float64(labelLineHeight)
			parts = append(parts, fmt.Sprintf(
				`<text x="%d" y="%.0f" font-family="Helvetica,Arial,sans-serif" font-size="%d" font-weight="700" text-anchor="middle" fill="%s" fill-opacity="0.85">%s</text>`,
				Canvas/2, y, labelFontSize, fg, xmlEscape(line)))
		}
		labelElements = strings.Join(parts, "")
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
	subvalueElement := ""
	if hasSub {
		subvalueElement = fmt.Sprintf(
			`<text x="%d" y="%.0f" font-family="Helvetica,Arial,sans-serif" font-size="%d" font-weight="700" text-anchor="middle" fill="%s" fill-opacity="0.85">%s</text>`,
			Canvas/2, subvalueBaselineY, subvalueFitSize, fg, subvalue)
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
			xf := ContentFitTransform(in.Glyph.D, gxOff, gyOff, gSize, gSize)

			fillLum := hexLuminance(fill)
			var frontColor string
			var frontOpacity float64
			if fillLum > 0.3 {
				// Bright fill (brand colors like Claude orange) — dark
				// knockout so the glyph doesn't wash out the color.
				frontColor = bg
				frontOpacity = 0.30
			} else {
				// Dark fill (reference cards, empty meters) — white
				// glyph so it's actually visible against the dark bg.
				frontColor = fg
				frontOpacity = 0.40
			}

			glyphBack = fmt.Sprintf(
				`<g transform="%s" fill="%s" fill-opacity="0.70"><path d="%s"/></g>`,
				xf, fg, in.Glyph.D)
			glyphFront = fmt.Sprintf(
				`<g transform="%s" fill="%s" fill-opacity="%.2f"><path d="%s"/></g>`,
				xf, frontColor, frontOpacity, in.Glyph.D)

		case "centered":
			gSize := 60.0
			gOff := (float64(Canvas) - gSize) / 2
			xf := ContentFitTransform(in.Glyph.D, gOff, gOff, gSize, gSize)
			glyphFront = fmt.Sprintf(
				`<g transform="%s" fill="%s" fill-opacity="0.92"><path d="%s"/></g>`,
				xf, fg, in.Glyph.D)

		case "corner":
			gSize := 20.0
			gx := float64(Canvas) - gSize - 6
			gy := 6.0
			xf := ContentFitTransform(in.Glyph.D, gx, gy, gSize, gSize)
			glyphFront = fmt.Sprintf(
				`<g transform="%s" fill="%s" fill-opacity="0.7"><path d="%s"/></g>`,
				xf, fg, in.Glyph.D)
		}
	}

	showValueText := !(glyphMode == "centered" && showGlyph)

	valueText := ""
	if showValueText {
		valueText = fmt.Sprintf(
			`<text x="%d" y="%.0f" font-family="Helvetica,Arial,sans-serif" font-size="%d" font-weight="800" text-anchor="middle" fill="%s">%s</text>`,
			Canvas/2, valueY, valueFontSize, fg, value)
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" opacity="%s">
  <defs>
    <clipPath id="card">
      <rect width="%d" height="%d" rx="16" ry="16"/>
    </clipPath>
    <filter id="ts" x="-5%%" y="-5%%" width="110%%" height="110%%">
      <feDropShadow dx="0" dy="1" stdDeviation="1.5" flood-color="#000" flood-opacity="0.55"/>
    </filter>
  </defs>
  <g clip-path="url(#card)">
    <rect width="%d" height="%d" fill="%s"/>
    %s
    <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>
  </g>
  %s
  %s
  <g filter="url(#ts)">
  %s
  %s
  %s
  </g>
</svg>`,
		Canvas, Canvas, opacity,
		Canvas, Canvas,
		Canvas, Canvas, bg,
		glyphBack,
		rect.X, rect.Y, rect.W, rect.H, fill,
		borderElement,
		glyphFront,
		labelElements,
		valueText,
		subvalueElement,
	)
}

// RenderLoading produces a loading face with just the provider glyph.
func RenderLoading(glyph *ProviderGlyph, fillColor, bgColor, fgColor string, showBorder *bool) string {
	fg := def(fgColor, "#f9fafb")
	bg := def(bgColor, "#111827")
	border := showBorder == nil || *showBorder
	glyphColor := fillColor
	if glyphColor == "" {
		glyphColor = fg
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
		xf := ContentFitTransform(glyph.D, gxOff, gyOff, loadGlyphSize, loadGlyphSize)
		glyphElement = fmt.Sprintf(
			`<g transform="%s" fill="%s" fill-opacity="0.85"><path d="%s"/></g>`,
			xf, glyphColor, glyph.D)
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
  </g>
  %s
</svg>`,
		Canvas, Canvas,
		Canvas, Canvas,
		Canvas, Canvas, bg,
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

// LightenHex blends a hex color toward white by amount (0..1).
func LightenHex(hex string, amount float64) string {
	expanded, ok := expandHexColor(hex)
	if !ok {
		return "#ffffff" // Invalid color, return white as a safe fallback
	}
	r, _ := strconv.ParseInt(expanded[0:2], 16, 64)
	g, _ := strconv.ParseInt(expanded[2:4], 16, 64)
	b, _ := strconv.ParseInt(expanded[4:6], 16, 64)
	r = int64(math.Min(255, math.Round(float64(r)+(255-float64(r))*amount)))
	g = int64(math.Min(255, math.Round(float64(g)+(255-float64(g))*amount)))
	b = int64(math.Min(255, math.Round(float64(b)+(255-float64(b))*amount)))
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
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
func FormatCountdown(seconds float64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", int(seconds))
	}
	mins := int(seconds) / 60
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours := mins / 60
	if hours < 48 {
		return fmt.Sprintf("%dh %dm", hours, mins%60)
	}
	days := hours / 24
	return fmt.Sprintf("%dd", days)
}