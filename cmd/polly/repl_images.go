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
	"unicode"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/images"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const (
	maxLocalImageBytes  = images.MaxSourceBytes
	maxLocalImagePixels = images.MaxSourcePixels
)

type markdownRenderState struct {
	baseDir        string
	images         []style.Image
	imagePositions []int
	codeCache      *markdownCodeCache
	codeIndex      int
	// clip is used only by the scrollback renderer. Source offsets, rather
	// than rendered row indexes, keep committed text stable across restyling.
	clip *markdownSourceRange
	// streaming marks the source as a truncated in-flight prefix: a table at
	// the stream edge renders unaligned, since its column widths are not final.
	streaming bool
	// deferredTable reports that a table rendered in the unaligned streaming
	// form; the stream owner must re-render once the message settles even if
	// no text was held back.
	deferredTable bool
}

// transcriptDisplayBlock is one renderable unit of the transcript. An
// activity block carries the reasoning and tool disclosure records it shows;
// adjacent activity entries merge into one block, so both lists may hold
// several IDs.
type transcriptDisplayBlock struct {
	key                     string
	text                    string
	cells                   []ui.Cell // optional formatted streaming prefix
	images                  []style.Image
	reasoningIDs            []int64
	toolDisclosureIDs       []int64
	turnTrailerID           int64
	activityFields          []turnDockPlacement
	activityReasoningDetail string
	activityToolDetail      string
	activityImageDetail     string
	agentLinks              []agentLink
}

// isActivity reports whether the block projects reasoning or tool records.
func (b transcriptDisplayBlock) isActivity() bool {
	return len(b.reasoningIDs) > 0 || len(b.toolDisclosureIDs) > 0
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

// resolveLocalTranscriptImage accepts only explicit filesystem references to
// existing regular raster files. Remote URLs, data URLs, arbitrary prose, and
// missing files remain ordinary text.
func resolveLocalTranscriptImage(ref, alt, baseDir string) (style.Image, bool) {
	display := strings.TrimSpace(ref)
	if display == "" || strings.ContainsRune(display, '\x00') {
		return style.Image{}, false
	}

	path := display
	parsed, err := url.Parse(display)
	if err != nil {
		return style.Image{}, false
	}
	if parsed.Scheme != "" {
		windowsDrive := runtime.GOOS == "windows" && len(display) >= 2 && display[1] == ':'
		if !windowsDrive {
			if !strings.EqualFold(parsed.Scheme, "file") || (parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost")) {
				return style.Image{}, false
			}
			path = parsed.Path
		}
	} else if parsed.RawQuery != "" || parsed.Fragment != "" {
		return style.Image{}, false
	}
	if unescaped, err := url.PathUnescape(path); err == nil {
		path = unescaped
	} else {
		return style.Image{}, false
	}
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return style.Image{}, false
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
		return style.Image{}, false
	}
	info, err := os.Stat(abs)
	if err != nil {
		// Retry with Unicode-space folding: macOS screenshot names contain
		// U+202F before AM/PM, which upstream tokenizers normalize to U+0020,
		// so model-emitted paths never byte-match the real file.
		abs = resolveSpaceFoldedPath(abs)
		info, err = os.Stat(abs)
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxLocalImageBytes {
		return style.Image{}, false
	}
	width, height, ok := localImageDimensions(abs)
	if !ok {
		return style.Image{}, false
	}
	return style.Image{
		Path:        abs,
		DisplayPath: display,
		Alt:         strings.TrimSpace(alt),
		Width:       width,
		Height:      height,
		Version:     localImageVersion(abs),
	}, true
}

// resolveSpaceFoldedPath handles paths whose Unicode space separators were
// normalized to U+0020 before reaching us (e.g. macOS screenshot names use
// U+202F before AM/PM). It returns path unchanged when it already exists;
// otherwise it scans each directory component for an entry whose name matches
// after folding all Unicode spaces to U+0020. Only the first fold-match per
// component is used; exact matches always win.
func resolveSpaceFoldedPath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	vol := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, vol)
	components := strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' })
	cur := vol
	// filepath.IsAbs can't root this walk: on Windows the volume-trimmed
	// rest ("\Users\...") is rooted but not absolute, and losing the
	// separator here silently yields drive-relative paths ("C:Users\...").
	if len(rest) > 0 && (rest[0] == '/' || rest[0] == '\\') {
		cur += string(filepath.Separator)
	}
	for _, comp := range components {
		next := filepath.Join(cur, comp)
		if _, err := os.Stat(next); err == nil {
			cur = next
			continue
		}
		entries, err := os.ReadDir(cur)
		if err != nil {
			return path
		}
		matched := ""
		for _, e := range entries {
			if spaceFold(e.Name()) == spaceFold(comp) {
				matched = e.Name()
				break
			}
		}
		if matched == "" {
			return path
		}
		cur = filepath.Join(cur, matched)
	}
	return cur
}

