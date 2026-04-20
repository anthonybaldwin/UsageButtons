package render

import "testing"

// TestContrastOver_KeepsFgOutsideCollisionZones verifies that a fg
// outside the near-dark / near-white zones is returned unchanged even
// if WCAG ratio would be marginal — the user's chosen text color wins
// unless it would truly blend into a similarly extreme backdrop.
func TestContrastOver_KeepsFgOutsideCollisionZones(t *testing.T) {
	// White on Claude terracotta — WCAG ratio ~3:1 (below AA body-text
	// 4.5) but fg is light, over is mid, so NOT a collision zone. Must
	// keep white — the whole point of the new rule.
	if got := contrastOver("#f9fafb", "#cc7c5e"); got != "#f9fafb" {
		t.Fatalf("white over terracotta: want #f9fafb (no collision), got %s", got)
	}
	// Black on light gray — fg is dark, over is mid-light. Not a
	// collision; keep black.
	if got := contrastOver("#000000", "#cccccc"); got != "#000000" {
		t.Fatalf("black over #cccccc: want #000000 (no collision), got %s", got)
	}
}

// TestContrastOver_FlipsOnDarkDarkCollision verifies that near-black
// fg on a near-black backdrop (Ollama's dark bg case) flips to
// near-white.
func TestContrastOver_FlipsOnDarkDarkCollision(t *testing.T) {
	got := contrastOver("#0a0a0a", "#141414")
	if got != "#f9fafb" {
		t.Fatalf("near-black over near-black: want flip to near-white, got %s", got)
	}
}

// TestContrastOver_FlipsOnLightLightCollision verifies that near-white
// fg on a near-white backdrop (Ollama's brand fill + default text)
// flips to near-black.
func TestContrastOver_FlipsOnLightLightCollision(t *testing.T) {
	got := contrastOver("#f9fafb", "#f7f7f7")
	if got != "#0a0a0a" {
		t.Fatalf("near-white over near-white: want flip to near-black, got %s", got)
	}
}

// TestContrastOver_KeepsDarkFgOnLightBg verifies that dark fg over
// light backdrop is preserved — opposite zones are fine as-is.
func TestContrastOver_KeepsDarkFgOnLightBg(t *testing.T) {
	if got := contrastOver("#0a0a0a", "#f7f7f7"); got != "#0a0a0a" {
		t.Fatalf("dark fg over light bg: want #0a0a0a (no collision), got %s", got)
	}
}

// TestContrastOver_KeepsMidGrayOnMidGray verifies that mid-luminance
// pairs where neither color hits a collision zone stay unchanged. A
// strict WCAG gate would have flipped these; the zone-based rule
// honors the user's choice as long as both colors aren't simultaneously
// dark or simultaneously light.
func TestContrastOver_KeepsMidGrayOnMidGray(t *testing.T) {
	if got := contrastOver("#888888", "#7a7a7a"); got != "#888888" {
		t.Fatalf("mid-gray on mid-gray: want fg unchanged (no collision zone), got %s", got)
	}
}

// TestContrastRatio_KnownExtremes verifies contrastRatio returns
// the canonical WCAG values at the edges: ~21 for black-on-white and
// 1.0 for identical colors.
func TestContrastRatio_KnownExtremes(t *testing.T) {
	black := hexRelativeLuminance("#000000")
	white := hexRelativeLuminance("#ffffff")
	if r := contrastRatio(black, white); r < 20.9 || r > 21.1 {
		t.Fatalf("black vs white contrast ratio: want ~21, got %.3f", r)
	}
	if r := contrastRatio(black, black); r < 0.99 || r > 1.01 {
		t.Fatalf("identical colors contrast ratio: want 1, got %.3f", r)
	}
}

// TestHexRelativeLuminance_MatchesWCAGReference verifies that
// hexRelativeLuminance returns the canonical WCAG relative luminance
// for the sRGB primaries (0.2126 / 0.7152 / 0.0722 for pure red /
// green / blue) and the extremes (0.0 for black, 1.0 for white). A
// gamma-uncorrected implementation would miss these, so this guards
// against accidentally wiring contrastOver to the wrong luminance.
func TestHexRelativeLuminance_MatchesWCAGReference(t *testing.T) {
	// Canonical WCAG values for the sRGB primaries / extremes. A
	// gamma-uncorrected implementation would miss these — e.g. return
	// ~0.59 for pure green instead of ~0.7152.
	cases := []struct {
		hex  string
		want float64
	}{
		{"#000000", 0.0},
		{"#ffffff", 1.0},
		{"#ff0000", 0.2126},
		{"#00ff00", 0.7152},
		{"#0000ff", 0.0722},
	}
	for _, c := range cases {
		got := hexRelativeLuminance(c.hex)
		if got < c.want-0.001 || got > c.want+0.001 {
			t.Errorf("%s: want ~%.4f, got %.4f", c.hex, c.want, got)
		}
	}
}
