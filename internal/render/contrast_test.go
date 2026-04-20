package render

import "testing"

// TestContrastOver_KeepsHighContrastFg verifies that a foreground
// already meeting WCAG AA (4.5:1) against the backdrop is returned
// unchanged, and that a backdrop which fails the gate flips the fg
// to whichever of near-black / near-white has higher contrast.
func TestContrastOver_KeepsHighContrastFg(t *testing.T) {
	// Black on light gray (#cccccc) is ~13:1 — well above AA — so the
	// caller-supplied fg must be preserved. The earlier luminance-
	// delta heuristic could have flipped pairs with small delta but
	// still-fine contrast; WCAG ratio gates on the actual ratio.
	got := contrastOver("#000000", "#cccccc")
	if got != "#000000" {
		t.Fatalf("black over #cccccc: want #000000 (already high-contrast), got %s", got)
	}

	// #444 under WCAG is actually a bad bg for black (~2.2:1, below
	// AA), so contrastOver must flip — and the flip should land on
	// near-white, which is the higher-contrast choice against #444
	// (~9.5:1 vs ~2.2:1 for near-black).
	if got := contrastOver("#000000", "#444444"); got != "#f9fafb" {
		t.Fatalf("black over #444444 (actually low-contrast under WCAG): want flip to #f9fafb, got %s", got)
	}
}

// TestContrastOver_FlipsWhenFgBlends verifies that a near-black
// foreground on a near-black backdrop (Ollama's dark bg case) flips
// to near-white so the text stays readable.
func TestContrastOver_FlipsWhenFgBlends(t *testing.T) {
	// Near-black on Ollama's dark bg — low contrast, must flip.
	got := contrastOver("#0a0a0a", "#141414")
	if got == "#0a0a0a" {
		t.Fatal("near-black over near-black should flip, got same color back")
	}
	if got != "#f9fafb" {
		t.Fatalf("expected flip to near-white, got %s", got)
	}
}

// TestContrastOver_StaysOnLightFill verifies that a dark foreground
// on a light fill (Ollama's brand fill case) is preserved — the
// user's chosen text color is honored when contrast is fine.
func TestContrastOver_StaysOnLightFill(t *testing.T) {
	// Black text over Ollama's light brand fill — already high
	// contrast, must not flip.
	got := contrastOver("#0a0a0a", "#f7f7f7")
	if got != "#0a0a0a" {
		t.Fatalf("black over light fill: want #0a0a0a, got %s", got)
	}
}

// TestContrastOver_PicksBetterOfDarkOrLight verifies that when the
// caller's fg fails the 4.5:1 gate, the fallback picks whichever of
// near-black / near-white yields the higher contrast ratio against
// the backdrop — not a fixed midpoint rule.
func TestContrastOver_PicksBetterOfDarkOrLight(t *testing.T) {
	// For a mid-luminance over where neither dark nor light is ideal,
	// ensure we return whichever of dark/light yields higher contrast.
	// Must use the same luminance function as production (WCAG
	// relative luminance) so the expectation lines up with the pick.
	over := "#7a7a7a"
	fg := "#888888" // blends with `over`
	got := contrastOver(fg, over)
	if got == fg {
		t.Fatalf("blending fg should flip, got same color %s", got)
	}
	overLum := hexRelativeLuminance(over)
	darkRatio := contrastRatio(hexRelativeLuminance("#0a0a0a"), overLum)
	lightRatio := contrastRatio(hexRelativeLuminance("#f9fafb"), overLum)
	var want string
	if darkRatio >= lightRatio {
		want = "#0a0a0a"
	} else {
		want = "#f9fafb"
	}
	if got != want {
		t.Fatalf("mid-gray over #7a7a7a: want %s (higher contrast), got %s", want, got)
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
