package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestRestoreArtifactRequiresStore(t *testing.T) {
	store := newTestArtifactStore()
	for _, tc := range []struct {
		name   string
		config AgentConfig
		want   bool
	}{
		{name: "no store"},
		{name: "enabled", config: AgentConfig{ArtifactStore: store}, want: true},
		{name: "selected without store", config: AgentConfig{Builtins: []string{BuiltinRestoreArtifact}}},
		{name: "omitted", config: AgentConfig{ArtifactStore: store, Builtins: []string{BuiltinReadArtifact}}},
		{name: "disabled", config: AgentConfig{ArtifactStore: store, DisableTools: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tools.NewToolRegistry(nil)
			defer source.Close()
			agent := NewAgent(nil, source, tc.config)
			defer agent.Close()
			_, ok := agent.ToolRegistry().Get(BuiltinRestoreArtifact)
			if ok != tc.want || slices.Contains(tc.config.BuiltinTools(), BuiltinRestoreArtifact) != tc.want {
				t.Fatalf("restore available=%v, catalog=%v; want %v", ok, tc.config.BuiltinTools(), tc.want)
			}
			if _, ok := source.Get(BuiltinRestoreArtifact); ok {
				t.Fatal("private restore leaked into source registry")
			}
		})
	}
}

func TestRestoreArtifactConversationAndPublicationAuthorization(t *testing.T) {
	store := newTestArtifactStore()
	data := []byte("\x00\xffCRLF\r\nno final newline")
	local := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindBinary, Name: "not-the-destination", Data: data})
	private := putTestArtifact(t, store, artifacts.Blob{Data: []byte("private, not referenced")})
	published := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindBinary, Data: []byte("published\x00\r\n")})
	allowed, opens := true, 0
	agent := NewAgent(nil, tools.NewToolRegistry(nil), AgentConfig{ArtifactStore: store, OpenArtifact: func(_ context.Context, id string) (artifacts.Ref, io.ReadCloser, error) {
		opens++
		if !allowed || id != published.ID {
			return artifacts.Ref{}, nil, errors.New("not published")
		}
		return published, io.NopCloser(strings.NewReader("published\x00\r\n")), nil
	}})
	defer agent.Close()
	agent.indexArtifact(local)
	tool, ok := agent.ToolRegistry().Get(BuiltinRestoreArtifact)
	if !ok {
		t.Fatal("restore builtin missing")
	}
	if _, recall := tools.RecallStub(tool); recall {
		t.Fatal("restore is a mutation, not recall")
	}
	dir := t.TempDir()
	call := func(id, name string) (string, error) {
		return tool.Execute(context.Background(), map[string]any{"id": id, "path": filepath.Join(dir, name)})
	}
	if out, err := call(local.ID, "local"); err != nil || !strings.Contains(out, "SHA-256 verified") || opens != 0 {
		t.Fatalf("local restore = %q, %v; shared opens=%d", out, err, opens)
	}
	got, err := os.ReadFile(filepath.Join(dir, "local"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("local bytes = %q, %v", got, err)
	}
	if _, err := call(private.ID, "private"); err == nil {
		t.Fatal("restored unreferenced private artifact")
	} else {
		assertRestoreToolError(t, err, "artifact_unavailable")
	}
	if _, err := call(published.ID, "published"); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "published"))
	if err != nil || string(got) != "published\x00\r\n" {
		t.Fatalf("published bytes = %q, %v", got, err)
	}
	allowed = false
	if _, err := call(published.ID, "revoked"); err == nil {
		t.Fatal("cached shared authorization")
	}
	for _, name := range []string{"private", "revoked", "not-the-destination"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected destination %s: %v", name, err)
		}
	}
}

func TestRestoreArtifactPrivateAgentIsolation(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Data: []byte("private")})
	agent := NewAgent(nil, tools.NewToolRegistry(nil), AgentConfig{ArtifactStore: store})
	defer agent.Close()
	tool, _ := agent.ToolRegistry().Get(BuiltinRestoreArtifact)
	path := filepath.Join(t.TempDir(), "private")
	if _, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "path": path}); err == nil {
		t.Fatal("restore bypassed the conversation index")
	}
	agent.indexArtifact(ref)
	if _, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "path": path}); err != nil {
		t.Fatal(err)
	}
	other := NewAgent(nil, tools.NewToolRegistry(nil), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer other.Close()
	other.indexArtifact(ref)
	otherTool, _ := other.ToolRegistry().Get(BuiltinRestoreArtifact)
	if _, err := otherTool.Execute(context.Background(), map[string]any{"id": ref.ID, "path": filepath.Join(t.TempDir(), "out")}); err == nil {
		t.Fatal("opened another agent's private store")
	}
}

