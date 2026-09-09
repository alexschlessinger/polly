package termimg

import (
	"bytes"
	_ "embed"
	"image"
	"sync"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// The image splash replaces the half-block bird when the terminal can draw
// native graphics: the reserved band grows to twelve art rows plus the same
// single blank separator, and the embedded PNG is placed through the ordinary
// thumbnail pipeline. Everything else — short terminals, forced
// POLLYTOOL_IMAGE_PROTOCOL=none, tmux — keeps the half-block art.
const (
	LogoArtRows = 12
	LogoHeight  = LogoArtRows + 1
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

// StartupLogoPlacement fits and horizontally centers the embedded logo in the
// reserved splash band. ok is false when the terminal is too narrow for a
// legible image; the band then simply stays blank.
func StartupLogoPlacement(width, cellWidth, cellHeight int) (Placement, bool) {
	logoWidth, logoHeight := embeddedLogoDims()
	if logoWidth <= 0 || logoHeight <= 0 {
		return Placement{}, false
	}
	cols, rows, fitByRows := CellGeometry(
		style.Image{Width: logoWidth, Height: logoHeight},
		width, LogoArtRows, cellWidth, cellHeight,
	)
	if cols <= 0 || rows <= 0 {
		return Placement{}, false
	}
	return Placement{
		Key:       "logo",
		Embedded:  embeddedLogoAsset,
		X:         (width - cols) / 2,
		Y:         0,
		Cols:      cols,
		Rows:      rows,
		FitByRows: fitByRows,
	}, true
}
