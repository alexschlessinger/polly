package main

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

var ansiSGRPattern = regexp.MustCompile("\\x1b\\[[0-9;]*m")

func TestRenderLineMarkdownANSI(t *testing.T) {
	source := "## Setup\n\n- **bold** and *italic* with `code`\n\n| Name | Qty |\n|---|---:|\n| kiwi | 12 |\n\n日本語"
	got := string(renderLineMarkdown(source, t.TempDir(), outputCapabilities{
		surface: outputSurfaceLineANSI,
		columns: 80,
	}))
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("ANSI output has no style sequences: %q", got)
	}
	plain := ansiSGRPattern.ReplaceAllString(got, "")
	for _, want := range []string{"▎ Setup", "• bold and italic with code", "Name", "kiwi", "12", "日本語"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("rendered output missing %q: %q", want, plain)
		}
	}
	for _, sourceMarker := range []string{"**", "*italic*", "`code`", "|---"} {
		if strings.Contains(plain, sourceMarker) {
			t.Fatalf("rendered output leaked Markdown marker %q: %q", sourceMarker, plain)
		}
	}
}

func TestRenderLineMarkdownToleratesIncompleteMarkdown(t *testing.T) {
	got := string(renderLineMarkdown("before\n\n```go\nfunc main() {", "", outputCapabilities{
		surface: outputSurfaceLineANSI,
		columns: 80,
	}))
	plain := ansiSGRPattern.ReplaceAllString(got, "")
	if !strings.Contains(plain, "before") || !strings.Contains(plain, "func main() {") {
		t.Fatalf("incomplete Markdown lost content: %q", plain)
	}
}

func TestRenderLineMarkdownStripsModelControlSequences(t *testing.T) {
	got := string(renderLineMarkdown("before\x1b]52;c;secret\aafter", "", outputCapabilities{
		surface: outputSurfaceLineANSI,
		columns: 80,
	}))
	plain := ansiSGRPattern.ReplaceAllString(got, "")
	if strings.Contains(plain, "\x1b") || strings.Contains(plain, "\a") || !strings.Contains(plain, "before]52;c;secretafter") {
		t.Fatalf("control sequence sanitization = %q", plain)
	}
}

func TestLineTurnUIBuffersOnlyRichOutput(t *testing.T) {
	var richOut bytes.Buffer
	rich := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface: outputSurfaceLineANSI,
		columns: 80,
	})
	rich.writer = &richOut
	rich.AppendAssistantText("## Hel")
	rich.AppendAssistantText("lo")
	if richOut.Len() != 0 {
		t.Fatalf("rich output streamed before settlement: %q", richOut.String())
	}
	rich.FinishTextTurn()
	if got := ansiSGRPattern.ReplaceAllString(richOut.String(), ""); got != "▎ Hello\n" {
		t.Fatalf("rich output = %q, want rendered heading", got)
	}

	var rawOut bytes.Buffer
	raw := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface: outputSurfaceLineRaw,
		columns: 80,
	})
	raw.writer = &rawOut
	raw.AppendAssistantText("# MixedCase **now**")
	if got := rawOut.String(); got != "# MixedCase **now**" {
		t.Fatalf("raw output did not stream source exactly: %q", got)
	}
	raw.FinishTextTurn()
	if got := rawOut.String(); got != "# MixedCase **now**\n" || strings.Contains(got, "\x1b") {
		t.Fatalf("raw final output = %q", got)
	}
}

func TestLineTurnUIFlushesRichOutputAtBoundaries(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	var out, errOut bytes.Buffer
	ui := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface: outputSurfaceLineANSI,
		columns: 80,
	})
	ui.writer = &out
	ui.errWriter = &errOut
	ui.AppendAssistantText("**before**")
	ui.AppendToolStart([]messages.ChatMessageToolCall{{Name: "read"}})
	if got := ansiSGRPattern.ReplaceAllString(out.String(), ""); got != "before\n" {
		t.Fatalf("tool boundary did not flush assistant segment: %q", got)
	}
	ui.AppendToolEnd(messages.ChatMessageToolCall{Name: "read"}, "ok", time.Millisecond, nil)
	ui.AppendAssistantText("*after*")
	ui.AppendWarning("check")
	ui.FinishTextTurn()
	if got := ansiSGRPattern.ReplaceAllString(out.String(), ""); got != "before\n\nafter\n" {
		t.Fatalf("boundary output = %q", got)
	}
	if strings.Contains(errOut.String(), "read") || !strings.Contains(errOut.String(), "Warning: check") {
		t.Fatalf("stderr should retain warnings without successful tool rows: %q", errOut.String())
	}
}

func TestLineTurnUIStopFlushesPartialRichOutput(t *testing.T) {
	var out bytes.Buffer
	ui := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface: outputSurfaceLineANSI,
		columns: 80,
	})
	ui.writer = &out
	ui.AppendAssistantText("**partial")
	ui.Stop()
	plain := ansiSGRPattern.ReplaceAllString(out.String(), "")
	if plain != "**partial\n" {
		t.Fatalf("abnormal stop output = %q", plain)
	}
	ui.Stop()
	if got := ansiSGRPattern.ReplaceAllString(out.String(), ""); got != plain {
		t.Fatalf("second stop duplicated output: %q", got)
	}
}

