package main

import (
	"fmt"
	"image"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alexschlessinger/pollytool/images"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const (
	// Image markers live only between Markdown rendering and the transcript
	// cell pass. They are replaced with blank cells before gotui reaches the
	// terminal; the native renderer uses their row and column as its anchor.
	transcriptImageMarkerBase   rune = '\ue000'
	maxTranscriptImagesPerBlock      = 256

	// A thumbnail is fitted inside this maximum cell box. The marker template
	// reserves the maximum rows; the cell pass collapses unused rows after it
	// accounts for the image and terminal-cell aspect ratios.
	transcriptImageThumbnailRows = 10
	transcriptImageThumbnailCols = 50
	minimumImageThumbnailCols    = 8

	maxLocalImageBytes  = images.MaxSourceBytes
	maxLocalImagePixels = images.MaxSourcePixels
)

// transcriptImage is deliberately a sidecar to transcript text. Tool and
// message interfaces continue to traffic in strings; only the managed TUI
// interprets explicit local-image references.
type transcriptImage struct {
	Path        string
	DisplayPath string
	Alt         string
	Width       int
	Height      int
	Version     string
}

type markdownRenderState struct {
	baseDir string
	images  []transcriptImage
}

type transcriptDisplayBlock struct {
	key    string
	text   string
	images []transcriptImage
}

type transcriptImageSpan struct {
	imageIndex int
	row        int
	x          int
	cols       int
	rows       int
	fitByRows  bool
}

type terminalImagePlacement struct {
	Key  string
	Path string
	// Embedded names a compile-time asset in embeddedTerminalImages instead
	// of a file on disk. A string key keeps the struct comparable.
	Embedded   string
	X, Y       int
	Cols, Rows int
	FitByRows  bool
}

func transcriptImagesEqual(a, b []transcriptImage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func transcriptImageMarker(index int) rune {
	return transcriptImageMarkerBase + rune(index)
}

func transcriptImageMarkerIndex(r rune) (int, bool) {
	index := int(r - transcriptImageMarkerBase)
	return index, index >= 0 && index < maxTranscriptImagesPerBlock
}

// stripTranscriptImageMarkers removes private marker runes from text that is
// about to share a transcript entry with real image slots, so pasted
// private-use characters cannot pose as slot anchors.
func stripTranscriptImageMarkers(s string) string {
	return strings.Map(func(r rune) rune {
		if _, ok := transcriptImageMarkerIndex(r); ok {
			return -1
		}
		return r
	}, s)
}

func transcriptImageSlot(index int, prefix string) string {
	line := prefix + string(transcriptImageMarker(index))
	return strings.Repeat(line+"\n", transcriptImageThumbnailRows-1) + line
}

func transcriptImageCaption(img transcriptImage) string {
	label := strings.TrimSpace(img.Alt)
	if label == "" {
		label = filepath.Base(img.Path)
	}
	displayPath := img.DisplayPath
	if displayPath == "" {
		displayPath = img.Path
	}
	return styled("image: "+label+" · "+truncate(displayPath, 100), "muted", "")
}

func renderTranscriptImage(index int, img transcriptImage, prefix string, leadingNewline, trailingNewline bool) string {
	var b strings.Builder
	if leadingNewline {
		b.WriteByte('\n')
	}
	b.WriteString(prefix)
	b.WriteString(transcriptImageCaption(img))
	b.WriteByte('\n')
	b.WriteString(transcriptImageSlot(index, prefix))
	if trailingNewline {
		b.WriteByte('\n')
	}
	return b.String()
}

func renderTranscriptImages(images []transcriptImage, prefix string) string {
	var blocks []string
	for i, img := range images {
		if i >= maxTranscriptImagesPerBlock {
			break
		}
		blocks = append(blocks, renderTranscriptImage(i, img, prefix, false, false))
	}
	return strings.Join(blocks, "\n")
}

// resolveLocalTranscriptImage accepts only explicit filesystem references to
// existing regular raster files. Remote URLs, data URLs, arbitrary prose, and
// missing files remain ordinary text.
func resolveLocalTranscriptImage(ref, alt, baseDir string) (transcriptImage, bool) {
	display := strings.TrimSpace(ref)
	if display == "" || strings.ContainsRune(display, '\x00') {
		return transcriptImage{}, false
	}

	path := display
	parsed, err := url.Parse(display)
	if err != nil {
		return transcriptImage{}, false
	}
	if parsed.Scheme != "" {
		windowsDrive := runtime.GOOS == "windows" && len(display) >= 2 && display[1] == ':'
		if !windowsDrive {
			if !strings.EqualFold(parsed.Scheme, "file") || (parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost")) {
				return transcriptImage{}, false
			}
			path = parsed.Path
		}
	} else if parsed.RawQuery != "" || parsed.Fragment != "" {
		return transcriptImage{}, false
	}
	if unescaped, err := url.PathUnescape(path); err == nil {
		path = unescaped
	} else {
		return transcriptImage{}, false
	}
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return transcriptImage{}, false
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~/"), `~\`))
	}
	if !filepath.IsAbs(path) {
		if baseDir == "" {
			baseDir, _ = os.Getwd()
		}
		path = filepath.Join(baseDir, path)
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil || !supportedLocalImageExtension(abs) {
		return transcriptImage{}, false
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxLocalImageBytes {
		return transcriptImage{}, false
	}
	width, height, ok := localImageDimensions(abs)
	if !ok {
		return transcriptImage{}, false
	}
	return transcriptImage{
		Path:        abs,
		DisplayPath: display,
		Alt:         strings.TrimSpace(alt),
		Width:       width,
		Height:      height,
		Version:     localImageVersion(abs),
	}, true
}

func localImageDimensions(path string) (int, int, bool) {
	file, err := openBoundedRegularFile(path, maxLocalImageBytes)
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()

	config, _, err := image.DecodeConfig(io.LimitReader(file, maxLocalImageBytes+1))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxLocalImagePixels {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

// refreshTranscriptImageSources updates dimensions when a referenced file is
// regenerated in place. This lets the transcript reflow its reserved slot
// before the terminal protocol sees the new aspect ratio.
func (m *replModel) refreshTranscriptImageSources() bool {
	changed := false
	for transcriptIndex, images := range m.transcriptImages {
		updated := images
		copied := false
		for imageIndex, img := range images {
			version := localImageVersion(img.Path)
			if version == img.Version {
				continue
			}
			if !copied {
				updated = append([]transcriptImage(nil), images...)
				copied = true
			}
			updated[imageIndex].Version = version
			if width, height, ok := localImageDimensions(img.Path); ok {
				updated[imageIndex].Width = width
				updated[imageIndex].Height = height
			}
			changed = true
		}
		if copied {
			m.transcriptImages[transcriptIndex] = updated
		}
	}
	return changed
}

func supportedLocalImageExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	default:
		return false
	}
}

// discoverToolOutputImages recognizes explicit Markdown images and bare path
// lines. It intentionally does not mine prose, JSON, inline code, or fenced
// code for path-looking substrings.
func discoverToolOutputImages(body, baseDir string) []transcriptImage {
	markdownImages := discoverMarkdownImages(body, baseDir)
	images := make([]transcriptImage, 0, len(markdownImages))
	seen := make(map[string]struct{}, len(markdownImages))
	for _, img := range markdownImages {
		if _, duplicate := seen[img.Path]; duplicate {
			continue
		}
		seen[img.Path] = struct{}{}
		images = append(images, img)
	}

	fence := byte(0)
	fenceLen := 0
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmedLeft := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmedLeft)
		if marker, count, ok := markdownFence(trimmedLeft, indent); ok {
			if fence == 0 {
				fence, fenceLen = marker, count
			} else if marker == fence && count >= fenceLen {
				fence, fenceLen = 0, 0
			}
			continue
		}
		if fence != 0 || indent >= 4 || strings.HasPrefix(line, "\t") {
			continue
		}
		candidate := strings.TrimSpace(line)
		img, ok := resolveLocalTranscriptImage(candidate, "", baseDir)
		if !ok {
			continue
		}
		if _, duplicate := seen[img.Path]; duplicate {
			continue
		}
		seen[img.Path] = struct{}{}
		images = append(images, img)
		if len(images) >= maxTranscriptImagesPerBlock {
			break
		}
	}
	return images
}

