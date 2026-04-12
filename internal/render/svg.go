package render

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

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
	Fill         string   // hex
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

// RenderButton produces an SVG string for a Stream Deck key face.
func RenderButton(in ButtonInput) string {
	fg := def(in.Fg, "#f9fafb")
	bg := def(in.Bg, "#111827")
	fill := def(in.Fill, "#3b82f6")
	showBorder := in.Border == nil || *in.Border
	opacity := "1"
	if in.Stale != nil && *in.Stale {
		opacity = "0.45"
	}

	// Value font size
	valueFontSize := textSizeToFont(in.ValueSize, "value")
	subvalueFontSize := textSizeToFont(in.SubvalueSize, "subvalue")

	// Label
	hasLabel := in.Label != ""
	labelBottom := 0.0
	labelElements := ""
	if hasLabel {
		lines := strings.Split(in.Label, "\n")
		if len(lines) == 1 {
			labelElements = fmt.Sprintf(
				`<text x="%d" y="27" font-family="system-ui,sans-serif" font-size="16" font-weight="700" text-anchor="middle" fill="%s" fill-opacity="0.85">%s</text>`,
				Canvas/2, fg, xmlEscape(lines[0]))
			labelBottom = 27
		} else {
			var parts []string
			y := 22.0
			for _, line := range lines {
				parts = append(parts, fmt.Sprintf(
					`<text x="%d" y="%.0f" font-family="system-ui,sans-serif" font-size="13" font-weight="700" text-anchor="middle" fill="%s" fill-opacity="0.85">%s</text>`,
					Canvas/2, y, fg, xmlEscape(line)))
				y += 15
			}
			labelElements = strings.Join(parts, "\n  ")
			labelBottom = y - 15
		}
	}

	// Subvalue
	hasSub := in.Subvalue != ""
	subvalueTop := float64(Canvas) - 16.0
	subvalueElement := ""
	if hasSub {
		subvalueElement = fmt.Sprintf(
			`<text x="%d" y="%.0f" font-family="system-ui,sans-serif" font-size="%d" font-weight="700" text-anchor="middle" fill="%s" fill-opacity="0.85">%s</text>`,
			Canvas/2, subvalueTop, subvalueFontSize, fg, xmlEscape(in.Subvalue))
	}

	// Value Y position
	valueY := 82.0
	if !hasSub && hasLabel {
		valueY = 88
	} else if !hasSub && !hasLabel {
		valueY = 82
	}

	// Fill rect
	ratio := 0.0
	if in.Ratio != nil {
		ratio = *in.Ratio
	}
	ratio = math.Max(0, math.Min(1, ratio))
	rect := fillRect(in.Direction, ratio)

	// Border
	borderElement := ""
	if showBorder {
		borderElement = fmt.Sprintf(
			`<rect x="0.75" y="0.75" width="%d" height="%d" rx="16" ry="16" fill="none" stroke="%s" stroke-opacity="0.18" stroke-width="1.5"/>`,
			Canvas-2, Canvas-2, fg)
	}

	// Glyph
	showGlyph := (in.ShowGlyph == nil || *in.ShowGlyph) && in.Glyph != nil && in.GlyphMode != "none"
	glyphMode := def(in.GlyphMode, "watermark")
	glyphBack := ""
	glyphFront := ""

	if showGlyph && in.Glyph != nil {
		switch glyphMode {
		case "watermark":
			zoneTop := 6.0
			if hasLabel {
				zoneTop = labelBottom + 6
			}
			zoneBot := float64(Canvas) - 6
			if hasSub {
				zoneBot = subvalueTop - 6
			}
			zoneH := zoneBot - zoneTop
			gSize := math.Max(44, math.Min(72, zoneH))
			gxOff := (float64(Canvas) - gSize) / 2
			gyOff := math.Round(zoneTop + (zoneH-gSize)/2)
			xf := ContentFitTransform(in.Glyph.D, gxOff, gyOff, gSize, gSize)

			fillLum := hexLuminance(fill)
			frontColor := bg
			frontOpacity := 0.30
			if fillLum < 0.15 {
				frontColor = fg
				frontOpacity = 0.25
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
			`<text x="%d" y="%.0f" font-family="system-ui,sans-serif" font-size="%d" font-weight="800" text-anchor="middle" fill="%s">%s</text>`,
			Canvas/2, valueY, valueFontSize, fg, xmlEscape(in.Value))
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

	glyphElement := ""
	if glyph != nil {
		glyphSize := 56.0
		glyphOffset := (float64(Canvas) - glyphSize) / 2
		xf := ContentFitTransform(glyph.D, glyphOffset, glyphOffset, glyphSize, glyphSize)
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
			`<rect x="0.75" y="0.75" width="%d" height="%d" rx="16" ry="16" fill="none" stroke="%s" stroke-opacity="0.18" stroke-width="1.5"/>`,
			Canvas-2, Canvas-2, fg)
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

type fillRectResult struct {
	X, Y, W, H float64
}

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

func textSizeToFont(size string, kind string) int {
	switch kind {
	case "value":
		switch size {
		case "small":
			return 28
		case "medium":
			return 34
		default: // "large"
			return 40
		}
	default: // "subvalue"
		switch size {
		case "small":
			return 16
		case "medium":
			return 19
		default: // "large"
			return 22
		}
	}
}

func def(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)

// IsValidHexColor checks if a string is a valid hex color.
func IsValidHexColor(s string) bool {
	return hexColorRe.MatchString(s)
}

func hexLuminance(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) < 6 {
		return 0
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)
	// Relative luminance (ITU-R BT.709)
	return (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 255
}

// LightenHex blends a hex color toward white by amount (0..1).
func LightenHex(hex string, amount float64) string {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) < 6 {
		return "#" + hex
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)
	r = int64(math.Min(255, math.Round(float64(r)+(255-float64(r))*amount)))
	g = int64(math.Min(255, math.Round(float64(g)+(255-float64(g))*amount)))
	b = int64(math.Min(255, math.Round(float64(b)+(255-float64(b))*amount)))
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

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
