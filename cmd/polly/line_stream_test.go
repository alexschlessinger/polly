package main

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

// A small screen interpreter checks visible text rather than counting bytes
// in a repaint trace (where mutable text legitimately appears repeatedly).
func lineTestScreen(raw string) string {
	lines := []map[int]rune{{}}
	x, y := 0, 0
	for len(raw) > 0 {
		if strings.HasPrefix(raw, "\x1b[") {
			end := 2
			for end < len(raw) && !(raw[end] >= 0x40 && raw[end] <= 0x7e) {
				end++
			}
			if end == len(raw) {
				break
			}
			arg, _ := strconv.Atoi(raw[2:end])
			switch raw[end] {
			case 'A':
				y = max(0, y-max(1, arg))
			case 'K':
				lines[y] = map[int]rune{}
			}
			raw = raw[end+1:]
			continue
		}
		r, size := utf8.DecodeRuneInString(raw)
		raw = raw[size:]
		switch r {
		case '\r':
			x = 0
		case '\n':
			y++
			x = 0
			for len(lines) <= y {
				lines = append(lines, map[int]rune{})
			}
		default:
			if !unicode.IsControl(r) {
				lines[y][x] = r
				x += max(0, rw.RuneWidth(r))
			}
		}
	}
	var out []string
	for _, line := range lines {
		maxX := -1
		for x := range line {
			maxX = max(maxX, x)
		}
		var b strings.Builder
		for x := 0; x <= maxX; x++ {
			if r, ok := line[x]; ok {
				b.WriteRune(r)
				x += max(1, rw.RuneWidth(r)) - 1
			} else {
				b.WriteByte(' ')
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return strings.Join(out, "\n")
}

func lineStreamTestUI(t *testing.T, quiet, noColor bool, columns, height *int) (*lineTurnUI, *bytes.Buffer) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	if noColor {
		t.Setenv("NO_COLOR", "1")
	} else {
		t.Setenv("NO_COLOR", "")
	}
	out := new(bytes.Buffer)
	ui := newLineTurnUIWithCapabilities(&Config{Quiet: quiet}, nil, outputCapabilities{surface: outputSurfaceLineANSI, columns: *columns, noColor: noColor})
	ui.writer, ui.errWriter = out, out
	ui.stdoutTTY, ui.stderrTTY, ui.sameTerminal = true, true, true
	ui.size = func(bool) (int, int) { return *columns, *height }
	ui.Start()
	ui.toolMu.Lock()
	ui.stopRendererLocked()
	ui.toolMu.Unlock()
	<-ui.renderDone
	t.Cleanup(ui.Stop)
	return ui, out
}

func paintLineStream(ui *lineTurnUI) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.stream.draw(ui, true)
}

func TestLineStreamingShowsMutableAnswerAndFooter(t *testing.T) {
	for _, noColor := range []bool{false, true} {
		t.Run(fmt.Sprint(noColor), func(t *testing.T) {
			columns, height := 45, 10
			ui, out := lineStreamTestUI(t, false, noColor, &columns, &height)
			ui.AppendAssistantText("first **unclosed")
			paintLineStream(ui)
			got := lineTestScreen(out.String())
			if !strings.Contains(got, "first") || !strings.Contains(got, "streaming") || strings.Contains(got, "unclosed") {
				t.Fatalf("no immediate answer/footer or holdback failed: %q", got)
			}
			ui.AppendAssistantText("** and last.")
			paintLineStream(ui)
			ui.FinishTextTurn()
			ui.CompleteTurn(turnCompletion{Elapsed: time.Second})
			got = lineTestScreen(out.String())
			if strings.Count(got, "first unclosed and last.") != 1 || strings.Contains(got, "streaming") || !strings.Contains(got, "✓ 1.0s") {
				t.Fatalf("bad settlement: %q", got)
			}
			if noColor && regexp.MustCompile(`\x1b\[[0-9;]*m`).Match(out.Bytes()) {
				t.Fatal("NO_COLOR emitted SGR")
			}
		})
	}
}

func TestLineStreamingOverflowDoesNotLoseOrDuplicateSource(t *testing.T) {
	for _, code := range []bool{false, true} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			columns, height := 24, 6
			ui, out := lineStreamTestUI(t, true, true, &columns, &height)
			var answer strings.Builder
			if code {
				ui.AppendAssistantText("```go\n")
			}
			for i := range 100 {
				chunk := fmt.Sprintf("word%03d ", i)
				answer.WriteString(chunk)
				ui.AppendAssistantText(chunk)
				paintLineStream(ui)
				if len(ui.stream.frame.widths) > height {
					t.Fatal("mutable frame escaped viewport")
				}
			}
			if ui.stream.committed == 0 {
				t.Fatal("no overflow was committed")
			}
			if code {
				ui.AppendAssistantText("\n```")
			}
			ui.FinishTextTurn()
			ui.CompleteTurn(turnCompletion{})
			got := lineTestScreen(out.String())
			if code {
				if strings.Count(got, "╭─ go") != 1 {
					t.Fatalf("code header replayed: %q", got)
				}
				got = strings.ReplaceAll(strings.ReplaceAll(got, "╭─ go", ""), "│", "")
			}
			compact := func(s string) string {
				return strings.Map(func(r rune) rune {
					if unicode.IsSpace(r) {
						return -1
					}
					return r
				}, s)
			}
			if compact(got) != compact(answer.String()) {
				t.Fatalf("source lost/duplicated:\n%s\nwant:\n%s", got, answer.String())
			}
		})
	}
}

