package style

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	rw "github.com/mattn/go-runewidth"
)

const (
	// Image markers live only between Markdown rendering and the transcript
	// cell pass. They are replaced with blank cells before gotui reaches the
	// terminal; the native renderer uses their row and column as its anchor.
	transcriptImageMarkerBase rune = '\ue000'
	MaxImagesPerBlock              = 256
	// The marker range must stop short of the styled-literal bracket runes
	// (style.go), or stripping markers would eat bracket placeholders. A
	// range that overlaps makes this constant negative and fails to compile.
	_ = uint(styledLiteralOpenBracket - (transcriptImageMarkerBase + MaxImagesPerBlock))

	// A thumbnail is fitted inside this maximum cell box. The marker template
	// reserves the maximum rows; the cell pass collapses unused rows after it
	// accounts for the image and terminal-cell aspect ratios.
	ThumbnailRows           = 10
	ThumbnailCols           = 50
	InspectionThumbnailRows = 6
	InspectionThumbnailCols = 40
	MinimumThumbnailCols    = 8
)

// Image is deliberately a sidecar to transcript text. Tool and
// message interfaces continue to traffic in strings; only the managed TUI
// interprets explicit local-image references.
type Image struct {
	Path string
	// Embedded names a compile-time asset known to the termimg placement
	// manager; it replaces Path as the pixel source and keeps the slot out of
	// the OS image viewer. Empty for file-backed images.
	Embedded    string
	DisplayPath string
	Alt         string
	Width       int
	Height      int
	Version     string
	Inspection  bool
	MaxCols     int
	MaxRows     int
}

func ImagesEqual(a, b []Image) bool { return slices.Equal(a, b) }

func ImageMarker(index int) rune {
	return transcriptImageMarkerBase + rune(index)
}

func ImageMarkerIndex(r rune) (int, bool) {
	index := int(r - transcriptImageMarkerBase)
	return index, index >= 0 && index < MaxImagesPerBlock
}

// StripImageMarkers removes private marker runes from text that is
// about to share a transcript entry with real image slots, so pasted
// private-use characters cannot pose as slot anchors.
func StripImageMarkers(s string) string {
	return strings.Map(func(r rune) rune {
		if _, ok := ImageMarkerIndex(r); ok {
			return -1
		}
		return r
	}, s)
}

func OffsetImageMarkers(s string, offset int) string {
	if offset == 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		index, ok := ImageMarkerIndex(r)
		if !ok {
			return r
		}
		index += offset
		if index >= MaxImagesPerBlock {
			return -1
		}
		return ImageMarker(index)
	}, s)
}

func ImageBounds(img Image) (int, int) {
	cols, rows := ThumbnailCols, ThumbnailRows
	// A set cap replaces the default rather than tightening it, so the image
	// logo can reserve more rows than an ordinary thumbnail.
	if img.MaxCols > 0 {
		cols = img.MaxCols
	}
	if img.MaxRows > 0 {
		rows = img.MaxRows
	}
	return max(cols, 1), max(rows, 1)
}

func transcriptImageSlot(index int, prefix string, rows int) string {
	line := prefix + string(ImageMarker(index))
	return strings.Repeat(line+"\n", rows-1) + line
}

func ImageCaptionText(img Image) string {
	label := strings.TrimSpace(SanitizeImageText(img.Alt))
	if label == "" {
		label = strings.TrimSpace(SanitizeImageText(filepath.Base(img.Path)))
	}
	if label == "" {
		label = "image"
	}
	label = Truncate(label, 80)
	if img.Inspection {
		parts := []string{"viewed", label}
		if img.Width > 0 && img.Height > 0 {
			parts = append(parts, fmt.Sprintf("%d×%d", img.Width, img.Height))
		}
		return strings.Join(parts, " · ")
	}
	displayPath := SanitizeImageText(img.DisplayPath)
	if displayPath == "" {
		displayPath = SanitizeImageText(img.Path)
	}
	return label + " · " + Truncate(displayPath, 100)
}

func SanitizeImageText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, StripImageMarkers(text))
}

func ImageCaption(img Image) string {
	return Styled(ImageCaptionText(img), "muted", "")
}

func RenderImage(index int, img Image, prefix string, leadingNewline, trailingNewline bool) string {
	var b strings.Builder
	if leadingNewline {
		b.WriteByte('\n')
	}
	b.WriteString(prefix)
	b.WriteString(ImageCaption(img))
	if img.Path == "" && img.Embedded == "" {
		if trailingNewline {
			b.WriteByte('\n')
		}
		return b.String()
	}
	b.WriteByte('\n')
	_, rows := ImageBounds(img)
	b.WriteString(transcriptImageSlot(index, prefix, rows))
	if trailingNewline {
		b.WriteByte('\n')
	}
	return b.String()
}

func RenderImages(images []Image, prefix string) string {
	var blocks []string
	for i, img := range images {
		if i >= MaxImagesPerBlock {
			break
		}
		blocks = append(blocks, RenderImage(i, img, prefix, false, false))
	}
	return strings.Join(blocks, "\n")
}

// RailBar is the bare muted bar under a disclosure row, and Rail the same bar
// followed by the space that separates it from a line's content. Every line
// hanging from a disclosure, in the transcript or the inspector, sits behind
// one of them.
var (
	RailBar = "  " + Styled("│", "muted", "")
	Rail    = RailBar + " "
)

// RenderInspectionImages gives model-viewed media its own subtle
// rail beneath the Images disclosure. The rail is text-layer chrome; native
// Kitty/Sixel placements begin immediately to its right.
func RenderInspectionImages(images []Image) string {
	return RenderImages(images, Rail)
}

// Truncate keeps the first line of s within width cells, marking the cut.
func Truncate(s string, width int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return rw.Truncate(s, width, "...")
}
