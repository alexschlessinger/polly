package termimg

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

func TestLogoImageSlot(t *testing.T) {
	width, height := embeddedLogoDims()
	if width != 418 || height != 418 {
		t.Fatalf("embedded logo dims = %dx%d, want 418x418", width, height)
	}

	logo := LogoImage()
	if logo.Embedded != embeddedLogoAsset || logo.Path != "" {
		t.Fatalf("logo slot source = %+v", logo)
	}
	_, maxRows := style.ImageBounds(logo)
	if maxRows != LogoArtRows {
		t.Fatalf("logo slot rows = %d, want %d", maxRows, LogoArtRows)
	}
	// A square source in 12 rows of 10x20 cells fits by height: 240px tall,
	// so 240px ≈ 24 columns wide.
	cols, rows, fitByRows := CellGeometry(logo, 80, maxRows, 10, 20)
	if cols != 24 || rows != LogoArtRows || !fitByRows {
		t.Fatalf("logo geometry = %dx%d fitByRows=%v, want 24x%d true", cols, rows, fitByRows, LogoArtRows)
	}
	if _, _, ok := CellGeometry(logo, style.MinimumThumbnailCols-1, maxRows, 10, 20); ok {
		t.Fatal("expected no logo geometry on a slot too narrow for a thumbnail")
	}
}

func TestLoadPlacementImageEmbedded(t *testing.T) {
	img, err := loadPlacementImage(Placement{Embedded: embeddedLogoAsset})
	if err != nil {
		t.Fatal(err)
	}
	if bounds := img.Bounds(); bounds.Dx() != 418 || bounds.Dy() != 418 {
		t.Fatalf("decoded embedded logo = %dx%d, want 418x418", bounds.Dx(), bounds.Dy())
	}
	if _, err := loadPlacementImage(Placement{Embedded: "nope"}); err == nil {
		t.Fatal("unknown embedded asset should error")
	}
	version := placementImageVersion(Placement{Embedded: embeddedLogoAsset})
	if !strings.HasPrefix(version, "embedded:logo:") {
		t.Fatalf("embedded version = %q", version)
	}
}
