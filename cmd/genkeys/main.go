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
	"amp":        "#dc2626",
	"augment":    "#6366f1",
	"claude":     "#cc7c5e",
	"codex":      "#10A37F",
	"copilot":    "#a855f7",
	"cursor":     "#00bfa5",
	"jetbrains":  "#ff3399",
	"kimi":       "#fe603c",
	"kilo":       "#f27027",
	"kiro":       "#ff9900",
	"minimax":    "#fe603c",
	"mistral":    "#ff500f",
	"ollama":     "#f9fafb",
	"opencode":   "#3b82f6",
	"opencodego": "#3b82f6",
	"openrouter": "#6467f2",
	"perplexity": "#20b2aa",
	"synthetic":  "#141414",
	"warp":       "#938bb4",
	"zai":        "#e85a6a",
	"kimi-k2":    "#4c00ff",
}

func main() {
	dir := "io.github.anthonybaldwin.UsageButtons.sdPlugin/assets"
	for id, glyph := range icons.ProviderIcons {
		color, ok := providerColors[id]
		if !ok {
			fmt.Fprintf(os.Stderr, "skip %s: no color\n", id)
			continue
		}
		if id != "codex" {
			xf := render.ContentFitGlyphTransform(glyph, 36, 38, 72, 72)
			body := glyphKeyMarkup(glyph, color)
			svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 144 144">
  <rect width="144" height="144" rx="16" fill="#111827"/>
  <g transform="%s" opacity="0.85">
    %s
  </g>
</svg>
`, xf, body)
			writeGenerated(filepath.Join(dir, fmt.Sprintf("action-%s-key.svg", id)), svg)
		}

		menuPath := filepath.Join(dir, fmt.Sprintf("action-%s.svg", id))
		if _, err := os.Stat(menuPath); os.IsNotExist(err) {
			menuXf := render.ContentFitGlyphTransform(glyph, 2, 2, 16, 16)
			menu := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20">
  <g transform="%s" opacity="0.9">
    %s
  </g>
</svg>
`, menuXf, glyphKeyMarkup(glyph, "#d1d5db"))
			writeGenerated(menuPath, menu)
		}
	}
}

func writeGenerated(path, content string) {
	if existing, err := os.ReadFile(path); err == nil {
		normalized := strings.ReplaceAll(string(existing), "\r\n", "\n")
		if normalized == content {
			return
		}
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
	} else {
		fmt.Printf("wrote %s\n", path)
	}
}

func glyphKeyMarkup(glyph *render.ProviderGlyph, color string) string {
	if glyph.Markup != "" {
		return fmt.Sprintf(`<g color="%s">%s</g>`, color, glyph.Markup)
	}
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
