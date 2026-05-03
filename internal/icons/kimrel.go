package icons

import "github.com/anthonybaldwin/UsageButtons/internal/render"

// Kimrel (provider ID "kimi-k2") is a third-party reseller of Kimi K2
// model access — not affiliated with Moonshot AI. We deliberately do
// not reuse Lobe's Kimi mark (which the auto-generated lobe_generated.go
// would otherwise pull in for this provider ID).
//
// The mark below is a plain bold sans-serif "K" — visually distinct
// from Kimi's flower-K and from Moonshot's lunar mark, signalling
// "credits balance" without implying official Moonshot/Kimi branding.
//
// File name "kimrel.go" sorts after "lobe_generated.go" so its init
// runs second and overwrites any stale entry the generator might
// re-emit on a future sync.
func init() {
	ProviderIcons["kimi-k2"] = &render.ProviderGlyph{
		ViewBox: "0 0 24 24",
		D:       `M5 4h2.6v7.7L14 4h3.2l-6.2 7.4L17.7 20h-3.2L9.6 13.2 7.6 15.5V20H5V4z`,
	}
}
