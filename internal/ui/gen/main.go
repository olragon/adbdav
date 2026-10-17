// Command gen draws the menu bar glyph.
//
// macOS template icons are black-on-transparent and are recoloured by the
// system for light, dark and highlighted menu bars, so only the alpha channel
// carries shape. Run with: go run ./internal/ui/gen
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
)

const (
	size   = 44 // 22pt at 2x
	bodyX0 = 12
	bodyX1 = 32
	bodyY0 = 4
	bodyY1 = 40
	stroke = 3
	radius = 5
)

// inRounded reports whether (x,y) lies inside a rounded rectangle.
func inRounded(x, y, x0, y0, x1, y1, r int) bool {
	if x < x0 || x >= x1 || y < y0 || y >= y1 {
		return false
	}
	cx, cy := x, y
	switch {
	case x < x0+r:
		cx = x0 + r
	case x >= x1-r:
		cx = x1 - r - 1
	}
	switch {
	case y < y0+r:
		cy = y0 + r
	case y >= y1-r:
		cy = y1 - r - 1
	}
	if cx == x && cy == y {
		return true
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func main() {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			outer := inRounded(x, y, bodyX0, bodyY0, bodyX1, bodyY1, radius)
			inner := inRounded(x, y, bodyX0+stroke, bodyY0+stroke, bodyX1-stroke, bodyY1-stroke, radius-1)
			// The home bar sits inside the screen, so it stays visible.
			homeBar := x >= 18 && x < 26 && y >= 33 && y < 36
			if (outer && !inner) || homeBar {
				img.Set(x, y, color.NRGBA{0, 0, 0, 255})
			}
		}
	}
	f, err := os.Create("internal/ui/icon.png")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
}
