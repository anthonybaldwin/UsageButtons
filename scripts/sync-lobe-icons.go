//go:build ignore
// +build ignore

// scripts/sync-lobe-icons.go fetches monochrome SVG glyphs from
// lobehub/lobe-icons (MIT) and regenerates
// internal/icons/lobe_generated.go.
//
// Re-run with:
//
//	go run scripts/sync-lobe-icons.go
//
// Add or change entries in `mapping` below — never edit the generated
// output by hand.
package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	lobeBase = "https://raw.githubusercontent.com/lobehub/lobe-icons/master/packages/static-svg/icons/"
	outPath  = "internal/icons/lobe_generated.go"
)

type entry struct {
	ProviderID string // ProviderIcons map key
	LobeName   string // base file name (without -text suffix), e.g. "claudecode"
	Variant    string // "mono" or "text"
	Note       string // freeform note rendered as a comment on the entry
}

// mapping pins each ProviderIcons key to a lobe icon file + variant.
// Variants:
//   - "mono": fetches "<name>.svg"             (currentColor monochrome)
//   - "text": fetches "<name>-text.svg"        (wordmark; gets vertical
//     scale wrap so it fills the squareish watermark zone)
var mapping = []entry{
	{ProviderID: "alibaba", LobeName: "alibaba", Variant: "mono"},
	{ProviderID: "amp", LobeName: "amp", Variant: "mono"},
	{ProviderID: "anthropic", LobeName: "anthropic", Variant: "mono"},
	{ProviderID: "antigravity", LobeName: "antigravity", Variant: "mono"},
	{ProviderID: "claude", LobeName: "claudecode", Variant: "mono",
		Note: "Claude Code product mark (Clawd is just the mascot)."},
	{ProviderID: "codex", LobeName: "codex", Variant: "mono"},
	{ProviderID: "copilot", LobeName: "copilot", Variant: "mono"},
	{ProviderID: "cursor", LobeName: "cursor", Variant: "mono"},
	{ProviderID: "deepseek", LobeName: "deepseek", Variant: "mono"},
	{ProviderID: "gemini", LobeName: "gemini", Variant: "mono"},
	{ProviderID: "grok", LobeName: "grok", Variant: "mono"},
	{ProviderID: "hermes-agent", LobeName: "hermesagent", Variant: "mono"},
	{ProviderID: "kilo", LobeName: "kilocode", Variant: "mono"},
	{ProviderID: "kimi", LobeName: "kimi", Variant: "mono"},
	// Kimrel (provider ID kimi-k2) is a third-party reseller — not allowed
	// to use Kimi's Lobe icon. Custom mark lives in internal/icons/kimrel.go.
	{ProviderID: "minimax", LobeName: "minimax", Variant: "mono"},
	{ProviderID: "mistral", LobeName: "mistral", Variant: "mono"},
	{ProviderID: "moonshot", LobeName: "moonshot", Variant: "mono"},
	{ProviderID: "nousresearch", LobeName: "nousresearch", Variant: "text",
		Note: "NousResearch wordmark; vertical-stretched so it reads at watermark size."},
	{ProviderID: "ollama", LobeName: "ollama", Variant: "mono"},
	{ProviderID: "openai", LobeName: "openai", Variant: "mono",
		Note: "OpenAI rosette (= the canonical ChatGPT brand mark)."},
	{ProviderID: "openclaw", LobeName: "openclaw", Variant: "mono"},
	{ProviderID: "opencode", LobeName: "opencode", Variant: "mono"},
	{ProviderID: "openrouter", LobeName: "openrouter", Variant: "mono"},
	{ProviderID: "perplexity", LobeName: "perplexity", Variant: "mono"},
	{ProviderID: "vertexai", LobeName: "vertexai", Variant: "mono"},
	{ProviderID: "zai", LobeName: "zai", Variant: "mono"},
}

type pathNode struct {
	D        string
	FillRule string // empty | "evenodd" | "nonzero"
}

type parsed struct {
	ViewBox string
	Paths   []pathNode
	// RawInner is the SVG body verbatim, used for "text" variants where
	// we wrap it in a vertical-scale <g> so wordmarks fill the watermark
	// zone rather than rendering at ~half height.
	RawInner string
}

