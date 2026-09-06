package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// Subagents. Every conversation's model has the spawn_agent tool. A child it
// spawns gets a generated session on the same store, recorded as spawned by
// the parent; the parent's settings with the brief's overrides; a derived
// view of the parent's tools (subagent.ChildRegistry), so no MCP server
// starts again; and the parent's skills. Its turn runs the same body as any
// turn, into a child turn UI that keeps the reply and forwards approvals to
// the parent's turn UI, which the parent's turn passes down through the
// tool context. The tool answers with the reply and the child's session
// name, so the child's transcript can be resumed and read later.

type parentTurnUIKey struct{}

// withParentTurnUI hands a turn's UI to the tools it runs.
func withParentTurnUI(ctx context.Context, turnUI TurnUI) context.Context {
	return context.WithValue(ctx, parentTurnUIKey{}, turnUI)
}

func parentTurnUIFrom(ctx context.Context) TurnUI {
	turnUI, _ := ctx.Value(parentTurnUIKey{}).(TurnUI)
	return turnUI
}

type toolCallKey struct{}

// withToolCall hands a tool the call it is answering, so a spawned child can
// attach itself to that call's disclosure row.
func withToolCall(ctx context.Context, call messages.ChatMessageToolCall) context.Context {
	return context.WithValue(ctx, toolCallKey{}, call)
}

func toolCallFrom(ctx context.Context) messages.ChatMessageToolCall {
	call, _ := ctx.Value(toolCallKey{}).(messages.ChatMessageToolCall)
	return call
}

// childHost is a turn UI that can run a child on a surface of its own (the
// managed REPL gives it a tab) and deliver a background child's reply
// later. RunChild returns errNoChildHost to decline, leaving the child to
// the inline runner.
type childHost interface {
	RunChild(ctx context.Context, req subagent.Request) (subagent.Result, error)
}

var errNoChildHost = errors.New("no child host")

// registerSpawnTool gives state's model the spawn_agent tool. It is an
// agent built-in like read_transcript: always allowed, never persisted as a
// loaded tool.
func registerSpawnTool(state *conversationState, config *Config, client llm.LLM) {
	tool := subagent.NewTool(spawnRunner(config, client, state))
	state.toolRegistry.Register(tool)
	state.toolRegistry.MarkAlwaysAllowed(tool.GetName())
}

// spawnRunner runs children of parent. A child's config is the process
// config without the one-shot output settings, which describe the parent's
// stdout: its reply goes back to the parent model, not to a terminal.
func spawnRunner(config *Config, client llm.LLM, parent *conversationState) subagent.Runner {
	childConfig := *config
	childConfig.SchemaPath = ""
	childConfig.Meta = false
	childConfig.Files = nil
	return func(ctx context.Context, req subagent.Request) (subagent.Result, error) {
		if host, ok := parentTurnUIFrom(ctx).(childHost); ok {
			res, err := host.RunChild(ctx, req)
			if !errors.Is(err, errNoChildHost) {
				return res, err
			}
		}
		// Inline: the child runs to completion here, its reply the tool's
		// result. Nothing could deliver a background reply later.
		child, err := openChildState(ctx, client, parent, req)
		if err != nil {
			return subagent.Result{}, err
		}
		defer func() { _ = child.Close() }()
		name, err := child.session.GetName(ctx)
		if err != nil {
			return subagent.Result{}, fmt.Errorf("read child session name: %w", err)
		}
		ui := &childTurnUI{parent: parentTurnUIFrom(ctx)}
		userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: req.Task}
		_, err = executeTurnWithUserMessage(ctx, &childConfig, child, userMsg, nil, nil, ui, false)
		if saveErr := recordSpawnOutcome(child.session, storedChildReport(subagent.Result{}, err).Status); saveErr != nil {
			if parentUI := parentTurnUIFrom(ctx); parentUI != nil {
				parentUI.AppendWarning("could not save agent outcome: " + saveErr.Error())
			}
		}
		res := subagent.Result{Session: name}
		res.Text, res.InputTokens, res.OutputTokens = ui.result()
		return res, err
	}
}