func discoverMarkdownImages(src, baseDir string) []transcriptImage {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	source := []byte(src)
	doc := mdParser.Parser().Parse(text.NewReader(source))
	var images []transcriptImage
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || len(images) >= maxTranscriptImagesPerBlock {
			return ast.WalkContinue, nil
		}
		imageNode, ok := n.(*ast.Image)
		if !ok {
			return ast.WalkContinue, nil
		}
		if img, ok := resolveLocalTranscriptImage(string(imageNode.Destination), nodeText(n, source), baseDir); ok {
			images = append(images, img)
		}
		return ast.WalkContinue, nil
	})
	return images
}

func markdownFence(line string, indent int) (byte, int, bool) {
	if indent > 3 || len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0, false
	}
	marker := line[0]
	count := 0
	for count < len(line) && line[count] == marker {
		count++
	}
	return marker, count, count >= 3
}

// locateTranscriptImages removes private markers from terminal cells. With no
// native backend the marker rows collapse completely, leaving the caption/path
// as a compact fallback.
func locateTranscriptImages(rows [][]ui.Cell, images []transcriptImage, native bool, width, cellWidth, cellHeight int) ([][]ui.Cell, []transcriptImageSpan) {
	if len(images) == 0 {
		return rows, nil
	}
	type point struct {
		row int
		x   int
	}
	type slotGeometry struct {
		cols      int
		rows      int
		fitByRows bool
	}
	points := make(map[int][]point)
	geometries := make(map[int]slotGeometry)
	markerRows := make(map[int]int)
	out := make([][]ui.Cell, 0, len(rows))
	for _, row := range rows {
		markerIndex := -1
		markerCell := -1
		x := 0
		markerX := 0
		for i, cell := range row {
			if index, ok := transcriptImageMarkerIndex(cell.Rune); ok && index < len(images) {
				markerIndex, markerCell, markerX = index, i, x
				break
			}
			width := rw.RuneWidth(cell.Rune)
			if width > 0 {
				x += width
			}
		}
		if markerIndex >= 0 && !native {
			continue
		}
		if markerIndex >= 0 {
			geometry, ok := geometries[markerIndex]
			if !ok {
				maxCols := min(transcriptImageThumbnailCols, width-markerX)
				cols, slotRows, fitByRows := imageCellGeometry(images[markerIndex], maxCols, transcriptImageThumbnailRows, cellWidth, cellHeight)
				geometry = slotGeometry{cols: cols, rows: slotRows, fitByRows: fitByRows}
				geometries[markerIndex] = geometry
			}
			seenRows := markerRows[markerIndex]
			markerRows[markerIndex] = seenRows + 1
			if geometry.cols <= 0 || geometry.rows <= 0 || seenRows >= geometry.rows {
				continue
			}
			row = append([]ui.Cell(nil), row...)
			row[markerCell].Rune = ' '
			points[markerIndex] = append(points[markerIndex], point{row: len(out), x: markerX})
		}
		out = append(out, row)
	}

	var spans []transcriptImageSpan
	for imageIndex := range images {
		geometry := geometries[imageIndex]
		imagePoints := points[imageIndex]
		for start := 0; start < len(imagePoints); {
			end := start + 1
			for end < len(imagePoints) && imagePoints[end].row == imagePoints[end-1].row+1 && imagePoints[end].x == imagePoints[start].x {
				end++
			}
			spans = append(spans, transcriptImageSpan{
				imageIndex: imageIndex,
				row:        imagePoints[start].row,
				x:          imagePoints[start].x,
				cols:       geometry.cols,
				rows:       end - start,
				fitByRows:  geometry.fitByRows,
			})
			start = end
		}
	}
	return out, spans
}