func TestRestoreArtifactCannotInheritArtifactAuthority(t *testing.T) {
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("parent-only publication")})
	opens := 0
	parent := NewAgent(nil, nil, AgentConfig{ArtifactStore: newTestArtifactStore(), OpenArtifact: func(context.Context, string) (artifacts.Ref, io.ReadCloser, error) {
		opens++
		return ref, io.NopCloser(strings.NewReader("parent-only publication")), nil
	}})
	defer parent.Close()
	for _, tc := range []struct {
		name   string
		config AgentConfig
	}{
		{name: "no store"},
		{name: "omitted", config: AgentConfig{ArtifactStore: newTestArtifactStore(), Builtins: []string{BuiltinReadArtifact}}},
		{name: "disabled", config: AgentConfig{ArtifactStore: newTestArtifactStore(), DisableTools: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := NewAgent(nil, parent.ToolRegistry(), tc.config)
			defer child.Close()
			if _, ok := child.ToolRegistry().Get(BuiltinRestoreArtifact); ok {
				t.Fatal("child inherited restore_artifact despite its configuration")
			}
		})
	}
	// A child that does install restoration must use its own artifact authority.
	child := NewAgent(nil, parent.ToolRegistry(), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer child.Close()
	tool, ok := child.ToolRegistry().Get(BuiltinRestoreArtifact)
	if !ok {
		t.Fatal("child's own restore_artifact is missing")
	}
	path := filepath.Join(t.TempDir(), "private")
	_, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "path": path})
	assertRestoreToolError(t, err, "artifact_unavailable")
	if opens != 0 {
		t.Fatalf("child consulted parent's publication authority %d times", opens)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("child restored parent-only artifact: %v", err)
	}
}

func TestRestoreArtifactInvalidArgumentsAndMetadata(t *testing.T) {
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("valid")})
	closed := false
	agent := NewAgent(nil, tools.NewToolRegistry(nil), AgentConfig{ArtifactStore: newTestArtifactStore(), OpenArtifact: func(context.Context, string) (artifacts.Ref, io.ReadCloser, error) {
		wrong := ref
		wrong.Bytes = -1
		return wrong, &restoreCloseObserver{Reader: strings.NewReader("valid"), close: func() { closed = true }}, nil
	}})
	defer agent.Close()
	tool, _ := agent.ToolRegistry().Get(BuiltinRestoreArtifact)
	path := filepath.Join(t.TempDir(), "out")
	for _, args := range []map[string]any{{"path": path}, {"id": "bad", "path": path}, {"id": ref.ID}, {"id": ref.ID, "path": 42}} {
		_, err := tool.Execute(context.Background(), args)
		assertRestoreToolError(t, err, "invalid_argument")
	}
	_, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "path": path})
	assertRestoreToolError(t, err, "artifact_unavailable")
	if !closed {
		t.Fatal("invalid shared reader was not closed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created file with invalid metadata: %v", err)
	}
}

func TestRestoreArtifactHonorsToolSelections(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []tools.DeriveOption
		want bool
	}{
		{name: "unrestricted", want: true},
		{name: "read only", opts: []tools.DeriveOption{tools.AllowTools("read_*")}},
		{name: "explicit", opts: []tools.DeriveOption{tools.AllowTools(BuiltinRestoreArtifact)}, want: true},
		{name: "denied", opts: []tools.DeriveOption{tools.DenyTools(BuiltinRestoreArtifact)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tools.NewToolRegistry(nil)
			defer root.Close()
			source := root.Derive(tc.opts...)
			defer source.Close()
			agent := NewAgent(nil, source, AgentConfig{ArtifactStore: newTestArtifactStore()})
			defer agent.Close()
			if _, ok := agent.ToolRegistry().Get(BuiltinRestoreArtifact); ok != tc.want {
				t.Fatalf("restore visible=%v, want %v", ok, tc.want)
			}
			if _, ok := agent.ToolRegistry().Get(BuiltinReadArtifact); !ok {
				t.Fatal("recall builtin behavior changed")
			}
		})
	}
}

func TestRestoreArtifactHonorsSourceSkillPolicyChanges(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "restricted")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: restricted\ndescription: read only\nallowed-tools: read_*\n---\nRead only.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	source := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	defer source.Close()
	runtime, err := tools.NewSkillRuntime(catalog, source)
	if err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(nil, source, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	if _, ok := agent.ToolRegistry().Get(BuiltinRestoreArtifact); !ok {
		t.Fatal("unrestricted restore missing")
	}
	if _, err := runtime.Activate("restricted"); err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.ToolRegistry().Get(BuiltinRestoreArtifact); ok {
		t.Fatal("restore bypassed source skill restriction")
	}
	if _, ok := agent.ToolRegistry().Get(BuiltinReadArtifact); !ok {
		t.Fatal("recall exemption changed")
	}
}

