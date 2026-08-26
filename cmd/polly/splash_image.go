package main

import (
	"bytes"
	_ "embed"
	"image"
	"sync"
)

// The image splash replaces the half-block bird when the terminal can draw
// native graphics: the reserved band grows to twelve art rows plus the same
// single blank separator, and the embedded PNG is placed through the ordinary
// thumbnail pipeline. Everything else — short terminals, forced
// POLLYTOOL_IMAGE_PROTOCOL=none, tmux — keeps the half-block art.
const (
	imageLogoArtRows = 12
	imageLogoHeight  = imageLogoArtRows + 1
)

//go:embed assets/logo.png
var embeddedLogoPNG []byte

const embeddedLogoAsset = "logo"

// embeddedTerminalImages resolves placement asset names to compile-time image
// bytes. Placements stay comparable structs; only the manager dereferences
// this registry.
var embeddedTerminalImages = map[string][]byte{
	embeddedLogoAsset: embeddedLogoPNG,
}

var embeddedLogoDims = sync.OnceValues(func() (int, int) {
	config, _, err := image.DecodeConfig(bytes.NewReader(embeddedLogoPNG))
	if err != nil {
		return 0, 0
	}
	return config.Width, config.Height
})

// startupLogoPlacement fits and horizontally centers the embedded logo in the
// reserved splash band. ok is false when the terminal is too narrow for a
// legible image; the band then simply stays blank.
func startupLogoPlacement(width, cellWidth, cellHeight int) (terminalImagePlacement, bool) {
	logoWidth, logoHeight := embeddedLogoDims()
	if logoWidth <= 0 || logoHeight <= 0 {
		return terminalImagePlacement{}, false
	}
	cols, rows, fitByRows := imageCellGeometry(
		transcriptImage{Width: logoWidth, Height: logoHeight},
		width, imageLogoArtRows, cellWidth, cellHeight,
	)
	if cols <= 0 || rows <= 0 {
		return terminalImagePlacement{}, false
	}
	return terminalImagePlacement{
		Key:       "logo",
		Embedded:  embeddedLogoAsset,
		X:         (width - cols) / 2,
		Y:         0,
		Cols:      cols,
		Rows:      rows,
		FitByRows: fitByRows,
	}, true
}