func TestLineStreamingCompletedBlocksAndLateReferences(t *testing.T) {
	columns, height := 80, 16
	ui, out := lineStreamTestUI(t, true, true, &columns, &height)
	ui.AppendAssistantText("stable [old][ref]\n\nmutable [new][ref]")
	paintLineStream(ui)
	if ui.stream.committed == 0 {
		t.Fatal("complete block was not committed")
	}
	ui.AppendAssistantText("\n\n[ref]: https://example.com")
	paintLineStream(ui)
	ui.FinishTextTurn()
	ui.CompleteTurn(turnCompletion{})
	got := lineTestScreen(out.String())
	if strings.Count(got, "stable [old][ref]") != 1 || strings.Count(got, "https://example.com") != 1 || !strings.Contains(got, "mutable new") {
		t.Fatalf("late definition changed committed text or missed mutable text: %q", got)
	}
}

func TestLineSourceClippingPreservesTheSharedMarkdownWalker(t *testing.T) {
	for _, source := range []string{
		"<https://one.test><https://two.test>",
		"prefix <https://one.test> <https://two.test> suffix",
		"\\*escaped\\* ! &amp; &#x20ac; &copy;",
		"---\n\ntext\n\n---",
		"- first\n- second\n\n1. third\n2. fourth",
		"# title\n\n> quote\n> next",
		"Title\n---\n\n~~~go\n\tvar x = 1\n~~~",
		"| one | two |\n| --- | --- |\n| three | four |",
	} {
		doc := markdown.NewDocument(source, "", false, nil)
		rows, _ := doc.Render(0, len(source), 1000)
		var parts []string
		for _, row := range rows {
			parts = append(parts, lineCellsOutput(row, false))
		}
		want, _, _ := markdown.RenderWithLocalImages(source, "", false)
		if got := strings.Join(parts, "\n"); got != plainStyledText(want) {
			t.Fatalf("clipping changed %q:\n%s\nwant:\n%s", source, got, plainStyledText(want))
		}
	}
}

// Shrinking the terminal scrolls the top of the owned frame into scrollback.
// A notice arriving before the next repaint must not replay those rows.
func TestLineStreamingHeightShrinkThenNoticeDoesNotReplayScrollback(t *testing.T) {
	columns, height := 24, 12
	ui, out := lineStreamTestUI(t, false, true, &columns, &height)
	// One open paragraph wrapping to eight rows: a mutable tail the frame
	// owns in full, so nothing has been committed when the screen shrinks.
	var words []string
	for i := range 24 {
		words = append(words, fmt.Sprintf("word%03d", i))
	}
	ui.AppendAssistantText(strings.Join(words, " "))
	paintLineStream(ui)
	if rows := len(ui.stream.frame.widths); rows < 6 || rows > height || ui.stream.committed != 0 {
		t.Fatalf("frame rows=%d committed=%d", rows, ui.stream.committed)
	}
	height = 3
	ui.AppendWarning("check this")
	ui.FinishTextTurn()
	ui.CompleteTurn(turnCompletion{Elapsed: time.Second})
	got := lineTestScreen(out.String())
	for _, text := range append(words, "Warning: check this") {
		if strings.Count(got, text) != 1 {
			t.Fatalf("shrink/notice lost or duplicated %q: %q", text, got)
		}
	}
}

