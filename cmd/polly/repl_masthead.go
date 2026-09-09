package main

import (
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
)

// The masthead is the top of the transcript flow: the bird, the session and
// model, the sandbox posture, and an invitation until the first prompt. It
// scrolls with history and survives /clear because it is not a transcript
// entry. Terminals with native graphics show the macaw band at startup
// instead of the half-block bird, so their masthead is text only.

// pollyLogoPixels is a compact rendering of the supplied vector mark: red
// crown, yellow/orange left-facing beak, pink face, green body, lime wing,
// swept tail, and gold feet. Two pixel rows become one terminal half-block row
// so the bird keeps the vector's proportions in a character cell grid.
var pollyLogoPixels = []string{
	"  RRR        ",
	" YRRLL       ",
	"YPPDLG       ",
	" OPGGG       ",
	"  GGGAA      ",
	" GGGGAAA     ",
	" GGGGAAAA    ",
	"  GGGAAATT   ",
	"   GGGGATTT  ",
	"    GGGTT    ",
	"   F G F     ",
	"  FFF FFF    ",
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
// still has room for a full sandbox row.
const (
	pollyBirdWidth   = 13
	mastheadTextCol  = pollyBirdWidth + 2
	mastheadBirdMinW = 60
)

// pollyBirdRows renders the bird as six left-aligned markup rows, each
// exactly pollyBirdWidth cells wide.
func pollyBirdRows() []string {
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
}

// mastheadState is set for root sessions in the managed TUI; agent tabs,
// inspector snapshots, and the line frontends have none.
type mastheadState struct {
	enabled bool
	// sandbox is the posture summary shown under the title; empty hides the row.
	sandbox string
}

const mastheadInvitation = "Type a message, or / for commands."

// mastheadTextRows are the title, the sandbox posture, and the invitation
// while no prompt has been sent; the muted rows clip to width with an ellipsis.
func (m *replModel) mastheadTextRows(width int) []string {
	rows := []string{m.mastheadTitle(width)}
	if m.masthead.sandbox != "" {
		rows = append(rows, style.Styled(rw.Truncate(m.masthead.sandbox, width, "…"), "muted", ""))
	}
	if !m.userPromptSeen {
		rows = append(rows, style.Styled(rw.Truncate(mastheadInvitation, width, "…"), "muted", ""))
	}
	return rows
}

// mastheadTitle is `polly · session · model`, dropping the model and then
// the session when the row would not fit.
func (m *replModel) mastheadTitle(width int) string {
	session := m.status.displayLabel()
	if session == "-" {
		session = ""
	}
	model := shortModelName(m.status.modelName)
	fits := func(parts ...string) bool {
		n := 0
		for i, p := range parts {
			if i > 0 {
				n += 3
			}
			n += rw.StringWidth(p)
		}
		return n <= width
	}
	sep := style.Styled(" · ", "muted", "")
	title := style.Styled("polly", "", "bold")
	switch {
	case session != "" && model != "" && fits("polly", session, model):
		return title + sep + style.Styled(session, "accent", "") + sep + style.Styled(model, "muted", "")
	case session != "" && fits("polly", session):
		return title + sep + style.Styled(session, "accent", "")
	case model != "" && session == "" && fits("polly", model):
		return title + sep + style.Styled(model, "muted", "")
	default:
		return title
	}
}

// mastheadBlock lays the masthead out at width: with the half-block bird when
// the terminal has no native graphics and room for it, otherwise text only.
// The text ends in a newline so the block owns the blank row below it.
func (m *replModel) mastheadBlock(width int) (transcriptDisplayBlock, bool) {
	if !m.masthead.enabled || m.quiet || width < 1 {
		return transcriptDisplayBlock{}, false
	}
	var lines []string
	if !m.nativeImages && width >= mastheadBirdMinW {
		text := m.mastheadTextRows(width - mastheadTextCol)
		bird := pollyBirdRows()
		lines = make([]string, len(bird))
		for i, row := range bird {
			// Text sits beside the body, from the second bird row down.
			if i >= 1 && i-1 < len(text) {
				row += strings.Repeat(" ", mastheadTextCol-pollyBirdWidth) + text[i-1]
			}
			lines[i] = row
		}
	} else {
		lines = m.mastheadTextRows(width)
	}
	return transcriptDisplayBlock{key: "masthead", text: strings.Join(lines, "\n") + "\n"}, true
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

// setContextName and setModelName change the identity the masthead and the
// status row show; both invalidate the visual cache the masthead lives in.
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

// startupLogoRowCount reserves the native-image band above the transcript
// until the first turn starts, and only when the terminal can draw it with at
// least one transcript row to spare. Everywhere else the bird lives in the
// masthead.
func startupLogoRowCount(contentHeight int, visible, nativeImages bool) int {
	if !visible || !nativeImages || contentHeight <= imageLogoHeight {
		return 0
	}
	return imageLogoHeight
}
