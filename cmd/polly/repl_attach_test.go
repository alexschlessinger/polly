package main

import (
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ui "github.com/metaspartan/gotui/v5"
)

func TestSplitDroppedPaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "/tmp/a.png", []string{"/tmp/a.png"}},
		{"multiple", "/tmp/a.png /tmp/b.png", []string{"/tmp/a.png", "/tmp/b.png"}},
		{"single quoted", "'/tmp/with space.png'", []string{"/tmp/with space.png"}},
		{"double quoted", `"C:\Users\alex\shot.png"`, []string{`C:\Users\alex\shot.png`}},
		{"escaped space", `/tmp/Screen\ Shot.png`, []string{"/tmp/Screen Shot.png"}},
		{"windows separators survive", `C:\Users\alex\shot.png`, []string{`C:\Users\alex\shot.png`}},
		{"newline separated", "/tmp/a.png\n/tmp/b.png", []string{"/tmp/a.png", "/tmp/b.png"}},
		{"trailing space", "/tmp/a.png ", []string{"/tmp/a.png"}},
		{"unterminated quote", "'/tmp/a.png", nil},
		{"empty", "   ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitDroppedPaths(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitDroppedPaths(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitDroppedPaths(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// sendPasted drives text through the REPL as one bracketed paste, using the
// same per-key events the terminal pump would deliver.
func sendPasted(r *managedREPL, text string) {
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	send(pasteStartID)
	for _, ch := range text {
		switch ch {
		case '\n':
			send("<Enter>")
		case ' ':
			send("<Space>")
		case '\t':
			send("<Tab>")
		default:
			send(string(ch))
		}
	}
	send(pasteEndID)
}

func TestPasteConvertsDroppedImagePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Screen Shot.png")
	writeImageFixture(t, path, 8, 6)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	sendPasted(r, strings.ReplaceAll(path, " ", `\ `))

	if got := r.model.ed.text(); got != "[image #1] " {
		t.Fatalf("editor after image-path paste = %q, want %q", got, "[image #1] ")
	}
	att, ok := r.model.attachments[1]
	if !ok || att.Path != path {
		t.Fatalf("attachment registry = %+v, want path %q at #1", r.model.attachments, path)
	}
}

func TestPasteKeepsProseAndNonImagePathsAsText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 8, 6)

	prose := "see " + path
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	sendPasted(r, prose)
	if got := r.model.ed.text(); got != prose {
		t.Fatalf("prose paste = %q, want %q", got, prose)
	}

	r = newManagedREPL(&Config{}, "ctx", 0, 0)
	missing := filepath.Join(dir, "missing.png")
	sendPasted(r, missing)
	if got := r.model.ed.text(); got != missing {
		t.Fatalf("missing-file paste = %q, want %q", got, missing)
	}
	if len(r.model.attachments) != 0 {
		t.Fatalf("no attachments expected, got %+v", r.model.attachments)
	}
}

func TestPromptAttachmentsResolveInTokenOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.png")
	second := filepath.Join(dir, "b.png")
	writeImageFixture(t, first, 4, 4)
	writeImageFixture(t, second, 4, 4)

	m := newReplModel()
	tokenA := m.registerAttachment(first, "a.png")
	tokenB := m.registerAttachment(second, "b.png")
	if tokenA != "[image #1]" || tokenB != "[image #2]" {
		t.Fatalf("tokens = %q, %q", tokenA, tokenB)
	}

	got := m.promptAttachments("compare [image #2] with [image #1], again [image #2], and [image #9]")
	if len(got) != 2 || got[0].Path != second || got[1].Path != first {
		t.Fatalf("promptAttachments = %+v, want [%s %s]", got, second, first)
	}
	if atts := m.promptAttachments("no tokens here"); atts != nil {
		t.Fatalf("expected nil for token-free prompt, got %+v", atts)
	}
}

func TestPrepareImageForUploadPassthroughAndDownscale(t *testing.T) {
	dir := t.TempDir()

	small := filepath.Join(dir, "small.png")
	writeImageFixture(t, small, 32, 16)
	part, err := prepareImageForUpload(small)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(small)
	if part.MimeType != "image/png" || part.ImageData != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("small image should ship byte-for-byte as png, got mime %q", part.MimeType)
	}
	if part.FileName != "small.png" {
		t.Fatalf("FileName = %q", part.FileName)
	}

	big := filepath.Join(dir, "big.png")
	writeImageFixture(t, big, 2000, 500)
	part, err = prepareImageForUpload(big)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if format != "png" || config.Width != uploadMaxLongEdge || config.Height != 392 {
		t.Fatalf("downscaled = %s %dx%d, want png %dx392", format, config.Width, config.Height, uploadMaxLongEdge)
	}

	photo := filepath.Join(dir, "photo.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	file, err := os.Create(photo)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	part, err = prepareImageForUpload(photo)
	if err != nil {
		t.Fatal(err)
	}
	if part.MimeType != "image/jpeg" {
		t.Fatalf("jpeg mime = %q", part.MimeType)
	}
}

