package render

import (
	"strings"
	"testing"
)

func TestStarfield_PresentInSVGWhenEnabled(t *testing.T) {
	svg := RenderButton(ButtonInput{
		Label:     "GROK 3",
		Value:     "139/140",
		Subvalue:  "Queries",
		Bg:        "#000000",
		Fg:        "#ffffff",
		Fill:      "#000000",
		Starfield: true,
	})
	circles := strings.Count(svg, `<circle`)
	if circles < 30 {
		t.Errorf("expected >= 30 starfield circles, got %d", circles)
	}
	if !strings.Contains(svg, `fill="#ffffff"`) {
		t.Errorf("expected stars to be white-filled")
	}
}

func TestStarfield_AbsentWhenDisabled(t *testing.T) {
	svg := RenderButton(ButtonInput{
		Label:    "CLAUDE",
		Value:    "100%",
		Subvalue: "Remaining",
		Bg:       "#1c1210",
		Fg:       "#ffffff",
		Fill:     "#cc7c5e",
	})
	if strings.Count(svg, `<circle`) > 0 {
		t.Errorf("non-Grok button should not have starfield circles")
	}
}
