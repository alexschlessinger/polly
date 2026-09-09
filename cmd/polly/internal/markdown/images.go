package markdown

import (
	"image"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/images"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

type renderState struct {
	baseDir        string
	images         []style.Image
	imagePositions []int
	codeCache      *CodeCache
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

// ResolveLocalImage accepts only explicit filesystem references to
// existing regular raster files. Remote URLs, data URLs, arbitrary prose, and
// missing files remain ordinary text.
func ResolveLocalImage(ref, alt, baseDir string) (style.Image, bool) {
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
	if err != nil || !info.Mode().IsRegular() || info.Size() > images.MaxSourceBytes {
		return style.Image{}, false
	}
	width, height, ok := LocalImageDimensions(abs)
	if !ok {
		return style.Image{}, false
	}
	return style.Image{
		Path:        abs,
		DisplayPath: display,
		Alt:         strings.TrimSpace(alt),
		Width:       width,
		Height:      height,
		Version:     images.FileVersion(abs),
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

// LocalImageDimensions reads a raster file's dimensions within the source
// bounds without decoding its pixels.
func LocalImageDimensions(path string) (int, int, bool) {
	file, err := images.OpenBoundedFile(path, images.MaxSourceBytes)
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()

	config, _, err := image.DecodeConfig(io.LimitReader(file, images.MaxSourceBytes+1))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > images.MaxSourcePixels {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func supportedLocalImageExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	default:
		return false
	}
}

// DiscoverToolOutputImages recognizes explicit Markdown images and bare path
// lines. It intentionally does not mine prose, JSON, inline code, or fenced
// code for path-looking substrings.
func DiscoverToolOutputImages(body, baseDir string) []style.Image {
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
		img, ok := ResolveLocalImage(candidate, "", baseDir)
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
		if img, ok := ResolveLocalImage(string(imageNode.Destination), nodeText(n, source), baseDir); ok {
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
