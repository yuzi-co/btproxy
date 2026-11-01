//go:build ignore

// gen_icon draws the tray icon, a white Bluetooth rune on a blue disc, into btproxy.ico (16, 24 and 32
// pixels, PNG-compressed). Run: go run gen_icon.go
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func main() {
	sizes := []int{16, 24, 32}
	var imgs [][]byte
	for _, n := range sizes {
		var b bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&b, draw(n)); err != nil {
			panic(err)
		}
		imgs = append(imgs, b.Bytes())
	}
	var out bytes.Buffer
	le := func(v any) { binary.Write(&out, binary.LittleEndian, v) }
	le([3]uint16{0, 1, uint16(len(sizes))}) // ICONDIR: reserved, type icon, count
	off := 6 + 16*len(sizes)
	for i, n := range sizes {
		le([4]uint8{uint8(n), uint8(n), 0, 0}) // width, height, palette, reserved
		le([2]uint16{1, 32})                   // planes, bits per pixel
		le([2]uint32{uint32(len(imgs[i])), uint32(off)})
		off += len(imgs[i])
	}
	for _, b := range imgs {
		out.Write(b)
	}
	if err := os.WriteFile("btproxy.ico", out.Bytes(), 0o644); err != nil {
		panic(err)
	}
}

// The rune's strokes in a unit square.
var strokes = [][2][2]float64{
	{{0.50, 0.17}, {0.50, 0.83}}, // spine
	{{0.50, 0.17}, {0.70, 0.34}},
	{{0.70, 0.34}, {0.30, 0.66}},
	{{0.30, 0.34}, {0.70, 0.66}},
	{{0.70, 0.66}, {0.50, 0.83}},
}

func draw(n int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	width := math.Max(0.085, 1.4/float64(n)) // stroke width, at least 1.4 pixels
	const ss = 8                             // samples per axis
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var disc, rune_ float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := (float64(x) + (float64(sx)+0.5)/ss) / float64(n)
					py := (float64(y) + (float64(sy)+0.5)/ss) / float64(n)
					if math.Hypot(px-0.5, py-0.5) <= 0.5 {
						disc++
						for _, s := range strokes {
							if segDist(px, py, s) <= width/2 {
								rune_++
								break
							}
						}
					}
				}
			}
			a := disc / (ss * ss)
			if a == 0 {
				continue
			}
			w := rune_ / disc // share of white within the disc
			blue := [3]float64{0x15, 0x65, 0xc0}
			c := func(i int) uint8 { return uint8(math.Round(blue[i]*(1-w) + 255*w)) }
			img.SetNRGBA(x, y, color.NRGBA{c(0), c(1), c(2), uint8(math.Round(a * 255))})
		}
	}
	return img
}

func segDist(px, py float64, s [2][2]float64) float64 {
	ax, ay, bx, by := s[0][0], s[0][1], s[1][0], s[1][1]
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}