// A cut inside a streaming code block lands on a line start, and the clipped
// continuation is a slice of one full-block highlight rather than a fresh
// chroma pass per probe: the same lines come back and the cache holds the
// whole block afterwards.
func TestFitPrefixCutsCodeAtLineStartsAndSharesHighlight(t *testing.T) {
	var b strings.Builder
	b.WriteString("```go\n")
	for i := range 40 {
		fmt.Fprintf(&b, "func f%02d() int { return %d } // trailing\n", i, i)
	}
	src := b.String()
	var cache markdown.CodeCache
	doc := markdown.NewDocument(src, "", true, &cache)
	rows, _ := doc.Render(0, len(src), 80)
	cut := doc.FitPrefix(0, len(src), 80, len(rows)-10)
	if cut == 0 || cut == len(src) || src[cut-1] != '\n' {
		t.Fatalf("cut %d is not a line start", cut)
	}
	var head, tail []string
	for _, row := range must2(doc.Render(0, cut, 80)) {
		head = append(head, lineCellsOutput(row, false))
	}
	for _, row := range must2(doc.Render(cut, len(src), 80)) {
		tail = append(tail, lineCellsOutput(row, false))
	}
	var full []string
	for _, row := range must2(doc.Render(0, len(src), 80)) {
		full = append(full, lineCellsOutput(row, false))
	}
	if got := append(head, tail...); !slices.Equal(got, full) {
		t.Fatalf("clipped code = %q, want %q", got, full)
	}
	if code, _ := cache.Block(0); cache.Len() != 1 || code != strings.TrimPrefix(src, "```go\n") {
		t.Fatalf("cache does not hold the whole block: %q", code)
	}
}

func TestLineStreamingResizeCJKAndNotice(t *testing.T) {
	columns, height := 70, 18
	ui, out := lineStreamTestUI(t, false, true, &columns, &height)
	ui.AppendAssistantText("你好世界 and a visible answer.")
	paintLineStream(ui)
	columns = 28
	paintLineStream(ui)
	if len(ui.stream.frame.widths) > height {
		t.Fatal("resized frame exceeds screen")
	}
	ui.AppendWarning("check this")
	ui.AppendAssistantText("The continuation.")
	paintLineStream(ui)
	ui.FinishTextTurn()
	ui.CompleteTurn(turnCompletion{Elapsed: time.Second})
	got := lineTestScreen(out.String())
	for _, text := range []string{"你好世界", "Warning: check this", "The continuation.", "✓ 1.0s"} {
		if strings.Count(got, text) != 1 {
			t.Fatalf("resize/notice lost or duplicated %q: %q", text, got)
		}
	}
}

func TestLineStreamingImagesCommitOnce(t *testing.T) {
	columns, height := 80, 20
	ui, out := lineStreamTestUI(t, true, false, &columns, &height)
	ui.imageBaseDir = t.TempDir()
	writeImageFixture(t, ui.imageBaseDir+"/chart.png", 8, 4)
	ui.capabilities.imageProtocol = termimg.ProtocolKitty
	ui.AppendAssistantText("![chart](chart.png)")
	paintLineStream(ui)
	paintLineStream(ui)
	if strings.Contains(out.String(), "\x1b_Ga=T") {
		t.Fatal("mutable preview transmitted native image")
	}
	ui.AppendAssistantText("\n\nAfter image.")
	paintLineStream(ui)
	columns = 65
	paintLineStream(ui)
	ui.FinishTextTurn()
	ui.CompleteTurn(turnCompletion{})
	if count := strings.Count(out.String(), "\x1b_Ga=T"); count != 1 {
		t.Fatalf("native image transmitted %d times", count)
	}
}

func TestLineStreamingApprovalDefersTextAndNotices(t *testing.T) {
	columns, height := 80, 20
	ui, out := lineStreamTestUI(t, false, true, &columns, &height)
	ui.AppendAssistantText("before approval")
	paintLineStream(ui)
	ui.toolMu.Lock()
	ui.clearActivityLocked()
	ui.flushBufferedMarkdown()
	ui.prompting = true
	fmt.Fprint(ui.errWriter, "allow? ")
	ui.toolMu.Unlock()
	before := out.String()
	ui.AppendAssistantText("text arriving during approval")
	ui.AppendWarning("queued notice")
	paintLineStream(ui)
	if out.String() != before {
		t.Fatal("callback/repaint overwrote approval prompt")
	}
	ui.toolMu.Lock()
	fmt.Fprintln(ui.errWriter, "yes")
	ui.prompting = false
	ui.flushBufferedMarkdown()
	ui.errWriter.Write(ui.pending.Bytes())
	ui.pending.Reset()
	ui.toolMu.Unlock()
	ui.FinishTextTurn()
	ui.CompleteTurn(turnCompletion{})
	got := lineTestScreen(out.String())
	for _, text := range []string{"before approval", "allow? yes", "text arriving during approval", "Warning: queued notice"} {
		if strings.Count(got, text) != 1 {
			t.Fatalf("approval lost/duplicated %q: %q", text, got)
		}
	}
}

