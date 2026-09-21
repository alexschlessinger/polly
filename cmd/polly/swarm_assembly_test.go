package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// registerSwarm opens member tools natively over the parent's registry; the
// repository instructions a member receives are read by its own binding
// from its root, after the persona and coding contract.
func TestRegisterSwarmMembersReadRepositoryInstructionsThroughTheirBinding(t *testing.T) {
	root := t.TempDir()
	skipInsideRepository(t, root)
	// registerSwarm promotes the memory store into $HOME/.pollytool/polly.db
	// on the first spawn; keep that out of the real home.
	t.Setenv("HOME", t.TempDir())
	writeRepositoryTestFile(t, filepath.Join(root, "AGENTS.md"), "MEMBER ROOT GUIDANCE\n")
	t.Chdir(root)
	var system atomic.Pointer[string]
	model := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if len(req.Messages) > 0 && req.Messages[0].Role == messages.MessageRoleSystem {
			content := req.Messages[0].Content
			system.Store(&content)
		}
		return spawnTestReply("done")
	})
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	r.config.SwarmDirectory = filepath.Join(t.TempDir(), "runtime")
	state := r.state
	state.settings = Settings{Model: "test/model", MaxTokens: 128, MaxIterations: 10, SystemPrompt: "PARENT PERSONA"}
	state.toolRegistry = tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	if err := registerSwarm(state, r.config, model); err != nil {
		t.Fatal(err)
	}
	if state.swarm == nil {
		t.Fatal("swarm not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := state.swarm.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "answer", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	got := system.Load()
	if got == nil {
		t.Fatal("member made no model request")
	}
	persona, contract, guidance := strings.Index(*got, "PARENT PERSONA"), strings.Index(*got, codingContract), strings.Index(*got, "MEMBER ROOT GUIDANCE")
	if persona < 0 || contract < 0 || guidance < 0 || !(persona < contract && contract < guidance) {
		t.Fatalf("member system prompt order persona=%d contract=%d guidance=%d:\n%s", persona, contract, guidance, *got)
	}
	if strings.Count(*got, "MEMBER ROOT GUIDANCE") != 1 {
		t.Fatalf("repository guidance inserted more than once:\n%s", *got)
	}
}
