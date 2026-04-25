package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthonybaldwin/UsageButtons/internal/icons"
	"github.com/anthonybaldwin/UsageButtons/internal/render"
)

var providerColors = map[string]string{
	"claude":     "#cc7c5e",
	"codex":      "#10A37F",
	"copilot":    "#1f6feb",
	"cursor":     "#888888",
	"ollama":     "#f9fafb",
	"openrouter": "#6467f2",
	"warp":       "#938bb4",
	"zai":        "#4c00ff",
	"kimi-k2":    "#e85a6a",
}

func main() {
	dir := "io.github.anthonybaldwin.UsageButtons.sdPlugin/assets"
	for id, glyph := range icons.ProviderIcons {
		color, ok := providerColors[id]
		if !ok {
			fmt.Fprintf(os.Stderr, "skip %s: no color\n", id)
			continue
		}
		xf := render.ContentFitGlyphTransform(glyph, 36, 38, 72, 72)
		body := glyphKeyMarkup(glyph, color)
		svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 144 144">
  <rect width="144" height="144" rx="16" fill="#111827"/>
  <g transform="%s" opacity="0.85">
    %s
  </g>
</svg>
`, xf, body)
		path := filepath.Join(dir, fmt.Sprintf("action-%s-key.svg", id))
		if err := os.WriteFile(path, []byte(svg), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
		} else {
			fmt.Printf("wrote %s\n", path)
		}
	}
}

func glyphKeyMarkup(glyph *render.ProviderGlyph, color string) string {
	if len(glyph.Paths) == 0 {
		if glyph.Stroke {
			return fmt.Sprintf(`<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" vector-effect="non-scaling-stroke"/>`, glyph.D, color)
		}
		return fmt.Sprintf(`<path d="%s" fill="%s"/>`, glyph.D, color)
	}
	parts := make([]string, 0, len(glyph.Paths))
	for _, p := range glyph.Paths {
		if p.Stroke {
			width := p.StrokeWidth
			if width <= 0 {
				width = 2
			}
			parts = append(parts, fmt.Sprintf(`<path d="%s" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round" stroke-linejoin="round"/>`, p.D, color, width))
		} else {
			parts = append(parts, fmt.Sprintf(`<path d="%s" fill="%s"/>`, p.D, color))
		}
	}
	return strings.Join(parts, "\n    ")
}