func TestLineCompletionRejectsLateCallbacks(t *testing.T) {
	ui, out, status := activityTestUI(t, true, &Config{ActivityDetails: true})
	ui.AppendAssistantText("answer")
	ui.FinishTextTurn()
	ui.CompleteTurn(turnCompletion{})
	beforeAnswer, beforeStatus := out.String(), status.String()
	ui.AppendAssistantText("late answer")
	ui.ShowThinking("late reasoning")
	ui.AppendToolStart([]messages.ChatMessageToolCall{{ID: "late", Name: "read_file"}})
	ui.AppendToolMedia(messages.ChatMessageToolCall{}, []style.Image{{Alt: "late image"}})
	ui.RecordTurnTokens(100, 20)
	ui.Stop()
	if out.String() != beforeAnswer || status.String() != beforeStatus {
		t.Fatal("late callbacks wrote after the completed trailer")
	}
}

func TestPrintedImageDetailsDoNotResendPreviews(t *testing.T) {
	ui, out, status := activityTestUI(t, true, &Config{ActivityDetails: true})
	path := t.TempDir() + "/image.png"
	writeImageFixture(t, path, 8, 4)
	ui.activity.imageCaps = outputCapabilities{surface: outputSurfaceLineANSI, imageProtocol: termimg.ProtocolKitty, columns: 80}
	ui.AppendToolMedia(messages.ChatMessageToolCall{}, []style.Image{{Path: path, Alt: "inspected", Width: 8, Height: 4, Inspection: true}})
	if strings.Count(status.String(), "\x1b_Ga=T") != 1 {
		t.Fatal("inspection did not display its preview")
	}
	ui.AppendAssistantText("answer")
	ui.FinishTextTurn()
	ui.CompleteTurn(turnCompletion{})
	ui.Stop()
	if strings.Count(status.String(), "\x1b_Ga=T") != 1 || strings.Count(status.String(), "  Images\n") != 1 {
		t.Fatal("image details duplicated preview or group")
	}
	if out.String() != "answer\n" {
		t.Fatal("image details entered stdout")
	}
}

func TestLineFooterFittingProtectsOutcomeAndElapsed(t *testing.T) {
	for _, width := range []int{12, 20, 24, 40, 80, 160} {
		activity := []turnDockField{accentField("thought 1.2s"), accentField("20 tools"), accentField("4 agents, 1 failed, 1 canceled"), accentField("3 images viewed")}
		status := []turnDockField{turnOutcomeField(turnOutcomeIncomplete, "30.0s"), mutedField("100k in / 20k out", true), mutedField("ctx 100k/128k", true), mutedField("long/model/name", true), mutedField("saved-session-name", true)}
		rows := renderLineActivityRows(activity, status, width)
		if len(rows) > 2 {
			t.Fatal("footer exceeds two rows")
		}
		joined := plainStyledText(strings.Join(rows, "\n"))
		if !strings.Contains(joined, "incomplete") || !strings.Contains(joined, "30.0s") {
			t.Fatalf("outcome hidden at %d: %q", width, joined)
		}
		for _, row := range rows {
			if style.TextWidth(row) > width {
				t.Fatalf("footer overflows %d: %q", width, row)
			}
		}
	}
}

func TestNarrowLiveAgentsShowAggregateStatus(t *testing.T) {
	ui, _, _ := activityTestUI(t, false, &Config{})
	ui.activity.caps = lineStatusCapabilities{live: true, columns: 24}
	call := messages.ChatMessageToolCall{ID: "a", Name: "spawn_agent", Arguments: `{"label":"a very long description for the child"}`}
	ui.AppendToolStart([]messages.ChatMessageToolCall{call})
	child := ui.childActivity(call)
	child.phase(turnStateThinking)
	ui.toolMu.Lock()
	rows := ui.activityRowsLocked(false)
	ui.toolMu.Unlock()
	if got := strings.Join(rows, "\n"); !strings.Contains(got, "waiting for agents") || strings.Contains(got, "thinking") || strings.Contains(got, "description") {
		t.Fatalf("unexpected agent status: %q", got)
	}
}

func TestSeparateTerminalsDoNotShareCursorOrWhitespace(t *testing.T) {
	ui, out, status := activityTestUI(t, true, &Config{})
	ui.stdoutTTY, ui.sameTerminal = true, false
	ui.AppendAssistantText("partial")
	ui.AppendWarning("independent")
	ui.AppendToolMedia(messages.ChatMessageToolCall{}, []style.Image{{Alt: "receipt"}})
	ui.CompleteTurn(turnCompletion{})
	if out.String() != "partial" || !strings.Contains(status.String(), "independent") {
		t.Fatalf("independent streams interfered: stdout=%q stderr=%q", out.String(), status.String())
	}
}

func must2[T any, U any](t T, _ U) T { return t }
