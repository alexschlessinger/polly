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
const DefaultMaxConcurrent = 32

// Request is a parent's brief for one child, as the model wrote it.
type Request struct {
	// Source is a checkout to copy into isolation; ReadOnly permits research
	// outside Git. Session and TaskID continue existing swarm work.
	Source, Session, TaskID, CallID string
	ReadOnly                        bool
	// Task is the brief. It is everything the child knows.
	Task string
	// Label names the job in a few words for the people watching.
	Label string
	// Tools lists the tool names or globs the child may use. Nil inherits the
	// parent's tools; an explicit empty slice disables every model tool.
	// The child never gets spawn_agent itself.
	Tools []string
	// Model overrides the parent's model when set.
	Model string
	// MaxIterations is a trusted host override for the child's model calls.
	// The model-facing spawn tool always inherits the configured limit.
	MaxIterations int
	// Background asks the host to return as soon as the child has started
	// and deliver its reply later as a message to the parent. A host that
	// cannot deliver later runs the child to completion instead.
	Background bool
}

// Result is what a child returned.
type Result struct {
	// Yielded releases a blocking parent when a child needs its response.
	Yielded bool
	// Text is the child's final reply.
	Text string
	// Session identifies the saved child conversation using the host's name
	// or stable ID, and is empty when the child ran without one.
	Session string
	// InputTokens and OutputTokens are the child's own usage, reported
	// separately from the parent's.
	InputTokens  int
	OutputTokens int
	// Started marks a background child that is running; its reply follows
	// as a later message.
	Started bool
	// Done is closed by the host once the child has settled or an unstarted
	// spawn has been discarded. Return it even on cancellation when a child
	// outlives the call: its concurrency slot remains held until Done closes.
	// A host that leaves it nil frees the slot when the call returns.
	Done <-chan struct{}
}

// String is the tool result the parent model reads: the reply, then the
// session to find the transcript in; for a child still running, where it
// runs and how its reply arrives.
func (r Result) String() string {
	if r.Yielded {
		return "coordination yielded; inspect addressed messages and respond before waiting again. Other children in this batch continue in the background.\n(agent session " + r.Session + ")"
	}
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

type callIDKey struct{}

// WithCallID binds the provider's call identity without accepting it from
// model-authored arguments. Durable hosts use it to reconcile retried spawns.
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey{}, id)
}

// CallID returns the provider call identity planted by WithCallID, or "".
func CallID(ctx context.Context) string {
	id, _ := ctx.Value(callIDKey{}).(string)
	return id
}

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
	run              Runner
	slots            chan struct{}
	runtimeScheduler bool
}

// WithRuntimeScheduler delegates all execution slots to a shared runtime.
// Standalone tools retain their existing local concurrency limiter.
func WithRuntimeScheduler() Option { return func(t *Tool) { t.runtimeScheduler = true } }

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
func (t *Tool) Untimed() bool     { return true }
func (t *Tool) Coordinates() bool { return true }

