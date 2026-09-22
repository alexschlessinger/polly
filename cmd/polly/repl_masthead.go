package main

import (
	"runtime/debug"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	rw "github.com/mattn/go-runewidth"
)

// The masthead is the top of the transcript flow: the bird, the build version,
// sandbox posture, and an invitation until the first prompt. It
// scrolls with history and leaves on /clear. Both native graphics and the
// half-block fallback use a compact four-row bird beside the identity text.

// pollyLogoPixels is a compact rendering of the supplied vector mark: red
// crown, yellow/orange left-facing beak, pink face, green body, lime wing,
// swept tail, and gold feet. Two pixel rows become one terminal half-block row
// so the bird keeps the vector's proportions in a character cell grid.
var pollyLogoPixels = []string{
	"  RRL    ",
	" YPDG    ",
	" OPGGA   ",
	"  GGAAA  ",
	" GGGGAAT ",
	"  GGGTTT ",
	"   GGT   ",
	"  FF FF  ",
}

// pollyPixelColor names the palette slot a pixel paints; the names are
// registered in StyleParserColorMap so the bird is ordinary styled text.
func pollyPixelColor(pixel rune) (string, bool) {
	switch pixel {
	case 'G', 'T':
		return "polly-green", true
	case 'L':
		return "polly-light", true
	case 'A':
		return "polly-wing", true
	case 'R':
		return "polly-crown", true
	case 'Y':
		return "polly-beak", true
	case 'O':
		return "polly-mouth", true
	case 'P':
		return "polly-face", true
	case 'D':
		return "polly-eye", true
	case 'F':
		return "polly-foot", true
	default:
		return "", false
	}
}

func pollyHalfBlockMarkup(topPixel, bottomPixel rune) string {
	top, topPainted := pollyPixelColor(topPixel)
	bottom, bottomPainted := pollyPixelColor(bottomPixel)
	switch {
	case !topPainted && !bottomPainted:
		return " "
	case topPainted && !bottomPainted:
		return style.Styled("▀", top, "")
	case !topPainted && bottomPainted:
		return style.Styled("▄", bottom, "")
	case top == bottom:
		return style.Styled("█", top, "")
	default:
		return style.StyledBg("▀", top, bottom)
	}
}

// pollyBirdWidth is the bird's column count; masthead text starts two
// columns to its right, and the bird only appears when the text beside it
// still has room for readable posture text.
const (
	pollyBirdWidth   = 9
	mastheadTextCol  = pollyBirdWidth + 2
	mastheadBirdMinW = 30
)

// pollyBirdRows renders the bird as four left-aligned markup rows, each
// exactly pollyBirdWidth cells wide. The rows are constant, so they are
// rendered once; callers must not modify the returned slice.
var pollyBirdRows = sync.OnceValue(func() []string {
	rows := make([]string, 0, len(pollyLogoPixels)/2)
	for sourceRow := 0; sourceRow+1 < len(pollyLogoPixels); sourceRow += 2 {
		top := []rune(pollyLogoPixels[sourceRow])
		bottom := []rune(pollyLogoPixels[sourceRow+1])
		var b strings.Builder
		for column := 0; column < pollyBirdWidth; column++ {
			b.WriteString(pollyHalfBlockMarkup(top[column], bottom[column]))
		}
		rows = append(rows, b.String())
	}
	return rows
})

// mastheadState is set for root sessions in the managed TUI; agent tabs,
// inspector snapshots, and the line frontends have none.
type mastheadState struct {
	enabled bool
	// sandbox is the posture summary shown under the title; empty hides the row.
	sandbox string
}

const mastheadInvitation = "Type a message, or / for commands."

// pollyBuildRevision identifies this executable, not the user's current checkout.
var pollyBuildRevision = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value[:min(8, len(setting.Value))]
			}
		}
	}
	return "dev"
})

