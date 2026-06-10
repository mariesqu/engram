//go:build windows

package tray

import _ "embed"

// iconData holds the embedded monochrome .ico binary (16x16 + 32x32 images).
//
// The file was generated once by running:
//
//	cd internal/tray && go run gen_ico/gen_ico.go
//
// gen_ico.go produces a two-image ICO (16×16 + 32×32) using only the Go
// standard library (no external packages). The generator writes BITMAPINFOHEADER
// + monochrome XOR/AND bitmaps per the ICO specification. The glyph is a
// centered solid square (n/4 … 3n/4) — visible at both sizes on any DPI setting.
// To regenerate: cd internal/tray && go run gen_ico/gen_ico.go
//
//go:embed engram.ico
var iconData []byte
