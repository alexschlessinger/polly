package swarm

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/sessiontest"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestStorageReopenRecoversExecutionWithoutReplayingEffects(t *testing.T) {
	t.Parallel()
	for _, factory := range sessiontest.Factories() {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			fixture := factory.New(t)
			store := fixture.Open()
			parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{})
			must(err)
			parentID := parent.(sessions.ViewIdentity).ViewID()
			var modelCalls, effects atomic.Int32
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if modelCalls.Add(1) == 1 {
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "park", Name: "wait_agent", Arguments: `{}`}}}
				}
				recovered := 0
				for _, m := range req.Messages {
					if m.ToolCallID == "uncertain" && m.Content == llm.ToolInterruptedContent {
						recovered++
					}
				}
				if recovered != 1 {
					t.Errorf("recovered uncertain replies = %d", recovered)
				}
				return answer("recovered")
			})
			write := &tools.Func{Name: "external_effect", Desc: "Apply an external effect", Run: func(context.Context, tools.Args) (string, error) { effects.Add(1); return "applied", nil }}
			registry := tools.NewToolRegistry(nil)
			t.Cleanup(func() { registry.Close() })
			config := Config{Store: store, Parent: parent, Client: model, Registry: registry, Root: t.TempDir(), Directory: filepath.Join(t.TempDir(), "runtime"), MaxConcurrent: 1, MaxExecutions: 1, Agent: llm.AgentConfig{MaxIterations: 5}, OpenWorktrees: func(context.Context, worktree.Config) (*worktree.Manager, error) {
				return nil, worktree.ErrNotRepository
			}, OpenTools: func(context.Context, tools.ToolScope) (tools.ToolBinding, error) {
				r := tools.NewToolRegistry(nil)
				r.Register(write)
				return tools.ToolBinding{Registry: r, Close: r.Close}, nil
			}}
			runtime, err := New(config)
			must(err)
			t.Cleanup(func() { runtime.Close() })
			result, err := runtime.Spawn(ctx, subagent.Request{Label: "recovery", Task: "wait then recover", ReadOnly: true})
			must(err)
			if !result.Yielded {
				t.Fatal("member did not park")
			}
			must(runtime.Close())
			// Reproduce the crash boundary: intent committed, external effect applied,
			// but no tool-result checkpoint. Recovery must not repeat that effect.
			must(runtime.update(ctx, func(s *State) error {
				e := s.Executions[s.Members[result.Session].Execution]
				e.Intent = []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "uncertain", Name: "external_effect", Arguments: `{}`}}}}
				return nil
			}))
			_, err = write.Run(ctx, nil)
			must(err)
			state, err := runtime.State(ctx)
			must(err)
			member := state.Members[result.Session]
			originalName := member.Name
			child, err := store.Acquire(ctx, member.Name, sessions.AcquireOptions{ExpectedID: member.ID})
			must(err)
			ref, err := child.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("durable publication")})
			must(err)
			must(child.(sessions.CoordinationSession).UpdateCoordination(ctx, func(s *sessions.CoordinationState) error { s.Pins = []string{ref.ID}; return nil }))
			must(child.Rename(ctx, "renamed-member"))
			must(child.Close())
			must(parent.Close())
			must(store.Close())
			store = fixture.Open()
			parent, err = store.Acquire(ctx, "parent", sessions.AcquireOptions{ExpectedID: parentID})
			must(err)
			t.Cleanup(func() { parent.Close() })
			// Reuse the old display name; recovery must find the original stable ID.
			replacement, err := store.Acquire(ctx, originalName, sessions.AcquireOptions{})
			must(err)
			defer replacement.Close()
			config.Store = store
			config.Parent = parent
			runtime, err = New(config)
			must(err)
			must(runtime.Resume(ctx, result.Session, 0))
			awaitIdle(t, runtime, ctx)
			state, err = runtime.State(ctx)
			must(err)
			member = state.Members[result.Session]
			e := state.Executions[member.Execution]
			if member.Name != "renamed-member" || e.Status != "completed" || e.Iterations != 2 || len(e.Intent) != 0 || effects.Load() != 1 || modelCalls.Load() != 2 {
				t.Fatalf("bad recovery: member=%+v execution=%+v effects=%d model=%d", member, e, effects.Load(), modelCalls.Load())
			}
			for _, run := range state.Runs {
				if run.Starts != 1 {
					t.Fatalf("recovery charged another start: %+v", run)
				}
			}
			child, err = store.Acquire(ctx, member.Name, sessions.AcquireOptions{ExpectedID: member.ID})
			must(err)
			defer child.Close()
			history, err := child.GetHistory(ctx)
			must(err)
			briefs, replies := 0, 0
			for _, m := range history {
				if m.Role == messages.MessageRoleUser && strings.HasPrefix(m.Content, "wait then recover\n\nCompletion: ") {
					briefs++
				}
				if m.ToolCallID == "uncertain" {
					replies++
				}
			}
			if briefs != 1 || replies != 1 {
				t.Fatalf("duplicated recovery: briefs=%d replies=%d", briefs, replies)
			}
			untouched, err := replacement.GetHistory(ctx)
			must(err)
			if len(untouched) != 0 {
				t.Fatal("recovery wrote to a reused name")
			}
			_, reader, err := parent.(sessions.CoordinationSession).OpenPublishedArtifact(ctx, ref.ID)
			must(err)
			data, err := io.ReadAll(reader)
			must(err)
			must(reader.Close())
			if string(data) != "durable publication" {
				t.Fatal("publication lost across recovery")
			}
			must(runtime.Close())
		})
	}
}

