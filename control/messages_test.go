package control

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

type fakeRefs struct{ asked []string }

func (f *fakeRefs) OutputRef(output string, size int64, mime, name string) artifacts.Ref {
	f.asked = append(f.asked, output)
	return artifacts.Ref{ID: "sha256:" + strings.Repeat("a", 64), Kind: artifacts.KindImage, MIMEType: mime, Name: name, ImageToken: "[image " + output + "]", Bytes: size}
}

func TestLinesConvertBothWaysWithoutLoss(t *testing.T) {
	lines := []string{
		`{"role":"user","content":"hi","agent_synthetic":true}`,
		`{"role":"assistant","content":"done","text_blocks":[{"phase":"commentary","text":"looking"},{"id":"b1","text":"done"}],"tool_calls":[{"id":"call_1","name":"bash","arguments":"{\"command\":\"ls\"}","display_id":"d1"},{"id":"call_2","name":"todo"}],"reasoning":"think","stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":42,"tokens_reported":true,"cache_read_tokens":3,"cache_reported":true},"metadata":{"anthropic_thinking_blocks":[{"signature":"x"}]}}`,
		`{"role":"tool","content":"boom","tool_call_id":"call_1","tool_name":"bash","error_text":"exit 1","tool_succeeded":false}`,
		`{"role":"internal","metadata":{"compaction":{"summary":"what happened","keeps_turn":true,"removed_tokens":42}}}`,
		`{"role":"user","parts":[{"kind":"text","text":"see"},{"kind":"image_base64","image_data":"AAAA","mime_type":"image/png","file_name":"a.png"},{"kind":"image_url","image_url":"https://x/y.png"}]}`,
	}
	for _, raw := range lines {
		msg, err := LineToMessage(json.RawMessage(raw), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		back, err := MessageToLine(msg, nil)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var want, got any
		_ = json.Unmarshal([]byte(raw), &want)
		_ = json.Unmarshal(back, &got)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("round trip\n got %s\nwant %s", back, raw)
		}
	}
}

func TestLineFieldsLandWherePollyReadsThem(t *testing.T) {
	msg, err := LineToMessage(json.RawMessage(`{"role":"assistant","content":"x","tool_calls":[{"id":"c","name":"bash"}],"usage":{"input_tokens":7,"output_tokens":42,"tokens_reported":true,"cache_write_tokens":5,"cache_reported":true},"error_text":"stream cut"}`), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.GetInputTokens() != 7 || msg.GetOutputTokens() != 42 || msg.GetCacheWriteInputTokens() != 5 {
		t.Fatalf("usage = %+v", msg.Metadata)
	}
	if isErr, _ := msg.Metadata[messages.MetadataKeyIsError].(bool); !isErr || msg.Metadata[messages.MetadataKeyError] != "stream cut" {
		t.Fatalf("error = %+v", msg.Metadata)
	}
	if msg.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("a call without arguments = %+v", msg.ToolCalls[0])
	}
	marker, err := LineToMessage(json.RawMessage(`{"role":"internal","metadata":{"compaction":{"summary":"s","keeps_turn":true}}}`), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := marker.Compaction(); !ok || c.Summary != "s" || !c.KeepsTurn {
		t.Fatalf("compaction marker = %+v", marker)
	}
}

func TestSavedImagesBecomeArtifactsOfTheirSession(t *testing.T) {
	refs := &fakeRefs{}
	msg, err := LineToMessage(json.RawMessage(`{"role":"tool","tool_call_id":"v","parts":[{"kind":"image_file","path":"/r/sessions/abc/outputs/shot-2.png","bytes":12,"mime_type":"image/png","file_name":"shot.png"}]}`), "abc", refs)
	if err != nil {
		t.Fatal(err)
	}
	part := msg.Parts[0]
	if part.Type != "image_artifact" || part.Artifact == nil || part.Artifact.Bytes != 12 || part.Reference != "[image shot-2.png]" {
		t.Fatalf("part = %+v", part)
	}
	if len(refs.asked) != 1 || refs.asked[0] != "shot-2.png" {
		t.Fatalf("outputs asked = %v", refs.asked)
	}
	// Going the other way an artifact crosses inline.
	back, err := LineOf(messages.ChatMessage{Role: "user", Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: part.Artifact, MimeType: "image/png"}}}, func(*artifacts.Ref) ([]byte, error) { return []byte("img"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if back.Parts[0].Kind != "image_base64" || back.Parts[0].ImageData != "aW1n" {
		t.Fatalf("inline part = %+v", back.Parts[0])
	}
	if _, err := LineOf(messages.ChatMessage{Role: "user", Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: part.Artifact}}}, nil); err == nil {
		t.Fatal("an artifact crossed without a reader")
	}
}

