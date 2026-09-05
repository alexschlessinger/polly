// Package subagent lets a model delegate work to a child agent. The parent
// calls the spawn_agent tool with a brief; a child runs to completion in a
// fresh conversation with its own context window and a narrower view of the
// parent's tools, and the tool returns the child's final reply along with
// the session that holds its transcript. What running a child means is the
// host's choice, given as a Runner: the polly CLI opens a child session on
// the same store, and AgentRunner runs an in-memory agent for library use.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// ToolName is the tool the model calls to spawn a child agent.
const ToolName = "spawn_agent"

// DefaultMaxConcurrent bounds how many children one tool runs at once; a
// parent that calls the tool more times in one batch waits for a slot.
const DefaultMaxConcurrent = 4

// Request is a parent's brief for one child, as the model wrote it.
type Request struct {
	// Task is the brief. It is everything the child knows.
	Task string
	// Label names the job in a few words for the people watching.
	Label string
	// Tools lists the tool names or globs the child may use; empty means the
	// parent's tools. The child never gets spawn_agent itself.
	Tools []string
	// Model overrides the parent's model when set.
	Model string
	// MaxIterations caps the child's model calls when positive.
	MaxIterations int
	// Background asks the host to return as soon as the child has started
	// and deliver its reply later as a message to the parent. A host that
	// cannot deliver later runs the child to completion instead.
	Background bool
}

// Result is what a child returned.
type Result struct {
	// Text is the child's final reply.
	Text string
	// Session names the session holding the child's transcript, empty when
	// the child ran without one.
	Session string
	// InputTokens and OutputTokens are the child's own usage, reported
	// separately from the parent's.
	InputTokens  int
	OutputTokens int
	// Started marks a background child that is running; its reply follows
	// as a later message.
	Started bool
	// Done, for a started child, is closed by the host once the child has
	// settled. The tool keeps the child's concurrency slot until then, so
	// the cap counts children that are running, not calls that are waiting.
	// A host that leaves it nil frees the slot when the call returns.
	Done <-chan struct{}
}

// String is the tool result the parent model reads: the reply, then the
// session to find the transcript in; for a child still running, where it
// runs and how its reply arrives.
func (r Result) String() string {
	if r.Started {
		text := "started; the agent is working in the background and its reply will arrive as a message when it finishes"
		if r.Session != "" {
			text += "\n\n(agent session " + r.Session + ")"
		}
		return text
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		text = "(the agent returned no reply)"
	}
	if r.Session == "" {
		return text
	}
	trailer := "(agent session " + r.Session
	if r.InputTokens > 0 || r.OutputTokens > 0 {
		trailer += fmt.Sprintf(" · %d in / %d out", r.InputTokens, r.OutputTokens)
	}
	return text + "\n\n" + trailer + ")"
}

// Runner runs one child to completion. A failed run may still name the
// child's session in its Result so the failure can be inspected.
type Runner func(ctx context.Context, req Request) (Result, error)

// Option configures the tool.
type Option func(*Tool)

// WithMaxConcurrent sets how many children may run at once; n < 1 keeps the
// default.
func WithMaxConcurrent(n int) Option {
	return func(t *Tool) {
		if n > 0 {
			t.slots = make(chan struct{}, n)
		}
	}
}

// Tool is the spawn_agent tool. It parses the model's brief, bounds how many
// children run at once, and hands each brief to its Runner.
type Tool struct {
	run   Runner
	slots chan struct{}
}

// NewTool builds the spawn_agent tool over run.
func NewTool(run Runner, opts ...Option) *Tool {
	t := &Tool{run: run, slots: make(chan struct{}, DefaultMaxConcurrent)}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *Tool) GetName() string   { return ToolName }
func (t *Tool) GetType() string   { return "native" }
func (t *Tool) GetSource() string { return "builtin" }

// Untimed exempts the tool from the agent's per-tool timeout: a child agent
// runs as long as its own iteration cap and the parent's turn allow.
func (t *Tool) Untimed() bool { return true }

func (t *Tool) GetSchema() *schema.ToolSchema {
	return schema.Tool(ToolName,
		"Delegate a self-contained task to a child agent. The agent works in its own conversation with its own context window, using a subset of your tools, and you receive only its final reply, so use it for work whose intermediate output you do not need: surveying a codebase, checking many files, a long investigation. The agent sees nothing of this conversation: give a complete brief with the goal, where to look, and what to report back. Call the tool several times in one turn to run agents in parallel.",
		schema.Params{
			"task":           schema.S("The complete brief for the agent. It starts with no other context."),
			"label":          schema.S("Two to five words naming the job, shown to the user while it runs."),
			"tools":          schema.Strings("Names or globs of the tools the agent may use, for example [\"read_file\", \"search_files\"]. Default: every tool you have, except spawn_agent."),
			"model":          schema.S("Model to run the agent on, as provider/model. Default: your own model."),
			"max_iterations": schema.Int("Cap on the agent's model calls. Default: your own limit."),
			"background":     schema.Bool("Return at once and keep working; the agent's reply arrives later as a message. Default false: wait for the reply."),
		},
		"task")
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) (string, error) {
	req, err := parseRequest(tools.Args(args))
	if err != nil {
		return "", tools.NewToolError(err.Error(), "INVALID_ARGS")
	}
	if err := t.acquire(ctx); err != nil {
		return "", err
	}
	res, err := t.run(ctx, req)
	if err == nil && res.Started && res.Done != nil {
		// The child runs on after this call returns; its slot stays taken
		// until the host reports it settled.
		go func() {
			<-res.Done
			t.release()
		}()
	} else {
		t.release()
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		msg := "agent failed: " + err.Error()
		if res.Session != "" {
			msg += " (session " + res.Session + ")"
		}
		return "", tools.NewToolError(msg, "AGENT_FAILED")
	}
	return res.String(), nil
}