func main() {
	results := make(map[string]parsed, len(mapping))

	type key struct{ name, variant string }
	cache := map[key]parsed{}

	for _, e := range mapping {
		k := key{e.LobeName, e.Variant}
		if p, ok := cache[k]; ok {
			results[e.ProviderID] = p
			continue
		}
		fname := e.LobeName + ".svg"
		if e.Variant == "text" {
			fname = e.LobeName + "-text.svg"
		}
		url := lobeBase + fname
		body, err := fetch(url)
		check(err, "fetch %s", url)
		p, err := parseSVG(body)
		check(err, "parse %s", fname)
		fmt.Fprintf(os.Stderr, "  fetched %s (viewBox=%s, paths=%d)\n",
			fname, p.ViewBox, len(p.Paths))
		cache[k] = p
		results[e.ProviderID] = p
	}

	if err := writeGo(results); err != nil {
		fail("writing %s: %v", outPath, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d entries)\n", outPath, len(mapping))
}

func fetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// parseSVG walks the SVG token stream, extracting viewBox from <svg>
// and every <path d=...> regardless of nesting under <g> / <clipPath>.
// <defs>, <linearGradient>, <radialGradient>, and <stop> subtrees are
// skipped so brand-color gradient stops don't pollute the path list.
func parseSVG(body []byte) (parsed, error) {
	var p parsed
	dec := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	innerStart := -1
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return p, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "svg":
				for _, a := range t.Attr {
					if a.Name.Local == "viewBox" {
						p.ViewBox = a.Value
					}
				}
				innerStart = int(dec.InputOffset())
			case "defs", "linearGradient", "radialGradient", "stop":
				if err := dec.Skip(); err != nil {
					return p, err
				}
				depth--
			case "clipPath":
				if err := dec.Skip(); err != nil {
					return p, err
				}
				depth--
			case "path":
				var pn pathNode
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "d":
						pn.D = a.Value
					case "fill-rule":
						pn.FillRule = a.Value
					case "clip-rule":
						// some lobe paths use clip-rule alone — treat as fill-rule
						if pn.FillRule == "" {
							pn.FillRule = a.Value
						}
					}
				}
				if pn.D != "" {
					p.Paths = append(p.Paths, pn)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "svg" && innerStart >= 0 {
				end := int(dec.InputOffset()) - len("</svg>")
				if end > innerStart {
					p.RawInner = string(body[innerStart:end])
				}
			}
			depth--
		}
	}
	if p.ViewBox == "" {
		return p, fmt.Errorf("no viewBox")
	}
	if len(p.Paths) == 0 && p.RawInner == "" {
		return p, fmt.Errorf("no paths")
	}
	return p, nil
}

func writeGo(results map[string]parsed) error {
	var out bytes.Buffer
	out.WriteString(`// Code generated by scripts/sync-lobe-icons.go; DO NOT EDIT.
//
// Provider glyphs sourced from lobehub/lobe-icons (MIT) — see
// THIRD_PARTY_LICENSES.md for attribution. Edit scripts/sync-lobe-icons.go
// (the mapping table) and re-run "go run scripts/sync-lobe-icons.go" to
// regenerate this file.

package icons

import "github.com/anthonybaldwin/UsageButtons/internal/render"

func init() {
`)

	// stable order
	keys := make([]string, 0, len(mapping))
	keyMap := map[string]entry{}
	for _, e := range mapping {
		keys = append(keys, e.ProviderID)
		keyMap[e.ProviderID] = e
	}
	sort.Strings(keys)

	for _, k := range keys {
		e := keyMap[k]
		p := results[k]
		writeEntry(&out, e, p)
	}
	out.WriteString("}\n")
	return os.WriteFile(outPath, out.Bytes(), 0644)
}

