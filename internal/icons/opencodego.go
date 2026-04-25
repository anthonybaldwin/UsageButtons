package icons

import "github.com/anthonybaldwin/UsageButtons/internal/render"

func init() {
	ProviderIcons["opencodego"] = &render.ProviderGlyph{
		ViewBox: "0 0 100 100",
		Markup:  `<path fill="currentColor" fill-rule="evenodd" clip-rule="evenodd" d="M80 88H20V12H80V88ZM35 27H65V72H35V27Z"/>`,
	}
}
