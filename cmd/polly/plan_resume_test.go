package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestResumePlanDefersExecutionRuntime(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		shell, skill, accept bool
	}{
		{name: "broken shell on exit", shell: true},
		{name: "broken skill on exit", skill: true},
		{name: "accept restores tools", accept: true},
		{name: "failed startup preserves approval", shell: true, accept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testDraft(t)
			base, err := planWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			loaders := []tools.ToolLoaderInfo{{Type: "native", Name: "list_dir"}, {Type: "native", Name: "bash"}}
			if tc.shell {
				loaders = append(loaders, tools.ToolLoaderInfo{Type: "shell", Name: "missing", Source: filepath.Join(t.TempDir(), "missing.sh")})
			}
			var sources []string
			if tc.skill {
				sources = []string{filepath.Join(t.TempDir(), "missing-skill")}
			}
			store := testOpenMemoryStore(t, &sessions.Metadata{ActiveTools: loaders, SkillSources: sources})
			const session = "planning"
			s := testAcquireSession(t, store, session)
			if err := s.(sessions.PlanSession).StartPlan(ctx, path, base); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			const text = "## Steps\n- Check the result\n"
			_, rev, err := readPlanDraft(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writePlanDraft(path, rev, text); err != nil {
				t.Fatal(err)
			}

			config := &Config{NoSandbox: true, NoSkills: !tc.skill}
			opener := &conversationOpener{config: config, sessionStore: store}
			state, err := opener.open(ctx, session, Settings{}, false)
			if err != nil {
				t.Fatalf("draft resume: %v", err)
			}
			defer state.Close()
			if _, ok := state.toolRegistry.Get("bash"); ok || state.skillRuntime != nil || state.swarm != nil {
				t.Fatal("draft opened with execution tools, skills, or swarm")
			}
			if _, ok := state.toolRegistry.Get("list_dir"); !ok {
				t.Fatal("draft lost its selected native reader")
			}
			md, err := state.session.GetMetadata(ctx)
			if err != nil || !slices.Equal(md.ActiveTools, loaders) || !slices.Equal(md.SkillSources, sources) {
				t.Fatalf("draft erased deferred settings: %+v %v", md, err)
			}

			var out bytes.Buffer
			command := newWriterReplCommandContext(config, state, &out)
			turns := 0
			command.startTurn = func(_ string, _ messages.ChatMessage) error {
				turns++
				if _, ok := state.toolRegistry.Get("bash"); !ok || state.resumeExecution != nil || state.swarm == nil {
					t.Fatal("implementation started before the execution runtime was restored")
				}
				return nil
			}
			dispatch := func(input string) {
				t.Helper()
				if handled, _, err := defaultReplCommands.dispatch(input, command); !handled || err != nil {
					t.Fatalf("%s: handled=%v err=%v", input, handled, err)
				}
			}
			dispatch("/plan review")
			if !strings.Contains(out.String(), "Check the result") || strings.Contains(out.String(), "execution tools") {
				t.Fatalf("draft was not reviewable: %q", out.String())
			}
			want := sessions.PlanExited
			if tc.accept {
				dispatch("/plan accept")
				want = sessions.PlanAccepted
			} else {
				dispatch("/plan exit")
			}
			p, err := currentPlan(ctx, state)
			if err != nil || p == nil || p.Mode != want {
				t.Fatalf("explicit transition was not persisted: %+v %v", p, err)
			}
			if tc.shell || tc.skill {
				if turns != 0 || !strings.Contains(out.String(), "execution tools are still deferred") {
					t.Fatalf("failed startup: turns=%d notice=%q", turns, out.String())
				}
				msg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "Proceed"}
				if _, err := executeTurnWithUserMessage(ctx, config, state, msg, nil, nil, nil, false); err == nil || !strings.Contains(err.Error(), "execution tools are unavailable") {
					t.Fatalf("prompt was not refused after failed startup: %v", err)
				}
			} else if turns != 1 {
				t.Fatalf("accepted plan should start exactly once, got %d", turns)
			}
		})
	}
}