func writeEntry(w *bytes.Buffer, e entry, p parsed) {
	fmt.Fprintf(w, "\t// %s — lobe-icons %s.svg (%s)", e.ProviderID, lobeFile(e), e.Variant)
	if e.Note != "" {
		fmt.Fprintf(w, "\n\t// %s", e.Note)
	}
	fmt.Fprintf(w, "\n\tProviderIcons[%q] = &render.ProviderGlyph{\n", e.ProviderID)

	switch {
	case e.Variant == "text":
		// Wrap in a vertical-stretch group so wordmarks fill the
		// watermark zone vertically — without it they'd render at
		// ~50% height because bbox-fit is width-limited on wide
		// aspects. Expand the emitted viewBox y by the same factor so
		// bbox-fit accounts for the stretched content extent.
		const textStretchY = 1.5
		stretchedVB, err := scaleViewBoxY(p.ViewBox, textStretchY)
		check(err, "scaling viewBox %q", p.ViewBox)
		fmt.Fprintf(w, "\t\tViewBox: %q,\n", stretchedVB)

		var inner strings.Builder
		fmt.Fprintf(&inner, `<g transform="scale(1,%g)">`, textStretchY)
		for _, pn := range p.Paths {
			inner.WriteString(`<path fill="currentColor"`)
			if pn.FillRule != "" {
				inner.WriteString(` fill-rule="` + pn.FillRule + `"`)
			}
			inner.WriteString(` d="` + pn.D + `"/>`)
		}
		inner.WriteString(`</g>`)
		fmt.Fprintf(w, "\t\tMarkup:  %s,\n", goRawString(inner.String()))

	case len(p.Paths) == 1 && p.Paths[0].FillRule == "":
		fmt.Fprintf(w, "\t\tViewBox: %q,\n", p.ViewBox)
		fmt.Fprintf(w, "\t\tD:       %s,\n", goRawString(p.Paths[0].D))

	default:
		fmt.Fprintf(w, "\t\tViewBox: %q,\n", p.ViewBox)
		// multi-path or fill-rule needed → use Paths array. fill-rule is
		// emitted by wrapping the path's data with a Markup field on the
		// glyph; for the simple case (no rule), we go through GlyphPath.
		needsMarkup := false
		for _, pn := range p.Paths {
			if pn.FillRule != "" {
				needsMarkup = true
				break
			}
		}
		if needsMarkup {
			var inner strings.Builder
			for _, pn := range p.Paths {
				inner.WriteString(`<path fill="currentColor"`)
				if pn.FillRule != "" {
					inner.WriteString(` fill-rule="` + pn.FillRule + `"`)
				}
				inner.WriteString(` d="` + pn.D + `"/>`)
			}
			fmt.Fprintf(w, "\t\tMarkup:  %s,\n", goRawString(inner.String()))
		} else {
			w.WriteString("\t\tPaths: []render.GlyphPath{\n")
			for _, pn := range p.Paths {
				fmt.Fprintf(w, "\t\t\t{D: %s},\n", goRawString(pn.D))
			}
			w.WriteString("\t\t},\n")
		}
	}

	w.WriteString("\t}\n")
}

// scaleViewBoxY parses a "minX minY width height" viewBox and returns
// it with the height multiplied by factor (preserving minY). Used so
// the emitted viewBox matches the inner scale(1, factor) wrap on text
// variants, keeping bbox-fit math consistent with rendered extent.
func scaleViewBoxY(vb string, factor float64) (string, error) {
	parts := strings.Fields(vb)
	if len(parts) != 4 {
		return "", fmt.Errorf("malformed viewBox %q", vb)
	}
	h, err := strconv.ParseFloat(parts[3], 64)
	if err != nil {
		return "", fmt.Errorf("parsing viewBox height %q: %w", parts[3], err)
	}
	return fmt.Sprintf("%s %s %s %s", parts[0], parts[1], parts[2],
		strconv.FormatFloat(h*factor, 'f', -1, 64)), nil
}

func lobeFile(e entry) string {
	if e.Variant == "text" {
		return e.LobeName + "-text"
	}
	return e.LobeName
}

// goRawString renders s as a Go string literal. Prefers a backtick
// raw-string when s has no backticks; otherwise falls back to a
// double-quoted strconv-quoted form (which escapes everything safely).
func goRawString(s string) string {
	if !strings.ContainsRune(s, '`') {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

func check(err error, msg string, args ...any) {
	if err != nil {
		fail(msg+": %v", append(args, err)...)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sync-lobe-icons: "+format+"\n", args...)
	os.Exit(1)
}
