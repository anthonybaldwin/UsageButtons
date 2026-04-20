package render_test

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/anthonybaldwin/UsageButtons/internal/icons"
	"github.com/anthonybaldwin/UsageButtons/internal/render"
)

// TestGlyphDensityAudit prints each provider glyph's path bbox, the
// declared viewBox, and what fraction of the viewBox the bbox fills.
// Not an assertion — just evidence for sizing decisions. Run with:
//   go test ./internal/render/ -run TestGlyphDensityAudit -v
func TestGlyphDensityAudit(t *testing.T) {
	ids := make([]string, 0, len(icons.ProviderIcons))
	for id := range icons.ProviderIcons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	t.Logf("%-12s  %-14s  %-18s  %-18s  %s",
		"glyph", "viewBox", "bbox w×h", "viewBox w×h", "bbox/vb area %")
	for _, id := range ids {
		g := icons.ProviderIcons[id]
		bb := render.PathBBox(g.D)
		bw, bh := bb.Width(), bb.Height()
		vw, vh := parseViewBoxWH(g.ViewBox)
		pct := 0.0
		if vw > 0 && vh > 0 {
			pct = 100 * (bw * bh) / (vw * vh)
		}
		t.Logf("%-12s  %-14s  %-18s  %-18s  %5.1f%%",
			id, g.ViewBox,
			fmt.Sprintf("%.2f x %.2f", bw, bh),
			fmt.Sprintf("%.2f x %.2f", vw, vh),
			pct)
	}
}

func parseViewBoxWH(vb string) (float64, float64) {
	parts := strings.Fields(vb)
	if len(parts) != 4 {
		return 0, 0
	}
	w, _ := strconv.ParseFloat(parts[2], 64)
	h, _ := strconv.ParseFloat(parts[3], 64)
	return w, h
}
