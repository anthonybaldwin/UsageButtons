// Package render produces SVG button faces for Stream Deck keys.
package render

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// BBox is an axis-aligned bounding box.
type BBox struct {
	MinX, MinY, MaxX, MaxY float64
}

// Width returns the bounding box width.
func (b BBox) Width() float64 { return b.MaxX - b.MinX }

// Height returns the bounding box height.
func (b BBox) Height() float64 { return b.MaxY - b.MinY }

// tokenRe splits an SVG path d attribute into command letters and numbers.
var tokenRe = regexp.MustCompile(`[a-zA-Z]|[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?`)

// PathBBox computes the approximate bounding box of an SVG path's d
// attribute. Includes bezier control points (conservative estimate).
func PathBBox(d string) BBox {
	tokens := tokenRe.FindAllString(d, -1)
	if len(tokens) == 0 {
		return BBox{}
	}

	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	cx, cy := 0.0, 0.0

	mark := func(x, y float64) {
		if x < minX { minX = x }
		if x > maxX { maxX = x }
		if y < minY { minY = y }
		if y > maxY { maxY = y }
	}

	i := 0
	cmd := "M"

	nextNum := func() float64 {
		for i < len(tokens) && len(tokens[i]) == 1 && isLetter(tokens[i][0]) {
			i++
		}
		if i >= len(tokens) {
			return 0
		}
		v, _ := strconv.ParseFloat(tokens[i], 64)
		i++
		return v
	}

	// consumeArcFlag advances past a single arc flag ('0' or '1'). In
	// compact SVG path notation, flags may be packed with adjacent
	// numbers (e.g., "013.046" = flag 0, flag 1, coord 3.046); in
	// that case we peel one character off the current token and leave
	// the remainder for the next read.
	consumeArcFlag := func() {
		for i < len(tokens) && len(tokens[i]) == 1 && isLetter(tokens[i][0]) {
			i++
		}
		if i >= len(tokens) {
			return
		}
		tok := tokens[i]
		if tok == "0" || tok == "1" {
			i++
			return
		}
		if len(tok) > 1 && (tok[0] == '0' || tok[0] == '1') {
			tokens[i] = tok[1:]
			return
		}
		// Malformed; fall back to consuming a whole number.
		i++
	}

	for i < len(tokens) {
		tok := tokens[i]
		if len(tok) == 1 && isLetter(tok[0]) {
			cmd = tok
			i++
		}

		rel := cmd == strings.ToLower(cmd)
		CMD := strings.ToUpper(cmd)

		switch CMD {
		case "M", "L", "T":
			x, y := nextNum(), nextNum()
			if rel { x += cx; y += cy }
			mark(x, y)
			cx, cy = x, y
			if CMD == "M" {
				if rel { cmd = "l" } else { cmd = "L" }
			}
		case "H":
			x := nextNum()
			if rel { x += cx }
			mark(x, cy)
			cx = x
		case "V":
			y := nextNum()
			if rel { y += cy }
			mark(cx, y)
			cy = y
		case "C":
			x1, y1 := nextNum(), nextNum()
			x2, y2 := nextNum(), nextNum()
			x, y := nextNum(), nextNum()
			if rel { x1 += cx; y1 += cy; x2 += cx; y2 += cy; x += cx; y += cy }
			mark(x1, y1); mark(x2, y2); mark(x, y)
			cx, cy = x, y
		case "S", "Q":
			x1, y1 := nextNum(), nextNum()
			x, y := nextNum(), nextNum()
			if rel { x1 += cx; y1 += cy; x += cx; y += cy }
			mark(x1, y1); mark(x, y)
			cx, cy = x, y
		case "A":
			// rx, ry, x-axis-rotation
			nextNum(); nextNum(); nextNum()
			// large-arc-flag, sweep-flag (single '0'/'1' chars, may
			// be packed with the next coord in compact notation)
			consumeArcFlag(); consumeArcFlag()
			// endpoint
			x, y := nextNum(), nextNum()
			if rel { x += cx; y += cy }
			mark(x, y)
			cx, cy = x, y
		case "Z":
			// no-op
		default:
			i++
		}
	}

	if math.IsInf(minX, 1) {
		return BBox{}
	}
	return BBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
}

// ContentFitTransform returns an SVG transform string that scales and
// centers a path's actual content into a target rectangle.
func ContentFitTransform(d string, tx, ty, tw, th float64) string {
	bb := PathBBox(d)
	bw, bh := bb.Width(), bb.Height()
	if bw <= 0 || bh <= 0 {
		return ""
	}
	scale := math.Min(tw/bw, th/bh)
	ox := tx + (tw-bw*scale)/2 - bb.MinX*scale
	oy := ty + (th-bh*scale)/2 - bb.MinY*scale
	return fmt.Sprintf("translate(%g,%g) scale(%g)", ox, oy, scale)
}

// isLetter reports whether b is a Unicode letter (used to detect SVG path
// commands like M/L/C).
func isLetter(b byte) bool {
	return unicode.IsLetter(rune(b))
}
