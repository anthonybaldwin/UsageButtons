//go:build ignore

// Render the Usage Buttons Helper extension icons (16/48/128 PNG) from
// the same four-bar motif as io.github.anthonybaldwin.UsageButtons.sdPlugin/
// assets/plugin-icon.svg. Chrome extensions require PNG icons; this
// script produces them without an external tool chain.
//
// Run:
//   go run scripts/gen-extension-icons.go
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

// Four bars, proportions taken verbatim from plugin-icon.svg
// (viewBox 0..256, bar x/y/w/h). Colors match.
type bar struct {
	x, y, w, h float64
	col        color.RGBA
}

var bars = []bar{
	{48, 160, 32, 56, color.RGBA{0x6e, 0xe7, 0xb7, 0xff}},
	{96, 120, 32, 96, color.RGBA{0x60, 0xa5, 0xfa, 0xff}},
	{144, 80, 32, 136, color.RGBA{0xfb, 0xbf, 0x24, 0xff}},
	{192, 48, 32, 168, color.RGBA{0xf8, 0x71, 0x71, 0xff}},
}

var (
	bg          = color.RGBA{0x1f, 0x29, 0x37, 0xff}
	accent      = color.RGBA{0x22, 0xc5, 0x5e, 0xff} // "connection live" dot
	accentRing  = color.RGBA{0x14, 0x53, 0x2e, 0xff}
	transparent = color.RGBA{}
)

const srcSize = 256.0

func render(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	// Fill background.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.SetRGBA(x, y, bg)
		}
	}
	scale := float64(size) / srcSize
	for _, b := range bars {
		x0 := int(b.x * scale)
		y0 := int(b.y * scale)
		x1 := int((b.x + b.w) * scale)
		y1 := int((b.y + b.h) * scale)
		if x0 < 0 {
			x0 = 0
		}
		if y0 < 0 {
			y0 = 0
		}
		if x1 > size {
			x1 = size
		}
		if y1 > size {
			y1 = size
		}
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetRGBA(x, y, b.col)
			}
		}
	}
	// Accent dot top-right to signal "helper / connected" — drop for
	// 16px because it muddies the shape at that resolution.
	if size >= 32 {
		drawAccent(img, size)
	}
	// Rounded corners: punch transparent pixels on sizes that can
	// afford it (48+). The outer PNG alpha channel carries through.
	if size >= 48 {
		radius := int(float64(size) * 0.08)
		roundCorners(img, size, radius)
	}
	return img
}

func drawAccent(img *image.RGBA, size int) {
	scale := float64(size) / srcSize
	cx := int(218 * scale)
	cy := int(38 * scale)
	rOuter := int(24 * scale)
	rInner := rOuter - int(6*scale)
	if rInner < 2 {
		rInner = 2
	}
	for y := cy - rOuter; y <= cy+rOuter; y++ {
		for x := cx - rOuter; x <= cx+rOuter; x++ {
			if x < 0 || y < 0 || x >= size || y >= size {
				continue
			}
			dx, dy := x-cx, y-cy
			d2 := dx*dx + dy*dy
			switch {
			case d2 <= rInner*rInner:
				img.SetRGBA(x, y, accent)
			case d2 <= rOuter*rOuter:
				img.SetRGBA(x, y, accentRing)
			}
		}
	}
}

func roundCorners(img *image.RGBA, size, r int) {
	corners := [][2]int{
		{r, r}, {size - 1 - r, r}, {r, size - 1 - r}, {size - 1 - r, size - 1 - r},
	}
	for _, c := range corners {
		cx, cy := c[0], c[1]
		for y := cy - r; y <= cy+r; y++ {
			for x := cx - r; x <= cx+r; x++ {
				if x < 0 || y < 0 || x >= size || y >= size {
					continue
				}
				// Only affect the pixels *outside* the corner radius
				// — i.e. the actual corner square of the canvas.
				inCorner := (cx == r && cy == r && x < cx && y < cy) ||
					(cx != r && cy == r && x > cx && y < cy) ||
					(cx == r && cy != r && x < cx && y > cy) ||
					(cx != r && cy != r && x > cx && y > cy)
				if !inCorner {
					continue
				}
				dx, dy := x-cx, y-cy
				if dx*dx+dy*dy > r*r {
					img.SetRGBA(x, y, transparent)
				}
			}
		}
	}
}

func main() {
	outDir := filepath.Join("chrome-extension", "icons")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, size := range []int{16, 48, 128} {
		img := render(size)
		name := filepath.Join(outDir, ("icon-" + (func() string {
			switch size {
			case 16:
				return "16"
			case 48:
				return "48"
			case 128:
				return "128"
			}
			return ""
		}())) + ".png")
		f, err := os.Create(name)
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			log.Fatal(err)
		}
		_ = f.Close()
		log.Printf("wrote %s", name)
	}
}
