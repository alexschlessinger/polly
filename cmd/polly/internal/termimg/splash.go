package termimg

import (
	"bytes"
	_ "embed"
	"image"
	"sync"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// The image logo lives in the masthead, left of the identity text, exactly
// where the half-block bird sits on terminals without native graphics. The
// masthead reserves LogoArtRows marker rows for it and the placement manager
// draws the embedded PNG through the ordinary thumbnail pipeline: it scrolls
// with the transcript, gives way to modals, and stays out of the OS image
// viewer because the slot has no backing file.
const LogoArtRows = 4

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

// LogoImage describes the embedded logo as a style.Image slot. Width and
// Height come from the PNG; MaxRows makes the slot reserve LogoArtRows
// terminal rows so the logo stays compact beside the text.
func LogoImage() style.Image {
	width, height := embeddedLogoDims()
	return style.Image{
		Embedded: embeddedLogoAsset,
		Width:    width,
		Height:   height,
		MaxRows:  LogoArtRows,
	}
}