func (t *Tool) acquire(ctx context.Context) error {
	select {
	case t.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (t *Tool) release() {
	<-t.slots
}

func parseRequest(args tools.Args) (Request, error) {
	req := Request{
		Task:          strings.TrimSpace(args.String("task")),
		Label:         strings.TrimSpace(args.String("label")),
		Model:         strings.TrimSpace(args.String("model")),
		MaxIterations: args.Int("max_iterations", 0),
		Background:    args.Bool("background"),
	}
	if req.Task == "" {
		return Request{}, errors.New("task is required: the complete brief for the agent")
	}
	if req.MaxIterations < 0 {
		return Request{}, errors.New("max_iterations must be positive")
	}
	for _, pattern := range args.StringSlice("tools") {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			req.Tools = append(req.Tools, pattern)
		}
	}
	return req, nil
}

// ChildRegistry derives a child's view of the parent's tools: those the
// brief allows, or all of them, and never spawn_agent itself, so a child
// does not spawn children of its own. The child's built-ins it registers
// later stay visible regardless (see tools.ToolRegistry.Derive).
func ChildRegistry(parent *tools.ToolRegistry, allow []string) *tools.ToolRegistry {
	opts := []tools.DeriveOption{tools.DenyTools(ToolName)}
	if len(allow) > 0 {
		opts = append(opts, tools.AllowTools(allow...))
	}
	return parent.Derive(opts...)
}

// ErrNoMatchingTools reports a brief whose tool list matched none of the
// parent's tools.
var ErrNoMatchingTools = errors.New("no tools match the requested list")

// RunnerOption configures AgentRunner.
type RunnerOption func(*agentRunner)

type agentRunner struct {
	callbacks func(Request) *llm.AgentCallbacks
}

// WithCallbacks gives each child the callbacks the factory returns for its
// request: a host can stream a child's text, watch and approve its tool
// calls, or inject the context values its tools need, and the request lets
// it name the child by its label. A nil return runs that child unobserved.
func WithCallbacks(factory func(Request) *llm.AgentCallbacks) RunnerOption {
	return func(r *agentRunner) {
		r.callbacks = factory
	}
}

// AgentRunner runs each child as an in-memory llm.Agent: the brief as the
// only user message after base's messages (a system prompt, typically),
// base's model and sampling settings unless the brief overrides the model,
// and ChildRegistry(parent, req.Tools) as its tools. Children run without a
// session, so Result.Session is empty. Without WithCallbacks a child runs
// unobserved, every tool call approved.
func AgentRunner(client llm.LLM, parent *tools.ToolRegistry, base llm.CompletionRequest, config llm.AgentConfig, opts ...RunnerOption) Runner {
	var runner agentRunner
	for _, opt := range opts {
		opt(&runner)
	}
	return func(ctx context.Context, req Request) (Result, error) {
		registry := ChildRegistry(parent, req.Tools)
		defer registry.Close()
		if len(req.Tools) > 0 && len(registry.All()) == 0 {
			return Result{}, fmt.Errorf("%w: %s", ErrNoMatchingTools, strings.Join(req.Tools, ", "))
		}
		agentConfig := config
		if req.MaxIterations > 0 {
			agentConfig.MaxIterations = req.MaxIterations
		}
		agent := llm.NewAgent(client, registry, agentConfig)
		defer agent.Close()

		childReq := base
		if req.Model != "" {
			childReq.Model = req.Model
		}
		childReq.Messages = append(append([]messages.ChatMessage(nil), base.Messages...), messages.User(req.Task)...)
		childReq.Tools = registry.All()
		callbacks := &llm.AgentCallbacks{}
		if runner.callbacks != nil {
			if cb := runner.callbacks(req); cb != nil {
				callbacks = cb
			}
		}
		resp, err := agent.Run(ctx, &childReq, callbacks)
		if err != nil {
			return Result{}, err
		}
		if resp == nil || resp.Message == nil {
			return Result{}, errors.New("agent returned no response")
		}
		res := Result{Text: resp.Message.GetContent()}
		res.InputTokens, res.OutputTokens = turnTokens(resp.AllMessages)
		return res, nil
	}
}

// turnTokens sums a run's usage the way polly reports a turn: providers
// count input per call, cumulatively, so the largest call stands for the
// run; output is summed across calls.
func turnTokens(all []messages.ChatMessage) (in, out int) {
	for _, m := range all {
		if m.Role != messages.MessageRoleAssistant {
			continue
		}
		if t := m.GetInputTokens(); t > in {
			in = t
		}
		out += m.GetOutputTokens()
	}
	return in, out
}