func (t *Tool) GetSchema() *schema.ToolSchema {
	description := "Delegate a self-contained task to a child agent with its own conversation and context window. Give a complete brief with the goal, relevant paths, constraints and authorization, validation, and what to report back. Treat files as shared unless the host explicitly provides isolation. Give editing agents non-overlapping files, avoid changing those files while they run, and inspect their changes. Parallel calls should describe independent work."
	if t.runtimeScheduler {
		description = "Delegate work to a direct child in your shared swarm. Give a complete brief with the goal, constraints, existing authorization, validation, and expected result; private conversations are not shared. Set read_only:true for research, summaries, and reviews that do not require file edits; instructions such as 'do not edit' do not change the execution mode. Inside Git, both editing and read-only children receive isolated worktrees seeded by the runtime from the source checkout's current files. Use repository-relative paths in briefs; source selects snapshot input, not the child's working directory. Do not instruct children to cd or git -C to your checkout. They run Git inspection commands in their assigned worktrees. For history reviews, include the source commit ID in the brief; the child's HEAD is a parentless snapshot. Children discover teammates, exchange addressed messages, and publish findings. They cannot spawn children or write repository Git metadata. You own task creation, acceptance, and integration: review results and apply accepted editing changes before reporting completion. Prefer background execution followed by swarm_wait. A blocking call may return yielded when coordination needs your response; read and answer addressed requests before waiting again. Outside Git, read_only uses live files. If runtime Git setup is denied, report the error; do not copy or repoint Git metadata, modify ignore rules, or disable sandboxing to work around it."
	}
	description += " Children inherit the host's model-call limit. Do not impose guessed iteration caps in the brief. If a limit is reached, preserve the assignment and findings for explicit continuation."
	return schema.Tool(ToolName, description,
		schema.Params{
			"source":     schema.S("Absolute checkout root used to seed the child's isolated snapshot, not its working directory. Omit to seed from the parent checkout. Must belong to the parent's Git repository; not a file or subdirectory."),
			"read_only":  schema.Bool("Set true for research, summaries, or reviews without file edits. Inside Git still uses an isolated runtime snapshot; outside Git observes live files. Defaults to false."),
			"session":    schema.S("Existing swarm member ID to continue"),
			"task_id":    schema.S("Existing task to assign"),
			"task":       schema.S("The complete brief for the agent. It starts with no other context."),
			"label":      schema.S("Two to five words naming the job, shown to the user while it runs."),
			"tools":      schema.Strings("Names or globs of permitted tools. Omitted: inherit compatible parent tools except spawn_agent. Explicit []: disable all model tools, including private built-ins. Nonempty selections retain private transcript/artifact tools and host coordination tools."),
			"model":      schema.S("Model to run the agent on, as provider/model. Default: your own model."),
			"background": schema.Bool("Return at once and keep working; the agent's reply arrives later as a message. Default false: wait for the reply."),
		},
		"task")
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) (string, error) {
	req, err := parseRequest(tools.Args(args))
	req.CallID = CallID(ctx)
	if err != nil {
		return "", tools.NewToolError(err.Error(), "INVALID_ARGS")
	}
	if err := t.acquire(ctx); err != nil {
		return "", err
	}
	res, err := t.run(ctx, req)
	if t.runtimeScheduler {
		// The runtime owns slots, including releases while a member waits.
	} else if res.Done != nil {
		// The child runs on after this call returns; its slot stays taken
		// until the host reports it settled.
		select {
		case <-res.Done:
			t.release()
		default:
			go func() {
				<-res.Done
				t.release()
			}()
		}
	} else {
		t.release()
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		if llm.IsIterationLimit(err) {
			return res.String(), tools.NewToolError("agent paused at its iteration limit: "+err.Error(), "ITERATION_LIMIT")
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
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if t.runtimeScheduler {
		return nil
	}
	select {
	case t.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (t *Tool) release() {
	if t.runtimeScheduler {
		return
	}
	<-t.slots
}

func parseRequest(args tools.Args) (Request, error) {
	if _, supplied := args["max_iterations"]; supplied {
		return Request{}, errors.New("max_iterations is controlled by the host; omit it to inherit the configured limit")
	}
	req := Request{
		Source: args.String("source"), Session: args.String("session"), TaskID: args.String("task_id"), ReadOnly: args.Bool("read_only"),
		Task:       strings.TrimSpace(args.String("task")),
		Label:      strings.TrimSpace(args.String("label")),
		Model:      strings.TrimSpace(args.String("model")),
		Background: args.Bool("background"),
	}
	if req.Task == "" {
		return Request{}, errors.New("task is required: the complete brief for the agent")
	}
	// Only an explicit array narrows the child's tools; null means omitted,
	// and a bare string is one pattern rather than an empty selection.
	switch raw := args["tools"].(type) {
	case nil:
	case string:
		if pattern := strings.TrimSpace(raw); pattern != "" {
			req.Tools = []string{pattern}
		}
	case []any, []string:
		req.Tools = []string{}
		for _, pattern := range args.StringSlice("tools") {
			if pattern = strings.TrimSpace(pattern); pattern != "" {
				req.Tools = append(req.Tools, pattern)
			}
		}
	default:
		return Request{}, errors.New("tools must be an array of tool names or globs")
	}
	return req, nil
}

// ChildRegistry derives a child's view of the parent's tools: those the
// brief allows, excluding nested spawning and coordination tools bound to
// the parent's identity. The child's built-ins it registers
// later stay visible regardless (see tools.ToolRegistry.Derive).
func ChildRegistry(parent *tools.ToolRegistry, allow []string) *tools.ToolRegistry {
	opts := []tools.DeriveOption{tools.DenyTools(ToolName, "swarm_*", "workflow_*", "list_agents", "send_message", "read_messages")}
	if allow != nil && len(allow) == 0 {
		opts = append(opts, tools.DenyTools("*"))
	}
	if len(allow) > 0 {
		opts = append(opts, tools.AllowTools(allow...))
	}
	return parent.Derive(opts...)
}

// ErrNoMatchingTools reports a brief whose tool list matched none of the
// parent's tools.
var ErrNoMatchingTools = errors.New("no tools match the requested list")

// CheckChildTools reports ErrNoMatchingTools for a brief whose tool list
// names nothing the child will have. The agent built-ins count: NewAgent
// registers them on every child, whatever the list allows of the parent's
// tools.
func CheckChildTools(patterns []string, registry *tools.ToolRegistry) error {
	if len(patterns) == 0 || len(registry.All()) > 0 {
		return nil
	}
	for _, pattern := range patterns {
		for _, name := range llm.BuiltinToolNames() {
			if tools.MatchesToolPattern(pattern, name) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s", ErrNoMatchingTools, strings.Join(patterns, ", "))
}

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
		if err := CheckChildTools(req.Tools, registry); err != nil {
			return Result{}, err
		}
		agentConfig := config
		agentConfig.DisableTools = agentConfig.DisableTools || req.Tools != nil && len(req.Tools) == 0
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