func TestBuildREPLUserMessage(t *testing.T) {
	msg, err := buildREPLUserMessage("plain prompt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "plain prompt" || len(msg.Parts) != 0 {
		t.Fatalf("text-only message = %+v", msg)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 8, 8)
	msg, err = buildREPLUserMessage("look [image #1]", []composerAttachment{{Path: path, Label: "shot.png"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "" || len(msg.Parts) != 2 {
		t.Fatalf("multimodal message shape = %+v", msg)
	}
	if msg.Parts[0].Type != "text" || msg.Parts[0].Text != "look [image #1]" {
		t.Fatalf("leading text part = %+v", msg.Parts[0])
	}
	if msg.Parts[1].Type != "image_base64" || msg.Parts[1].MimeType != "image/png" {
		t.Fatalf("image part = %+v", msg.Parts[1])
	}

	if _, err := buildREPLUserMessage("x", []composerAttachment{{Path: filepath.Join(dir, "gone.png")}}); err == nil {
		t.Fatal("missing attachment file should fail the build")
	}
}

func TestBeginTurnEchoesAttachmentThumbnails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 8, 8)

	m := newReplModel()
	token := m.registerAttachment(path, "shot.png")
	prompt := "describe " + token + " " + string(transcriptImageMarker(0))
	m.beginTurn(prompt)

	idx := len(m.transcript) - 1
	entry := m.transcript[idx]
	if !strings.Contains(entry, "image: shot.png") {
		t.Fatalf("user echo lacks attachment caption: %q", entry)
	}
	if !strings.ContainsRune(entry, transcriptImageMarker(0)) {
		t.Fatal("user echo lacks thumbnail slot markers")
	}
	// The prompt's own private-use rune was stripped; only the slot rows carry
	// marker runes, all below the caption line.
	if strings.ContainsRune(strings.SplitN(entry, "\n", 2)[0], transcriptImageMarker(0)) {
		t.Fatal("pasted marker rune survived in the echoed prompt line")
	}
	if imgs := m.transcriptImages[idx]; len(imgs) != 1 || imgs[0].Path != path {
		t.Fatalf("sidecar images = %+v", m.transcriptImages[idx])
	}
}

func TestClipboardImageCommands(t *testing.T) {
	dest := "/cache/clip-1.png"
	has := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, n := range names {
				if n == name {
					return "/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	env := func(pairs map[string]string) func(string) string {
		return func(key string) string { return pairs[key] }
	}

	cmds := clipboardImageCommands("darwin", env(nil), has("pngpaste"), dest)
	if len(cmds) != 3 || cmds[0].argv[0] != "pngpaste" || cmds[1].argv[0] != "osascript" {
		t.Fatalf("darwin with pngpaste = %+v", cmds)
	}
	cmds = clipboardImageCommands("darwin", env(nil), has(), dest)
	if len(cmds) != 2 || cmds[0].argv[0] != "osascript" {
		t.Fatalf("darwin without pngpaste = %+v", cmds)
	}
	cmds = clipboardImageCommands("linux", env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"}), has("wl-paste", "xclip"), dest)
	if len(cmds) != 2 || cmds[0].argv[0] != "wl-paste" || !cmds[0].toStdout || cmds[1].argv[0] != "xclip" {
		t.Fatalf("wayland linux = %+v", cmds)
	}
	cmds = clipboardImageCommands("linux", env(nil), has("xclip"), dest)
	if len(cmds) != 1 || cmds[0].argv[0] != "xclip" {
		t.Fatalf("x11 linux = %+v", cmds)
	}
	if cmds = clipboardImageCommands("linux", env(nil), has(), dest); len(cmds) != 0 {
		t.Fatalf("bare linux should have no candidates, got %+v", cmds)
	}
	cmds = clipboardImageCommands("windows", env(nil), has(), dest)
	if len(cmds) != 1 || cmds[0].argv[0] != "powershell" {
		t.Fatalf("windows = %+v", cmds)
	}
}

func TestReplAttachCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "board layout.png")
	writeImageFixture(t, path, 8, 8)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	ctx := newManagedReplCommandContext(r)

	handled, quit := r.runCommand("/attach '" + path + "'")
	if !handled || quit {
		t.Fatalf("handled=%v quit=%v", handled, quit)
	}
	if got := r.model.ed.text(); got != "[image #1] " {
		t.Fatalf("editor after /attach = %q", got)
	}

	res := replAttachCommand(ctx, []string{"/attach", filepath.Join(dir, "missing.png")})
	if res.quit || res.err != nil {
		t.Fatalf("attach error path should reply, not fail: %+v", res)
	}
	if got := r.model.ed.text(); got != "[image #1] " {
		t.Fatalf("failed attach must not touch the editor, got %q", got)
	}
}

func TestStripTranscriptImageMarkers(t *testing.T) {
	in := "a" + string(transcriptImageMarker(0)) + "b" + string(transcriptImageMarker(255)) + "c\uE200d"
	if got := stripTranscriptImageMarkers(in); got != "abc\uE200d" {
		t.Fatalf("stripTranscriptImageMarkers = %q", got)
	}
}