func TestLineTurnUISchemaSuppressesRichDisplay(t *testing.T) {
	var out bytes.Buffer
	ui := newLineTurnUIWithCapabilities(&Config{SchemaPath: "schema.json"}, nil, outputCapabilities{
		surface:       outputSurfaceLineANSI,
		imageProtocol: terminalImageKitty,
		columns:       80,
	})
	ui.writer = &out
	ui.AppendAssistantText("![x](x.png)")
	ui.FinishTextTurn()
	ui.Stop()
	if out.Len() != 0 {
		t.Fatalf("schema path emitted display output: %q", out.String())
	}
}

func TestRenderLineMarkdownImages(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/chart.png"
	writeImageFixture(t, path, 8, 4)
	source := "before\n\n![latency](chart.png)\n\nafter"

	t.Run("kitty", func(t *testing.T) {
		got := renderLineMarkdown(source, dir, outputCapabilities{
			surface:       outputSurfaceLineANSI,
			imageProtocol: terminalImageKitty,
			columns:       80,
		})
		text := string(got)
		beforeAt := strings.Index(text, "before")
		captionAt := strings.Index(text, "image: latency")
		imageAt := strings.Index(text, "\x1b_Ga=T")
		afterAt := strings.Index(text, "after")
		if beforeAt < 0 || captionAt <= beforeAt || imageAt <= captionAt || afterAt <= imageAt {
			t.Fatalf("kitty image order is wrong: %q", text)
		}
		if !strings.Contains(text, "C=1") || strings.ContainsRune(text, transcriptImageMarker(0)) {
			t.Fatalf("kitty output lost cursor policy or leaked marker: %q", text)
		}
	})

	t.Run("sixel", func(t *testing.T) {
		got := string(renderLineMarkdown(source, dir, outputCapabilities{
			surface:       outputSurfaceLineANSI,
			imageProtocol: terminalImageSixel,
			columns:       80,
		}))
		if !strings.Contains(got, "\x1b7\x1bP") || !strings.Contains(got, "\x1b\\\x1b8") {
			t.Fatalf("sixel output lacks save/payload/restore framing")
		}
		if strings.ContainsRune(got, transcriptImageMarker(0)) {
			t.Fatalf("sixel output leaked marker")
		}
	})

	t.Run("caption fallback", func(t *testing.T) {
		got := string(renderLineMarkdown(source, dir, outputCapabilities{
			surface: outputSurfaceLineANSI,
			columns: 80,
		}))
		plain := ansiSGRPattern.ReplaceAllString(got, "")
		if !strings.Contains(plain, "image: latency") || !strings.Contains(plain, "after") || strings.Contains(got, "\x1b_G") {
			t.Fatalf("caption fallback = %q", got)
		}
	})

	t.Run("narrow terminal", func(t *testing.T) {
		got := string(renderLineMarkdown(source, dir, outputCapabilities{
			surface:       outputSurfaceLineANSI,
			imageProtocol: terminalImageKitty,
			columns:       minimumImageThumbnailCols - 1,
		}))
		if strings.Contains(got, "\x1b_G") || !strings.Contains(got, "image: latency") {
			t.Fatalf("narrow fallback = %q", got)
		}
	})
}

func TestRenderLineMarkdownDoesNotOpenRemoteOrMissingImages(t *testing.T) {
	got := string(renderLineMarkdown(
		"![remote](https://example.com/a.png) ![missing](missing.png)",
		t.TempDir(),
		outputCapabilities{surface: outputSurfaceLineANSI, imageProtocol: terminalImageKitty, columns: 80},
	))
	if strings.Contains(got, "\x1b_G") || !strings.Contains(got, "example.com") || !strings.Contains(got, "missing.png") {
		t.Fatalf("remote/missing image handling = %q", got)
	}
}

func TestRenderLineMarkdownPreservesImageOrderAndIgnoresFences(t *testing.T) {
	dir := t.TempDir()
	writeImageFixture(t, dir+"/a.png", 8, 4)
	writeImageFixture(t, dir+"/b.png", 4, 8)
	got := string(renderLineMarkdown(
		"![first](a.png)\n\nmiddle\n\n```markdown\n![literal](a.png)\n```\n\n![second](b.png)",
		dir,
		outputCapabilities{surface: outputSurfaceLineANSI, imageProtocol: terminalImageKitty, columns: 80},
	))
	firstAt := strings.Index(got, "image: first")
	middleAt := strings.Index(got, "middle")
	literalAt := strings.Index(got, "![literal](a.png)")
	secondAt := strings.Index(got, "image: second")
	if firstAt < 0 || middleAt <= firstAt || literalAt <= middleAt || secondAt <= literalAt {
		t.Fatalf("image/text ordering = %q", got)
	}
	if strings.Count(got, "\x1b_Ga=T") != 2 {
		t.Fatalf("kitty display count = %d, want 2", strings.Count(got, "\x1b_Ga=T"))
	}
}