// mastheadTextRows are the build version, sandbox posture, and invitation
// while no prompt has been sent; the muted rows clip to width with an ellipsis.
func (m *replModel) mastheadTextRows(width int) []string {
	title := style.Styled(rw.Truncate("polly", width, "…"), "", "bold")
	if width > 6 {
		title += " " + style.Styled(rw.Truncate("build "+pollyBuildRevision(), width-6, "…"), "muted", "")
	}
	rows := []string{title}
	if m.masthead.sandbox != "" {
		rows = append(rows, style.Styled(rw.Truncate(m.masthead.sandbox, width, "…"), "muted", ""))
	}
	if !m.userPromptSeen {
		rows = append(rows, style.Styled(rw.Truncate(mastheadInvitation, width, "…"), "muted", ""))
	}
	return rows
}

// mastheadBlock lays the masthead out at width: with the half-block bird when
// the terminal has no native graphics and room for it, with the embedded image
// logo riding the thumbnail pipeline on graphics-capable ones, and text only
// when neither fits. The text ends in a newline so the block owns the blank
// row below it.
func (m *replModel) mastheadBlock(width int) (transcriptDisplayBlock, bool) {
	if !m.masthead.enabled || m.quiet || width < 1 {
		return transcriptDisplayBlock{}, false
	}
	if m.nativeImages && width >= mastheadBirdMinW {
		if block, ok := m.mastheadImageBlock(width); ok {
			return block, true
		}
	}
	var lines []string
	if width >= mastheadBirdMinW {
		text := m.mastheadTextRows(width - mastheadTextCol)
		bird := pollyBirdRows()
		lines = make([]string, len(bird))
		textStart := max(0, (len(bird)-len(text))/2)
		for i, row := range bird {
			// Center the posture and hint beside the compact bird.
			if i >= textStart && i-textStart < len(text) {
				row += strings.Repeat(" ", mastheadTextCol-pollyBirdWidth) + text[i-textStart]
			}
			lines[i] = row
		}
	} else {
		lines = m.mastheadTextRows(width)
	}
	return transcriptDisplayBlock{key: "masthead", text: strings.Join(lines, "\n") + "\n"}, true
}

// mastheadImageBlock reserves marker rows for the embedded logo image at the
// left of the block and indents the posture text to its right, mirroring the
// half-block layout. The same CellGeometry inputs the transcript cell pass
// uses decide the logo's column count, so the text starts clear of the
// painted image, and the text is centered across the logo's rows.
func (m *replModel) mastheadImageBlock(width int) (transcriptDisplayBlock, bool) {
	logo := termimg.LogoImage()
	_, maxRows := style.ImageBounds(logo)
	cols, rows, _ := termimg.CellGeometry(logo, width, maxRows, m.imageCellWidth, m.imageCellHeight)
	if cols <= 0 || rows <= 0 {
		return transcriptDisplayBlock{}, false
	}
	textCol := cols + 2
	if textCol >= width {
		return transcriptDisplayBlock{}, false
	}
	text := m.mastheadTextRows(width - textCol)
	textStart := max(0, (rows-len(text))/2)
	marker := string(style.ImageMarker(0))
	lines := make([]string, rows)
	for i := range lines {
		line := marker
		if i >= textStart && i-textStart < len(text) {
			line += strings.Repeat(" ", textCol-1) + text[i-textStart]
		}
		lines[i] = line
	}
	return transcriptDisplayBlock{
		key:    "masthead",
		text:   strings.Join(lines, "\n") + "\n",
		images: []style.Image{logo},
	}, true
}

// mastheadRowCount is the number of visual rows the masthead occupies above
// the first transcript entry, including its blank separator row.
func (m *replModel) mastheadRowCount(width int) int {
	block, ok := m.mastheadBlock(width)
	if !ok {
		return 0
	}
	return strings.Count(block.text, "\n") + 1
}

// setContextName and setModelName update the identity shown in the status row.
func (m *replModel) setContextName(name string) {
	if m.status.contextName == name {
		return
	}
	m.status.contextName = name
	m.visual.invalidate()
}

func (m *replModel) setModelName(model string) {
	if m.status.modelName == model {
		return
	}
	m.status.modelName = model
	m.visual.invalidate()
}

// refreshSandboxPosture recomputes the masthead's sandbox line after the
// workspace profile changed.
func (r *managedREPL) refreshSandboxPosture() {
	if r.model.masthead.enabled {
		r.model.masthead.sandbox = currentSandboxPosture(r.config, r.state).summaryLine(false)
		r.model.visual.invalidate()
	}
}
