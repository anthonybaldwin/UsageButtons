package main

import (
	"fmt"
	"os"
	"path/filepath"

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
		xf := render.ContentFitTransform(glyph.D, 36, 38, 72, 72)
		svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 144 144">
  <rect width="144" height="144" rx="16" fill="#111827"/>
  <g transform="%s" opacity="0.85">
    <path d="%s" fill="%s"/>
  </g>
</svg>
`, xf, glyph.D, color)
		path := filepath.Join(dir, fmt.Sprintf("action-%s-key.svg", id))
		if err := os.WriteFile(path, []byte(svg), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
		} else {
			fmt.Printf("wrote %s\n", path)
		}
	}
}
