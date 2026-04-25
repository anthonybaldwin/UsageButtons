package icons

import "github.com/anthonybaldwin/UsageButtons/internal/render"

func init() {
	ProviderIcons["vertexai"] = &render.ProviderGlyph{
		ViewBox: "0 0 100 100",
		Markup:  `<path d="M50 5L90 27.5V72.5L50 95L10 72.5V27.5L50 5Z" fill="none" stroke="white" stroke-width="4"/><path d="M50 25L70 37.5V62.5L50 75L30 62.5V37.5L50 25Z" fill="white"/><circle cx="50" cy="50" r="8" fill="#4285F4"/>`,
	}
}
