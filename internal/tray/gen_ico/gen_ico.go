// gen_ico generates a minimal monochrome engram.ico with 16x16 and 32x32 images.
// The icon depicts a simple memory/brain glyph (solid squares pattern) in white
// on a transparent black background.
//
// Usage (run once; the generated file is checked in):
//
//	cd internal/tray && go run gen_ico/gen_ico.go
//
// The generated .ico is a standard Windows Icon format:
//   - ICO header (6 bytes)
//   - 2 ICONDIRENTRY records (16 bytes each)
//   - 2 BITMAPINFOHEADER + AND/XOR bitmaps (one per size)
//
// This tool has no external dependencies. It is NOT part of the engram binary.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

func main() {
	ico, err := buildICO()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen_ico:", err)
		os.Exit(1)
	}
	if err := os.WriteFile("engram.ico", ico, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen_ico: write:", err)
		os.Exit(1)
	}
	fmt.Println("gen_ico: wrote engram.ico")
}

// buildICO builds a 2-image (16x16, 32x32) monochrome ICO file.
// Format: ICONDIR header + 2 ICONDIRENTRY + 2 BITMAPINFOHEADER blocks.
func buildICO() ([]byte, error) {
	img16 := buildBMPBlock(16)
	img32 := buildBMPBlock(32)

	const headerSize = 6
	const entrySize = 16
	offset0 := headerSize + 2*entrySize
	offset1 := offset0 + len(img16)

	out := make([]byte, 0, headerSize+2*entrySize+len(img16)+len(img32))

	// ICONDIR header
	out = leU16(out, 0) // reserved (0)
	out = leU16(out, 1) // type: 1 = ICO
	out = leU16(out, 2) // image count: 2

	// ICONDIRENTRY for 16x16
	out = append(out, 16, 16) // width, height
	out = append(out, 2, 0)   // colorCount, reserved
	out = leU16(out, 0)       // planes
	out = leU16(out, 1)       // bit count
	out = leU32(out, uint32(len(img16)))
	out = leU32(out, uint32(offset0))

	// ICONDIRENTRY for 32x32
	out = append(out, 32, 32) // width, height (0 means 256 for 256x256, here it's 32)
	out = append(out, 2, 0)   // colorCount, reserved
	out = leU16(out, 0)       // planes
	out = leU16(out, 1)       // bit count
	out = leU32(out, uint32(len(img32)))
	out = leU32(out, uint32(offset1))

	out = append(out, img16...)
	out = append(out, img32...)
	return out, nil
}

// buildBMPBlock builds the BITMAPINFOHEADER + color table + XOR bitmap + AND
// bitmap for a monochrome square image of size n×n.
//
// The design is a centered "brain/node" glyph:
//   - Inner square of (n/2 x n/2) solid white pixels in the center
//   - Border ring of 2 pixels at n/4 offset from center
//
// This is visible at 16x16 and 32x32 without anti-aliasing.
func buildBMPBlock(n int) []byte {
	// Color table: 2 entries × 4 bytes (RGBQUAD)
	// Index 0 = black (transparent when combined with AND mask)
	// Index 1 = white
	colorTable := []byte{
		0, 0, 0, 0, // black (BGRA)
		255, 255, 255, 0, // white
	}

	// XOR bitmap: each row is padded to a multiple of 4 bytes.
	// Monochrome: 1 bit per pixel, 8 pixels per byte.
	rowBytes := (n + 31) / 32 * 4 // DWORD-aligned
	xorBitmap := make([]byte, rowBytes*n)

	// Draw a simple solid square glyph in the center.
	// Center area: from n/4 to 3*n/4 in both dimensions.
	lo := n / 4
	hi := (3 * n) / 4
	for y := lo; y < hi; y++ {
		for x := lo; x < hi; x++ {
			// ICO bitmaps are bottom-up: flip the y.
			row := n - 1 - y
			byteIdx := row*rowBytes + x/8
			bitIdx := 7 - (x % 8)
			xorBitmap[byteIdx] |= 1 << bitIdx
		}
	}

	// AND bitmap: 0 = opaque, 1 = transparent (same layout as XOR).
	// We want the inner square opaque (0) and outside transparent (1).
	andBitmap := make([]byte, rowBytes*n)
	for row := range n {
		// Set all bits to 1 (transparent) then clear the visible square.
		for b := range rowBytes {
			andBitmap[row*rowBytes+b] = 0xFF
		}
	}
	for y := lo; y < hi; y++ {
		row := n - 1 - y
		for x := lo; x < hi; x++ {
			byteIdx := row*rowBytes + x/8
			bitIdx := 7 - (x % 8)
			andBitmap[byteIdx] &^= 1 << bitIdx // clear → opaque
		}
	}

	// BITMAPINFOHEADER (40 bytes). Height is n*2 (XOR+AND combined).
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[0:], 40)          // biSize
	binary.LittleEndian.PutUint32(hdr[4:], uint32(n))   // biWidth
	binary.LittleEndian.PutUint32(hdr[8:], uint32(2*n)) // biHeight (XOR+AND)
	binary.LittleEndian.PutUint16(hdr[12:], 1)          // biPlanes
	binary.LittleEndian.PutUint16(hdr[14:], 1)          // biBitCount (monochrome)
	// biCompression, biSizeImage, etc.: all 0 (BI_RGB, auto-size)

	out := make([]byte, 0, 40+len(colorTable)+len(xorBitmap)+len(andBitmap))
	out = append(out, hdr...)
	out = append(out, colorTable...)
	out = append(out, xorBitmap...)
	out = append(out, andBitmap...)
	return out
}

func leU16(dst []byte, v uint16) []byte {
	return append(dst, byte(v), byte(v>>8))
}

func leU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