func TestLineTurnUIToolResultNeverRendersImage(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	dir := t.TempDir()
	path := dir + "/tool.png"
	writeImageFixture(t, path, 4, 4)
	var out, errOut bytes.Buffer
	ui := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface:       outputSurfaceLineANSI,
		imageProtocol: terminalImageKitty,
		columns:       80,
	})
	ui.writer = &out
	ui.errWriter = &errOut
	call := messages.ChatMessageToolCall{Name: "screenshot"}
	ui.AppendToolStart([]messages.ChatMessageToolCall{call})
	ui.AppendToolEnd(call, path, time.Millisecond, errors.New("capture failed"))
	ui.FinishTextTurn()
	if strings.Contains(out.String(), "\x1b_G") || strings.Contains(errOut.String(), "\x1b_G") {
		t.Fatalf("tool result triggered kitty graphics")
	}
}

func TestLineTurnUITypedToolImageRendersInspectionPreview(t *testing.T) {
	path := t.TempDir() + "/inspected.png"
	writeImageFixture(t, path, 8, 4)
	img, ok := resolveLocalTranscriptImage(path, "inspected.png", "")
	if !ok {
		t.Fatal("inspection fixture did not resolve")
	}
	img.Inspection = true
	img.MaxCols = inspectionImageThumbnailCols
	img.MaxRows = inspectionImageThumbnailRows

	var out, errOut bytes.Buffer
	ui := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface:       outputSurfaceLineANSI,
		imageProtocol: terminalImageKitty,
		columns:       80,
	})
	ui.writer = &out
	ui.errWriter = &errOut
	ui.stderrTTY = true
	ui.AppendToolMedia(messages.ChatMessageToolCall{Name: "view_image"}, []transcriptImage{img})

	if out.Len() != 0 {
		t.Fatalf("typed tool image polluted stdout: %q", out.String())
	}
	if got := errOut.String(); !strings.Contains(got, "viewed · inspected.png · 8×4") || !strings.Contains(got, "\x1b_Ga=T") || !strings.Contains(got, "c=24") {
		t.Fatalf("rich inspection preview = %q", got)
	}
}

func TestLineTurnUIRedirectedStderrKeepsInspectionTextOnly(t *testing.T) {
	img := transcriptImage{
		Path: "/tmp/inspected.png", Alt: "inspected.png", Width: 8, Height: 4,
		Inspection: true, MaxCols: inspectionImageThumbnailCols, MaxRows: inspectionImageThumbnailRows,
	}
	var out, errOut bytes.Buffer
	ui := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface:       outputSurfaceLineANSI,
		imageProtocol: terminalImageKitty,
		columns:       80,
	})
	ui.writer = &out
	ui.errWriter = &errOut
	ui.stderrTTY = false
	ui.AppendToolMedia(messages.ChatMessageToolCall{Name: "view_image"}, []transcriptImage{img})

	if out.Len() != 0 || strings.Contains(errOut.String(), "\x1b") {
		t.Fatalf("redirected inspection output stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if got := errOut.String(); !strings.Contains(got, "viewed · inspected.png · 8×4") {
		t.Fatalf("redirected inspection receipt = %q", got)
	}
}

func TestLineTurnUIRawToolImageEmitsTextReceiptOnly(t *testing.T) {
	img := transcriptImage{
		Path: "/tmp/inspected.png", Alt: "inspected.png", Width: 8, Height: 4,
		Inspection: true, MaxCols: inspectionImageThumbnailCols, MaxRows: inspectionImageThumbnailRows,
	}
	var out, errOut bytes.Buffer
	ui := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{
		surface:       outputSurfaceLineRaw,
		imageProtocol: terminalImageKitty,
		columns:       80,
	})
	ui.writer = &out
	ui.errWriter = &errOut
	ui.AppendToolMedia(messages.ChatMessageToolCall{Name: "view_image"}, []transcriptImage{img})

	if out.Len() != 0 || strings.Contains(errOut.String(), "\x1b") {
		t.Fatalf("raw inspection output stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if got := errOut.String(); !strings.Contains(got, "viewed · inspected.png · 8×4") {
		t.Fatalf("raw inspection receipt = %q", got)
	}
}

func TestKittyDisplayPNGChunksOnlyFirstCommandDisplays(t *testing.T) {
	got := string(kittyDisplayPNG(bytes.Repeat([]byte{0xab}, 5000), 20, 5, false))
	if strings.Count(got, "a=T") != 1 || !strings.Contains(got, "c=20") || !strings.Contains(got, "C=1") {
		t.Fatalf("kitty first chunk = %q", got[:min(len(got), 200)])
	}
	if strings.Count(got, "\x1b_G") < 2 || strings.Count(got, "m=0") != 1 {
		t.Fatalf("kitty payload was not chunked correctly")
	}
}
