// Package icons holds the SVG glyph data used to render provider logos
// on button faces.
//
// Most provider glyphs are pulled from lobehub/lobe-icons (MIT) by
// scripts/sync-lobe-icons.go and committed into lobe_generated.go.
// Re-run that script to refresh after upstream icon changes.
//
// This file holds the small set of providers whose mark is hand-drawn
// or otherwise not in lobe-icons (e.g., Warp Terminal). Per-provider
// files in this package register the rest of the hand-drawn marks
// (factory robot, jetbrains, abacus, etc.) the same way.
//
// See THIRD_PARTY_LICENSES.md for upstream attributions.
package icons

import "github.com/anthonybaldwin/UsageButtons/internal/render"

// ProviderIcons maps provider IDs to their SVG glyph data. Entries are
// populated from this file's literal, from per-provider <name>.go init
// funcs in this package, and from the lobe-icons sync output in
// lobe_generated.go.
var ProviderIcons = map[string]*render.ProviderGlyph{
	"warp": {
		// Warp Terminal — hand-drawn (lobe-icons does not ship a Warp
		// glyph). Two stacked rounded rectangles forming a shifted
		// "W"-shape silhouette.
		ViewBox: "0 0 100 100",
		D:       "M50.564 9.5L87.855 9.5C93.972 9.5 98.911 14.601 98.911 20.919L98.911 64.355C98.911 70.673 93.972 75.774 87.855 75.774L34.45 75.774L50.564 9.5Z M40.859 22.06L10.828 22.06C4.844 22.06 0 27.06 0 33.2L0 76.637C0 82.777 4.844 87.777 10.828 87.777L47.779 87.777L49.246 82.024L26.408 82.024L40.859 22.06Z",
	},
}
