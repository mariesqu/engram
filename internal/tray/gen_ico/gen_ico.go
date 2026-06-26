// gen_ico generates engram.ico: a gold "neuron" mark (a soma with radiating
// dendrites ending in synaptic boutons) — the engram brand. It renders 16, 32,
// and 48px images as 32-bit BGRA with anti-aliasing via a signed-distance field,
// so the same vector geometry stays crisp at every tray DPI.
//
// Geometry matches internal/webui/static/logo.svg (coordinates ÷32).
//
// Usage (run once; the generated file is checked in):
//
//	cd internal/tray && go run gen_ico/gen_ico.go
//
// No external dependencies — pure standard library.
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

type vec struct{ x, y float64 }

// Neuron geometry in normalized [0,1] coordinates (logo.svg coords / 32).
var (
	soma      = vec{0.5, 0.5}
	rSoma     = 5.0 / 32
	rSpark    = 2.2 / 32
	halfWidth = (1.7 / 32) / 2 // dendrite stroke half-width
	terminals = []struct {
		p vec
		r float64
	}{
		{vec{5.4 / 32, 5.4 / 32}, 1.9 / 32},
		{vec{26.6 / 32, 5.8 / 32}, 1.9 / 32},
		{vec{3.8 / 32, 19.2 / 32}, 1.75 / 32},
		{vec{28.2 / 32, 20.5 / 32}, 1.75 / 32},
		{vec{16.6 / 32, 28.2 / 32}, 1.9 / 32},
	}
)

// Brand colors (straight, not premultiplied).
var (
	gold  = [3]float64{232, 168, 69}  // #e8a845
	spark = [3]float64{245, 201, 122} // #f5c97a — bright memory core
)

func main() {
	if err := os.WriteFile("engram.ico", buildICO(16, 32, 48), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen_ico: write:", err)
		os.Exit(1)
	}
	fmt.Println("gen_ico: wrote engram.ico (neuron, 16/32/48 @ 32bpp)")
}

func buildICO(sizes ...int) []byte {
	blocks := make([][]byte, len(sizes))
	for i, n := range sizes {
		blocks[i] = buildBGRABlock(n)
	}

	const headerSize = 6
	const entrySize = 16
	offset := headerSize + entrySize*len(sizes)

	var out []byte
	out = leU16(out, 0)                  // reserved
	out = leU16(out, 1)                  // type: icon
	out = leU16(out, uint16(len(sizes))) // image count

	for i, n := range sizes {
		b := byte(n)
		if n >= 256 {
			b = 0
		}
		out = append(out, b, b) // width, height
		out = append(out, 0, 0) // colorCount, reserved
		out = leU16(out, 1)     // planes
		out = leU16(out, 32)    // bit count
		out = leU32(out, uint32(len(blocks[i])))
		out = leU32(out, uint32(offset))
		offset += len(blocks[i])
	}
	for _, blk := range blocks {
		out = append(out, blk...)
	}
	return out
}

// buildBGRABlock renders the neuron into a BITMAPINFOHEADER + 32bpp BGRA XOR
// bitmap + a 1bpp all-opaque AND mask (transparency comes from the alpha channel).
func buildBGRABlock(n int) []byte {
	aa := 1.0 / float64(n) // ~1px anti-aliasing band

	// XOR bitmap: n rows of n BGRA pixels, bottom-up.
	xor := make([]byte, n*n*4)
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			u := (float64(px) + 0.5) / float64(n)
			v := (float64(py) + 0.5) / float64(n)
			p := vec{u, v}

			d := dist(p, soma) - rSoma
			for _, t := range terminals {
				if e := dist(p, t.p) - t.r; e < d {
					d = e
				}
				if e := distSeg(p, soma, t.p) - halfWidth; e < d {
					d = e
				}
			}

			alpha := clamp01(0.5 - d/aa)
			if alpha <= 0 {
				continue
			}
			// Bright core blends in near the soma centre.
			sc := clamp01(0.5 - (dist(p, soma)-rSpark)/aa)
			col := [3]float64{
				lerp(gold[0], spark[0], sc),
				lerp(gold[1], spark[1], sc),
				lerp(gold[2], spark[2], sc),
			}

			row := n - 1 - py // ICO bitmaps are bottom-up
			i := (row*n + px) * 4
			xor[i+0] = byte(col[2] + 0.5) // B
			xor[i+1] = byte(col[1] + 0.5) // G
			xor[i+2] = byte(col[0] + 0.5) // R
			xor[i+3] = byte(alpha*255 + 0.5)
		}
	}

	andRow := (n + 31) / 32 * 4
	and := make([]byte, andRow*n) // all zero → opaque; alpha drives transparency

	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[0:], 40)          // biSize
	binary.LittleEndian.PutUint32(hdr[4:], uint32(n))   // biWidth
	binary.LittleEndian.PutUint32(hdr[8:], uint32(2*n)) // biHeight (XOR+AND)
	binary.LittleEndian.PutUint16(hdr[12:], 1)          // biPlanes
	binary.LittleEndian.PutUint16(hdr[14:], 32)         // biBitCount

	out := make([]byte, 0, len(hdr)+len(xor)+len(and))
	out = append(out, hdr...)
	out = append(out, xor...)
	out = append(out, and...)
	return out
}

func dist(a, b vec) float64 { return math.Hypot(a.x-b.x, a.y-b.y) }

func distSeg(p, a, b vec) float64 {
	abx, aby := b.x-a.x, b.y-a.y
	apx, apy := p.x-a.x, p.y-a.y
	denom := abx*abx + aby*aby
	t := 0.0
	if denom > 0 {
		t = (apx*abx + apy*aby) / denom
	}
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return math.Hypot(p.x-(a.x+t*abx), p.y-(a.y+t*aby))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func leU16(dst []byte, v uint16) []byte { return append(dst, byte(v), byte(v>>8)) }
func leU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