// Capability support is per acquired handle, not implied by the root session.
type plainSession struct{ sessions.Session }
type identitySession struct {
	sessions.Session
	sessions.ViewIdentity
}
type unsupportedChildren struct {
	sessions.SessionStore
	identity bool
}

func (s *unsupportedChildren) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	h, err := s.SessionStore.Acquire(ctx, name, opts)
	if err != nil {
		return nil, err
	}
	if s.identity {
		return &identitySession{Session: h, ViewIdentity: h.(sessions.ViewIdentity)}, nil
	}
	return &plainSession{h}, nil
}

func TestStorageRejectsUnsupportedChildCapabilities(t *testing.T) {
	t.Parallel()
	t.Run("spawn", func(t *testing.T) {
		r := runtimeTest(t, doneModel(), 1, 1)
		underlying := r.config.Store
		r.config.Store = &unsupportedChildren{SessionStore: underlying}
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("unsupported session panicked: %v", p)
			}
		}()
		_, err := r.Spawn(t.Context(), subagent.Request{Label: "test", Task: "test", ReadOnly: true})
		if err == nil || !strings.Contains(err.Error(), "coordination") {
			t.Fatalf("spawn error: %v", err)
		}
		summaries, err := underlying.ListSummaries(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range summaries {
			if s.Metadata.Name != "parent" && s.InUse {
				t.Fatal("rejected child lease leaked")
			}
		}
	})
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "renamed_resume"}[renamed], func(t *testing.T) {
			r := runtimeTest(t, doneModel(), 1, 1)
			underlying := r.config.Store
			child, err := underlying.Acquire(t.Context(), "child", sessions.AcquireOptions{Parent: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			member := &Member{ID: child.(sessions.ViewIdentity).ViewID(), Name: "child"}
			name := "child"
			if renamed {
				name = "renamed"
				if err := child.Rename(t.Context(), name); err != nil {
					t.Fatal(err)
				}
			}
			child.Close()
			if err := r.update(t.Context(), func(s *State) error { s.Members[member.ID] = member; return nil }); err != nil {
				t.Fatal(err)
			}
			r.config.Store = &unsupportedChildren{SessionStore: underlying, identity: true}
			got, err := r.acquireMemberSession(t.Context(), member)
			if got != nil {
				got.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "coordination") {
				t.Fatalf("acquire error: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			again, err := underlying.Acquire(ctx, name, sessions.AcquireOptions{ExpectedID: member.ID})
			if err != nil {
				t.Fatalf("rejected handle lease leaked: %v", err)
			}
			again.Close()
		})
	}
}