func TestParseTarget(t *testing.T) {
	for _, bad := range []string{"", "polly-zig", "ssh://host/x", "unix://host/x.sock", "unix:relative.sock", "exec://name/extra", "exec:///abs?x=1", "unix:///s.sock?dir=rel"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	u, err := ParseTarget("unix:///tmp/x/serve.sock?runtime=/r&dir=/w&home=/h")
	if err != nil || u.Socket != "/tmp/x/serve.sock" || u.Runtime != "/r" || u.Dir != "/w" || u.Home != "/h" {
		t.Fatalf("unix target = %+v, %v", u, err)
	}
	e, err := ParseTarget("exec:///opt/polly-zig/bin/polly-zig")
	if err != nil || e.Binary != "/opt/polly-zig/bin/polly-zig" {
		t.Fatalf("exec target = %+v, %v", e, err)
	}
	if _, err := ParseTarget("exec://sh"); err != nil {
		t.Fatalf("exec://sh: %v", err)
	}
}

func TestPromptEnvClearsSessionVariablesAndLeavesKeysOut(t *testing.T) {
	env := PromptEnv([]string{"PATH=/bin", "TERM=xterm", "POLLYTOOL_OPENAIKEY=sk", "POLLYTOOL_GEMINIKEY=g", "VIRTUAL_ENV=/v", "ZED=1", "ABC=2", "POLLY_ZIG_PIPES=x", "POLLYTOOL_IDLE=1s"})
	got := map[string]*string{}
	var order []string
	for _, o := range env {
		got[o.Name] = o.Value
		order = append(order, o.Name)
	}
	for _, name := range []string{"PATH", "POLLYTOOL_OPENAIKEY", "POLLYTOOL_GEMINIKEY", "POLLY_ZIG_PIPES", "POLLYTOOL_IDLE"} {
		if _, ok := got[name]; ok {
			t.Fatalf("%s crossed in a prompt's env", name)
		}
	}
	if got["TERM"] == nil || *got["TERM"] != "xterm" || got["TMUX"] != nil {
		t.Fatalf("session variables = %v", got)
	}
	if _, ok := got["TMUX"]; !ok {
		t.Fatal("an absent session variable is not cleared")
	}
	if n := len(order); order[n-2] != "ABC" || order[n-1] != "ZED" {
		t.Fatalf("extra variables are not sorted at the end: %v", order)
	}
	data, _ := json.Marshal(env[0])
	if !strings.HasPrefix(string(data), `["_",`) {
		t.Fatalf("pair = %s", data)
	}
}

func TestPlacesAndSetupKeysFollowTheRepositoryAndSplitOnWhatMatters(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) string {
		p := root + "/" + rel
		if err := mkdirAll(p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	work := mk("repo/a/b")
	mk("repo/.agents/skills/one")
	writeFile(t, root+"/repo/.git", "gitdir: x\n")
	writeFile(t, root+"/repo/.agents/skills/one/SKILL.md", "---\nname: one\n---\n")
	places, err := Places(work, root+"/home", root+"/rt/skills", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(places) != 4 || places[0].Kind != PlacePersonal || places[1].Path != root+"/repo/.agents/skills" || places[3].Path != work+"/.agents/skills" {
		t.Fatalf("places = %+v", places)
	}
	named, _ := Places(work, root+"/home", root+"/rt/skills", []string{"~/s", "rel", "rel"})
	if len(named) != 2 || named[0].Path != root+"/home/s" || named[1].Path != work+"/rel" || named[0].Kind != PlaceNamed {
		t.Fatalf("named places = %+v", named)
	}
	base := SetupKeyInput{Version: "v", Program: Program{Path: "/bin/x", Size: 1, Changed: 2}, RuntimeDir: root + "/rt", Provider: "openai", Natives: true, Dir: work, Home: root + "/home", Skills: places, Environ: []string{"PATH=/bin", "HOME=" + root + "/home", "PWD=/somewhere", "POLLYTOOL_OPENAIKEY=sk-1"}}
	key := func(s SetupKeyInput) string {
		k, err := SetupKey(s)
		if err != nil {
			t.Fatal(err)
		}
		if !IsID(k) {
			t.Fatalf("key %q", k)
		}
		return k
	}
	k0 := key(base)
	same := base
	same.Environ = append([]string{"PWD=/elsewhere", "TERM=dumb"}, base.Environ[:3]...)
	same.Environ = append(same.Environ, "POLLYTOOL_OPENAIKEY=sk-1")
	if key(same) != k0 {
		t.Fatal("a per-prompt variable split the key")
	}
	for name, change := range map[string]func(*SetupKeyInput){
		"provider key": func(s *SetupKeyInput) { s.Environ = append(s.Environ[:3:3], "POLLYTOOL_OPENAIKEY=sk-2") },
		"version":      func(s *SetupKeyInput) { s.Version = "w" },
		"program":      func(s *SetupKeyInput) { s.Program.Changed++ },
		"natives":      func(s *SetupKeyInput) { s.Natives = false },
		"base url":     func(s *SetupKeyInput) { s.BaseURL = "http://x" },
		"setup env":    func(s *SetupKeyInput) { s.Environ = append(append([]string{}, s.Environ...), "TZ=UTC") },
		"opted in": func(s *SetupKeyInput) {
			s.Environ = append(append([]string{}, s.Environ...), "SETUP_ME=1", SetupEnvVar+"=SETUP_ME")
		},
		"workspace": func(s *SetupKeyInput) { s.Workspace = "ssh:box" },
	} {
		s := base
		s.Environ = append([]string{}, base.Environ...)
		change(&s)
		if key(s) == k0 {
			t.Fatalf("%s did not split the key", name)
		}
	}
	// A skill manifest's change is seen.
	writeFile(t, root+"/repo/.agents/skills/one/SKILL.md", "---\nname: one\ndescription: longer\n---\n")
	if key(base) == k0 {
		t.Fatal("a rewritten skill did not split the key")
	}
}

func TestBuildKeyIsTheBuildsAlone(t *testing.T) {
	a := BuildKey("0.5.0", Program{Path: "/bin/x", Size: 10, Changed: 99})
	if !IsID(a) || a == BuildKey("0.5.0", Program{Path: "/bin/x", Size: 10, Changed: 100}) || a == BuildKey("0.5.1", Program{Path: "/bin/x", Size: 10, Changed: 99}) {
		t.Fatalf("build keys do not tell builds apart: %s", a)
	}
}

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