// openChildState builds a child's runtime: a generated auto session on the
// parent's store, the derived tool view, a skill runtime on the parent's
// catalog with the parent's active skills, and an agent on the parent's
// client. The session records the parent, the brief's label, and the
// settings and tools the child runs with, so it can be resumed like any
// session.
func openChildState(ctx context.Context, client llm.LLM, parent *conversationState, req subagent.Request) (state *conversationState, retErr error) {
	store := parent.sessionStore
	if store == nil || parent.session == nil || parent.toolRegistry == nil {
		return nil, errors.New("the parent conversation cannot spawn agents")
	}
	parentName, err := parent.session.GetName(ctx)
	if err != nil {
		return nil, fmt.Errorf("read parent session name: %w", err)
	}
	parentMetadata, err := parent.session.GetMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("read parent metadata: %w", err)
	}
	name, err := generateSessionName(ctx, store)
	if err != nil {
		return nil, err
	}
	session, err := store.Acquire(ctx, name, sessions.AcquireOptions{Auto: true, Parent: parentName})
	if err != nil {
		return nil, fmt.Errorf("open child session %q: %w", name, err)
	}
	registry := subagent.ChildRegistry(parent.toolRegistry, req.Tools)
	defer func() {
		if retErr == nil {
			return
		}
		_ = registry.Close()
		retErr = closeSessionAfterError(session, retErr)
	}()
	if err := subagent.CheckChildTools(req.Tools, registry); err != nil {
		return nil, err
	}

	settings := parent.settings.clone()
	if req.Model != "" {
		settings.Model = req.Model
	}
	if req.MaxIterations > 0 {
		settings.MaxIterations = req.MaxIterations
	}
	var skillRuntime *tools.SkillRuntime
	if parent.skillRuntime != nil {
		skillRuntime, err = parent.skillRuntime.Derive(registry)
	} else {
		skillRuntime, err = newSkillRuntime(parent.skillCatalog, registry)
	}
	if err != nil {
		return nil, fmt.Errorf("inherit the parent's skills: %w", err)
	}

	metadata, err := session.GetMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("read child metadata: %w", err)
	}
	metadata.Description = spawnLabel(req.Label, req.Task)
	metadata.SpawnCallID = toolCallFrom(ctx).ID
	for _, spec := range settingSpecs {
		if spec.toMeta != nil {
			spec.toMeta(&settings, metadata)
		}
	}
	// The child resumes with the parent's loaded tools it could see, not
	// with the parent's agent built-ins, which every agent registers anew.
	metadata.ActiveTools = nil
	for _, info := range parentMetadata.ActiveTools {
		if _, _, allowed := registry.GetIfAllowed(info.Name); allowed {
			metadata.ActiveTools = append(metadata.ActiveTools, info)
		}
	}
	metadata.SkillSources = parent.skillSources
	metadata.ContextWindows = maps.Clone(parentMetadata.ContextWindows)
	if err := session.SetMetadata(ctx, metadata); err != nil {
		return nil, fmt.Errorf("write child metadata: %w", err)
	}

	artifactStore := session.ArtifactStore()
	agent := llm.NewAgent(client, registry, llm.AgentConfig{
		MaxIterations: settings.MaxIterations,
		ToolTimeout:   settings.ToolTimeout,
		ArtifactStore: artifactStore,
	})
	return &conversationState{
		sessionStore:       store,
		session:            session,
		settings:           settings,
		agent:              agent,
		artifactStore:      artifactStore,
		toolRegistry:       registry,
		skillCatalog:       parent.skillCatalog,
		skillRuntime:       skillRuntime,
		skillSources:       parent.skillSources,
		sandboxWarnings:    parent.sandboxWarnings,
		sandboxProbe:       parent.sandboxProbe,
		outputCapabilities: parent.outputCapabilities,
		contextWindows:     parent.cachedContextWindows(),
	}, nil
}

// childTurnUI is a child's turn UI: nothing shows, since the child's output
// is the parent's tool result, but the reply is kept and tool approvals go
// to the parent's UI so a confirming user still decides.
type childTurnUI struct {
	parent TurnUI

	mu    sync.Mutex
	text  strings.Builder
	reply string
	in    int
	out   int
}

func (u *childTurnUI) Start()                              {}
func (u *childTurnUI) Stop()                               {}
func (u *childTurnUI) ShowThinking(string)                 {}
func (u *childTurnUI) AppendWarning(string)                {}
func (u *childTurnUI) RecordContextUsage(int, int, bool)   {}
func (u *childTurnUI) UserMessagePersistenceStarted()      {}
func (u *childTurnUI) UserMessagePersistenceFinished(bool) {}
func (u *childTurnUI) TurnPersistenceAllowed() bool        { return true }
func (u *childTurnUI) AppendToolEnd(messages.ChatMessageToolCall, string, time.Duration, error) {
}
func (u *childTurnUI) AppendToolMedia(messages.ChatMessageToolCall, []transcriptImage) {}

func (u *childTurnUI) AppendAssistantText(content string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text.WriteString(content)
}

// AppendToolStart drops the text streamed before a tool batch: that was
// the child working, not its reply.
func (u *childTurnUI) AppendToolStart([]messages.ChatMessageToolCall) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text.Reset()
}

func (u *childTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	if u.parent != nil {
		return u.parent.ApproveToolCalls(calls)
	}
	approved := make([]bool, len(calls))
	for i := range approved {
		approved[i] = true
	}
	return approved
}

func (u *childTurnUI) RecordTurnTokens(in, out int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.in, u.out = in, out
}

func (u *childTurnUI) FinishTextTurn() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reply = u.text.String()
}

// result is the reply and usage; a turn that never finished (a failure)
// yields the text streamed so far.
func (u *childTurnUI) result() (reply string, in, out int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	reply = u.reply
	if reply == "" {
		reply = u.text.String()
	}
	return reply, u.in, u.out
}
