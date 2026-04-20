package render

import "testing"

func TestContrastOver_KeepsHighContrastFg(t *testing.T) {
	// Black on mid-gray (#444) already clears WCAG AA; the earlier
	// luminance-delta heuristic would flip this to near-white, which
	// is actually worse — guard against that regression.
	got := contrastOver("#000000", "#444444")
	if got != "#000000" {
		t.Fatalf("black over #444444: want #000000 (already high-contrast), got %s", got)
	}
}

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

func TestContrastOver_StaysOnLightFill(t *testing.T) {
	// Black text over Ollama's light brand fill — already high
	// contrast, must not flip.
	got := contrastOver("#0a0a0a", "#f7f7f7")
	if got != "#0a0a0a" {
		t.Fatalf("black over light fill: want #0a0a0a, got %s", got)
	}
}

func TestContrastOver_PicksBetterOfDarkOrLight(t *testing.T) {
	// For a mid-luminance over where neither dark nor light is ideal,
	// ensure we return whichever of dark/light yields higher contrast.
	over := "#7a7a7a"
	fg := "#888888" // blends with `over`
	got := contrastOver(fg, over)
	if got == fg {
		t.Fatalf("blending fg should flip, got same color %s", got)
	}
	overLum := hexLuminance(over)
	darkRatio := contrastRatio(hexLuminance("#0a0a0a"), overLum)
	lightRatio := contrastRatio(hexLuminance("#f9fafb"), overLum)
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

func TestContrastRatio_KnownExtremes(t *testing.T) {
	black := hexLuminance("#000000")
	white := hexLuminance("#ffffff")
	if r := contrastRatio(black, white); r < 20.9 || r > 21.1 {
		t.Fatalf("black vs white contrast ratio: want ~21, got %.3f", r)
	}
	if r := contrastRatio(black, black); r < 0.99 || r > 1.01 {
		t.Fatalf("identical colors contrast ratio: want 1, got %.3f", r)
	}
}
