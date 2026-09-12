package swarm

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

type memberLeaseStore struct {
	sessions.SessionStore
	acquired chan sessions.Session
}

func (s *memberLeaseStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	got, err := s.SessionStore.Acquire(ctx, name, opts)
	if err == nil && opts.ExistingOnly {
		s.acquired <- got
	}
	return got, err
}

type memberLeaseTool struct{ *tools.Func }

func (memberLeaseTool) ContextIndependent() bool { return true }

func TestMemberLeaseLossCancelsActiveWork(t *testing.T) {
	for _, mode := range []string{"provider", "tool", "approval"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
			defer registry.Close()
			observed := &memberLeaseStore{SessionStore: store, acquired: make(chan sessions.Session, 1)}
			started := make(chan context.Context, 1)
			factory := make(chan context.Context, 1)
			release := make(chan struct{})
			wait := func(workCtx context.Context) {
				started <- workCtx
				select {
				case <-workCtx.Done():
				case <-release:
				}
			}
			registry.Register(memberLeaseTool{&tools.Func{Name: "lease_work", Run: func(workCtx context.Context, _ tools.Args) (string, error) {
				if mode == "tool" {
					wait(workCtx)
				}
				return "done", workCtx.Err()
			}}})
			r, err := New(Config{Store: observed, Parent: parent, Registry: registry,
				Client: modelFunc(func(workCtx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
					if mode == "provider" {
						wait(workCtx)
						return answer("done")
					}
					for _, message := range req.Messages {
						if message.ToolCallID == "work" {
							return answer("done")
						}
					}
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
						ToolCalls: []messages.ChatMessageToolCall{{ID: "work", Name: "lease_work", Arguments: `{}`}}}
				}), Root: t.TempDir(), Directory: filepath.Join(t.TempDir(), "members"),
				Callbacks: func(workCtx context.Context, _ Member) *llm.AgentCallbacks {
					factory <- workCtx
					if mode == "approval" {
						return &llm.AgentCallbacks{ApproveToolCalls: func(calls []messages.ChatMessageToolCall) []bool {
							wait(workCtx)
							return make([]bool, len(calls))
						}}
					}
					return nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer close(release)
			done := make(chan error, 1)
			go func() {
				_, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
				done <- err
			}()
			var activeCtx context.Context
			select {
			case activeCtx = <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			child := <-observed.acquired
			name, err := child.GetName(ctx)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// Simulate another process taking over an expired lease, without
			// sleeping for the production 20-second expiry and heartbeat.
			if _, err := db.ExecContext(ctx, "UPDATE session_leases SET owner_token=zeroblob(16) WHERE session_id=(SELECT id FROM sessions WHERE name=?)", name); err != nil {
				t.Fatal(err)
			}
			if _, err := child.GetMetadata(ctx); !errors.Is(err, sessions.ErrSessionLeaseLost) {
				t.Fatalf("expected lease loss, got %v", err)
			}
			for _, workCtx := range []context.Context{activeCtx, <-factory} {
				select {
				case <-workCtx.Done():
					if !errors.Is(context.Cause(workCtx), sessions.ErrSessionLeaseLost) {
						t.Fatalf("lost cancellation cause: %v", context.Cause(workCtx))
					}
				case <-ctx.Done():
					t.Fatal("member lease loss did not cancel active work")
				}
			}
			select {
			case err := <-done:
				if !errors.Is(err, sessions.ErrSessionLeaseLost) {
					t.Fatalf("lease failure was not reported: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("member did not settle after lease loss")
			}
		})
	}
}