func TestRestoreArtifactBindingUsesCurrentRootAndPolicy(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Data: []byte("artifact")})
	source := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	defer source.Close()
	agent := NewAgent(nil, source, AgentConfig{ArtifactStore: store})
	defer agent.Close()
	agent.indexArtifact(ref)
	for _, readOnly := range []bool{false, true} {
		root := t.TempDir()
		ec, err := source.ExecutionPolicy(root, tools.ExecutionGrant{ReadOnly: readOnly})
		if err != nil {
			t.Fatal(err)
		}
		ec.BuiltinTools = BuiltinToolNames()
		bound, _, err := source.BindExecutionContext(ec, []string{BuiltinRestoreArtifact})
		if err != nil {
			t.Fatal(err)
		}
		defer bound.Close()
		original, _ := agent.ToolRegistry().Get(BuiltinRestoreArtifact)
		tool, err := original.(tools.ContextTool).BindExecutionContext(bound, ec)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tool.Execute(context.Background(), map[string]any{"id": ref.ID, "path": "relative"})
		if readOnly {
			assertRestoreToolError(t, err, "artifact_restore_failed")
			if _, err := os.Stat(filepath.Join(root, "relative")); !os.IsNotExist(err) {
				t.Fatalf("wrote in read-only bound root: %v", err)
			}
		} else if data, readErr := os.ReadFile(filepath.Join(root, "relative")); err != nil || readErr != nil || string(data) != "artifact" {
			t.Fatalf("bound root restore: %q, %v, %v", data, err, readErr)
		}
	}
}

func TestRestoreArtifactPrivateInstallationAfterBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		allow  []string
		denied bool
		want   bool
	}{
		{name: "all", want: true},
		{name: "explicit", allow: []string{BuiltinRestoreArtifact}, want: true},
		{name: "empty", allow: []string{}},
		{name: "read only", allow: []string{BuiltinReadArtifact}},
		{name: "source denies", denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
			defer source.Close()
			if tc.denied {
				source = source.Derive(tools.DenyTools(BuiltinRestoreArtifact))
				defer source.Close()
				// Derive is not a native binding source; NativeOpenTools is not
				// needed to exercise parent restrictions here.
				agent := NewAgent(nil, source, AgentConfig{ArtifactStore: newTestArtifactStore()})
				defer agent.Close()
				if _, ok := agent.ToolRegistry().Get(BuiltinRestoreArtifact); ok {
					t.Fatal("source denial bypassed")
				}
				return
			}
			root := t.TempDir()
			binding, err := tools.NativeOpenTools(source)(context.Background(), tools.ToolScope{Root: root, AllowedTools: tc.allow})
			if err != nil {
				t.Fatal(err)
			}
			defer binding.Close()
			store := newTestArtifactStore()
			ref := putTestArtifact(t, store, artifacts.Blob{Data: []byte("bound artifact\x00\r\n")})
			agent := NewAgent(nil, binding.Registry, AgentConfig{ArtifactStore: store})
			defer agent.Close()
			agent.indexArtifact(ref)
			tool, ok := agent.ToolRegistry().Get(BuiltinRestoreArtifact)
			if ok != tc.want {
				t.Fatalf("restore visible=%v, want %v", ok, tc.want)
			}
			if tc.want {
				if _, err := tool.Execute(context.Background(), map[string]any{"id": ref.ID, "path": "restored"}); err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(filepath.Join(root, "restored")); err != nil || string(data) != "bound artifact\x00\r\n" {
					t.Fatalf("native binding restoration = %q, %v", data, err)
				}
			}
			if _, ok := agent.ToolRegistry().Get(BuiltinReadArtifact); !ok {
				t.Fatal("recall builtin behavior changed")
			}
		})
	}
}

func TestRestoreArtifactDisableToolsRejectsFabricatedCall(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Data: []byte("artifact")})
	path := filepath.Join(t.TempDir(), "out")
	calls := 0
	client := ownershipLLM(func(_ context.Context, req *CompletionRequest) messages.ChatMessage {
		calls++
		if len(req.Tools) != 0 {
			t.Fatal("disabled tools advertised")
		}
		if calls == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "restore", Name: BuiltinRestoreArtifact, Arguments: `{"id":"` + ref.ID + `","path":"` + filepath.ToSlash(path) + `"}`}}}
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	})
	agent := NewAgent(client, tools.NewToolRegistry(nil), AgentConfig{ArtifactStore: store, DisableTools: true})
	defer agent.Close()
	if slices.Contains(agent.config.BuiltinTools(), BuiltinRestoreArtifact) {
		t.Fatal("disabled restore builtin selected")
	}
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "restore", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "tool execution is disabled") {
		t.Fatalf("disabled restore call: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled tools restored file: %v", err)
	}
}

func assertRestoreToolError(t *testing.T, err error, code string) {
	t.Helper()
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != code {
		t.Fatalf("error = %v; want ToolError %q", err, code)
	}
}

type restoreCloseObserver struct {
	io.Reader
	close func()
}

func (r *restoreCloseObserver) Close() error { r.close(); return nil }
