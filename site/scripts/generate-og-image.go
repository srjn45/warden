// generate-og-image composes the social card from the current, isolated-demo
// TUI capture. It intentionally does not draw/recreate any product UI: every
// UI pixel comes from site/public/media/cockpit.png, produced by the
// checked-in demo capture rig.
//
// Usage: GO111MODULE=off go run ./site/scripts/generate-og-image.go
package main

import (
	"image"
	"image/draw"
	"image/png"
	"log"
	"os"
)

const (
	input  = "site/public/media/cockpit.png"
	output = "site/public/og-image.png"
	width  = 1200
	height = 630
)

func main() {
	f, err := os.Open(input)
	if err != nil {
		log.Fatal(err)
	}
	src, err := png.Decode(f)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		log.Fatal(err)
	}

	// Crop to the active upper-left cockpit (the lower half is intentionally
	// empty terminal space), then preserve that crop's 1.904:1 aspect ratio in
	// the standard 1200x630 social-card canvas. Nearest-neighbour keeps its
	// terminal glyphs crisp at this small marketing-card scale.
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(image.Black), image.Point{}, draw.Src)
	sb := src.Bounds()
	srcRect := image.Rect(sb.Min.X, sb.Min.Y, sb.Min.X+1428, sb.Min.Y+750).Intersect(sb)
	scaleW, scaleH := width, srcRect.Dy()*width/srcRect.Dx()
	if scaleH > height {
		scaleH, scaleW = height, srcRect.Dx()*height/srcRect.Dy()
	}
	x0, y0 := (width-scaleW)/2, (height-scaleH)/2
	for y := 0; y < scaleH; y++ {
		sy := srcRect.Min.Y + y*srcRect.Dy()/scaleH
		for x := 0; x < scaleW; x++ {
			sx := srcRect.Min.X + x*srcRect.Dx()/scaleW
			dst.Set(x0+x, y0+y, src.At(sx, sy))
		}
	}

	out, err := os.Create(output)
	if err != nil {
		log.Fatal(err)
	}
	err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(out, dst)
	if closeErr := out.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		log.Fatal(err)
	}
}