// imageCellGeometry fits an image inside a maximum cell rectangle while
// accounting for the fact that terminal cells are usually taller than they
// are wide. The returned axis tells Kitty which single dimension to constrain;
// Kitty derives the other from the source aspect ratio without distortion.
func imageCellGeometry(img transcriptImage, maxCols, maxRows, cellWidth, cellHeight int) (cols, rows int, fitByRows bool) {
	if maxCols < minimumImageThumbnailCols || maxRows <= 0 {
		return 0, 0, false
	}
	if cellWidth <= 0 {
		cellWidth = 10
	}
	if cellHeight <= 0 {
		cellHeight = 20
	}
	if img.Width <= 0 || img.Height <= 0 {
		return maxCols, maxRows, false
	}

	maxPixelWidth := maxCols * cellWidth
	maxPixelHeight := maxRows * cellHeight
	pixelWidth, pixelHeight := fitPixelDimensions(img.Width, img.Height, maxPixelWidth, maxPixelHeight)
	if pixelWidth <= 0 || pixelHeight <= 0 {
		return 0, 0, false
	}
	cols = min(maxCols, max(1, (pixelWidth+cellWidth-1)/cellWidth))
	rows = min(maxRows, max(1, (pixelHeight+cellHeight-1)/cellHeight))
	fitByRows = imageFitsByRows(img.Width, img.Height, maxPixelWidth, maxPixelHeight)
	return cols, rows, fitByRows
}

// visibleImagePlacements projects transcript-relative slots into screen cells.
// Partially clipped thumbnails are omitted; their caption remains visible and
// scrolling the complete slot into view draws the native image.
func (m *replModel) visibleImagePlacements(totalRows, viewportHeight, topRow, logoRows, width int, pinBottom, tickerVisible bool) []terminalImagePlacement {
	if !m.nativeImages || viewportHeight <= 0 || width < minimumImageThumbnailCols {
		return nil
	}
	viewStart := topRow
	topPadding := 0
	if pinBottom {
		viewStart = max(0, totalRows-viewportHeight)
		if totalRows < viewportHeight {
			topPadding = viewportHeight - totalRows
		}
	}
	viewEnd := viewStart + viewportHeight
	if tickerVisible {
		viewEnd--
	}

	var placements []terminalImagePlacement
	rowOffset := 0
	for _, block := range m.visualBlocks {
		for _, span := range block.imageSpans {
			if span.imageIndex < 0 || span.imageIndex >= len(block.images) || span.rows <= 0 {
				continue
			}
			row := rowOffset + span.row
			if row < viewStart || row+span.rows > viewEnd {
				continue
			}
			if span.cols <= 0 || span.x+span.cols > width {
				continue
			}
			img := block.images[span.imageIndex]
			placements = append(placements, terminalImagePlacement{
				Key:       fmt.Sprintf("%s:image:%d", block.key, span.imageIndex),
				Path:      img.Path,
				X:         span.x,
				Y:         logoRows + topPadding + row - viewStart,
				Cols:      span.cols,
				Rows:      span.rows,
				FitByRows: span.fitByRows,
			})
		}
		rowOffset += len(block.rows)
	}
	return placements
}

// openImageInViewer hands a local image to the OS default viewer, detached
// from the TUI. The reap goroutine keeps the exited launcher from lingering
// as a zombie.
func openImageInViewer(path string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
