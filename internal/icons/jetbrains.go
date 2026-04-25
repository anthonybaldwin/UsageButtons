package icons

import "github.com/anthonybaldwin/UsageButtons/internal/render"

func init() {
	ProviderIcons["jetbrains"] = &render.ProviderGlyph{
		ViewBox: "0 0 417 417",
		Markup: `<g>
  <path fill="currentColor" d="M298.958,344.271l-24.582,24.582c-13.311,13.254 -31.266,20.73 -50.07,20.73l-168.902,0c-15.633,0 -28.32,-12.688 -28.32,-28.32l0,-168.902c0,-18.805 7.477,-36.816 20.73,-50.07l24.582,-24.582l0,226.563l226.563,0Zm-226.563,-226.563l69.895,-69.895c13.311,-13.254 31.266,-20.73 50.07,-20.73l168.902,0c15.633,0 28.32,12.688 28.32,28.32l0,168.902c0,18.805 -7.477,36.816 -20.73,50.07l-69.895,69.895l0,-226.563l-226.563,0Z"/>
  <rect fill="currentColor" x="100.716" y="293.294" width="96.289" height="22.656"/>
</g>`,
	}
}