// spaceFold maps every Unicode space separator to U+0020 so path comparison
// is insensitive to which space character a filename actually contains.
func spaceFold(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
}

func localImageDimensions(path string) (int, int, bool) {
	file, err := images.OpenBoundedFile(path, maxLocalImageBytes)
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
func (m *replModel) refreshTranscriptImageSources(width int) bool {
	changed := false
	refresh := func(images []style.Image) ([]style.Image, bool) {
		updated := images
		copied := false
		for imageIndex, img := range images {
			version := localImageVersion(img.Path)
			if version == img.Version {
				continue
			}
			if !copied {
				updated = append([]style.Image(nil), images...)
				copied = true
			}
			updated[imageIndex].Version = version
			if width, height, ok := localImageDimensions(img.Path); ok {
				updated[imageIndex].Width = width
				updated[imageIndex].Height = height
			}
		}
		return updated, copied
	}
	// This runs inside transcriptRows, so it cannot measure display blocks
	// (that would re-enter the layout); it re-anchors in per-entry space,
	// which is exact only while no earlier activity entries have merged.
	for transcriptIndex := range m.transcript {
		updated, copied := refresh(m.transcript[transcriptIndex].images)
		if copied {
			oldCount, start := 0, 0
			if !m.followBottom {
				oldCount = m.entryVisualLineCount(transcriptIndex, width)
				start = m.entryVisualStart(transcriptIndex, width)
			}
			// The one raw lane write: this runs inside transcriptRows, which
			// invalidates on the returned flag, so the owner's invalidation
			// would be redundant here.
			m.transcript[transcriptIndex].images = updated
			if !m.followBottom {
				m.anchorForResizedEntry(start, oldCount, m.entryVisualLineCount(transcriptIndex, width))
			}
			changed = true
		}
	}
	// Tool and Images disclosures project canonical sidecars into transient
	// activity blocks. Keep those copies fresh so reopening either disclosure
	// cannot resurrect stale dimensions or file versions.
	for _, record := range m.toolDisclosures.all() {
		for rowIndex := range record.rows {
			updated, copied := refresh(record.rows[rowIndex].images)
			if copied {
				record.rows[rowIndex].images = updated
				changed = true
			}
			updated, copied = refresh(record.rows[rowIndex].inspectionImages)
			if copied {
				record.rows[rowIndex].inspectionImages = updated
				changed = true
			}
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
func discoverToolOutputImages(body, baseDir string) []style.Image {
	markdownImages := discoverMarkdownImages(body, baseDir)
	images := make([]style.Image, 0, len(markdownImages))
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
		if len(images) >= style.MaxImagesPerBlock {
			break
		}
	}
	return images
}

func discoverMarkdownImages(src, baseDir string) []style.Image {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	source := []byte(src)
	doc := mdParser.Parser().Parse(text.NewReader(source))
	var images []style.Image
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || len(images) >= style.MaxImagesPerBlock {
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
func locateTranscriptImages(rows [][]ui.Cell, images []style.Image, native bool, width, cellWidth, cellHeight int) ([][]ui.Cell, []transcriptImageSpan) {
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
			if index, ok := style.ImageMarkerIndex(cell.Rune); ok && index < len(images) {
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
				imageMaxCols, imageMaxRows := style.ImageBounds(images[markerIndex])
				maxCols := min(imageMaxCols, width-markerX)
				cols, slotRows, fitByRows := imageCellGeometry(images[markerIndex], maxCols, imageMaxRows, cellWidth, cellHeight)
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
func imageCellGeometry(img style.Image, maxCols, maxRows, cellWidth, cellHeight int) (cols, rows int, fitByRows bool) {
	if maxCols < style.MinimumThumbnailCols || maxRows <= 0 {
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
	pixelWidth, pixelHeight := images.FitDimensions(img.Width, img.Height, maxPixelWidth, maxPixelHeight)
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
func (m *replModel) visibleImagePlacements(v transcriptViewport) []terminalImagePlacement {
	if !m.nativeImages || v.width < style.MinimumThumbnailCols {
		return nil
	}
	var placements []terminalImagePlacement
	rowOffset := 0
	for _, block := range m.visual.blocks {
		for _, span := range block.imageSpans {
			if span.imageIndex < 0 || span.imageIndex >= len(block.images) || span.rows <= 0 {
				continue
			}
			row := rowOffset + span.row
			if row < v.start || row+span.rows > v.end {
				continue
			}
			if span.cols <= 0 || span.x+span.cols > v.width {
				continue
			}
			img := block.images[span.imageIndex]
			placements = append(placements, terminalImagePlacement{
				Key:       fmt.Sprintf("%s:image:%d", block.key, span.imageIndex),
				Path:      img.Path,
				X:         span.x,
				Y:         v.screenY(row),
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
