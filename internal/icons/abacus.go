package icons

import "github.com/anthonybaldwin/UsageButtons/internal/render"

func init() {
	ProviderIcons["abacus"] = &render.ProviderGlyph{
		ViewBox: "0 0 100 100",
		Markup: `<g transform="translate(15, 10)" fill="currentColor">
  <rect x="0" y="0" width="4" height="80" rx="2"/>
  <rect x="17.5" y="0" width="4" height="80" rx="2"/>
  <rect x="35" y="0" width="4" height="80" rx="2"/>
  <rect x="52.5" y="0" width="4" height="80" rx="2"/>
  <rect x="66" y="0" width="4" height="80" rx="2"/>
  <circle cx="2" cy="20" r="7"/>
  <circle cx="19.5" cy="45" r="7"/>
  <circle cx="37" cy="30" r="7"/>
  <circle cx="54.5" cy="60" r="7"/>
  <circle cx="68" cy="40" r="7"/>
</g>`,
	}
}
