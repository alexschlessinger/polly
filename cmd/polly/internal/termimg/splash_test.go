package termimg

import (
	"strings"
	"testing"
)

func TestStartupLogoPlacementGeometry(t *testing.T) {
	width, height := embeddedLogoDims()
	if width != 418 || height != 418 {
		t.Fatalf("embedded logo dims = %dx%d, want 418x418", width, height)
	}

	logo, ok := StartupLogoPlacement(80, 10, 20)
	if !ok {
		t.Fatal("no placement on a roomy terminal")
	}
	// A square source in 12 rows of 10x20 cells fits by height: 240px tall,
	// so 240px ≈ 24 columns wide, horizontally centered in 80 columns.
	if logo.Embedded != embeddedLogoAsset || logo.X != 28 || logo.Y != 0 {
		t.Fatalf("placement anchor = %+v", logo)
	}
	if logo.Rows != LogoArtRows || logo.Cols != 24 || !logo.FitByRows {
		t.Fatalf("placement geometry = %+v, want 24x%d fit-by-rows", logo, LogoArtRows)
	}

	if _, ok := StartupLogoPlacement(4, 10, 20); ok {
		t.Fatal("expected no placement on a terminal too narrow for a thumbnail")
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
