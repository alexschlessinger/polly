package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

func registerSwarm(state *conversationState, config *Config, client llm.LLM) error {
	// A child opened for human inspection must not acquire parent authority.
	meta, err := state.session.GetMetadata(state.session.Context())
	if err != nil {
		return err
	}
	if meta.Parent != "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".pollytool", "polly.db")
	systemPrompt := state.settings.SystemPrompt
	c := swarm.Config{Store: state.sessionStore, Parent: state.session, Registry: state.toolRegistry, Client: client,
		Request:   *createCompletionRequest(config, &state.settings, nil, state.toolRegistry, nil, nil),
		Agent:     llm.AgentConfig{MaxIterations: state.settings.MaxIterations, ToolTimeout: state.settings.ToolTimeout},
		Directory: config.SwarmDirectory, MaxConcurrent: config.SwarmConcurrent, MaxExecutions: config.SwarmExecutions,
		PrivatePaths:    []string{path, path + "-wal", path + "-shm"},
		DurableMessages: durableTurnMessages,
		Instructions: func(registry *tools.ToolRegistry) string {
			instructions, _ := loadRepositoryInstructions(registry)
			return systemPrompt + "\n\n" + codingContract + "\n\n" + instructions
		},
		Callbacks: func(ctx context.Context, m swarm.Member) *llm.AgentCallbacks {
			ui := parentTurnUIFrom(ctx)
			if ui == nil {
				return nil
			}
			child := &childTurnUI{parent: ui}
			if host, ok := ui.(lineChildActivityHost); ok {
				child.activity = host.childActivity(toolCallFrom(ctx))
			}
			return &llm.AgentCallbacks{OnReasoning: child.ShowThinking, OnContent: child.AppendAssistantText, OnToolStart: child.AppendToolStart, OnToolEnd: child.AppendToolEnd, ApproveToolCalls: ui.ApproveToolCalls, BeforeToolExecute: func(ctx context.Context, call messages.ChatMessageToolCall, _ map[string]any) context.Context {
				return withToolCall(withParentTurnUI(ctx, ui), call)
			}}
		},
	}
	if durable, ok := state.sessionStore.(sessions.DurableStore); ok {
		c.Promote = func(ctx context.Context) error { return durable.Promote(ctx, path) }
	}
	runtime, err := swarm.New(c)
	if err != nil {
		return err
	}
	state.swarm = runtime
	runtime.RegisterParentTools(state.toolRegistry)
	return nil
}

func registerSwarmCommands(r *replCommandRegistry) {
	r.register(replCommand{name: "/swarm", usage: "/swarm [members|tasks|messages|publications|workflows|previews|raw|stop ID|resume ID [ADDITIONAL_ITERATIONS]|grant N|cleanup CONTEXT_ID|cleanup all|cancel-workflow ID|acknowledge-workflow ID]", summary: "inspect and control this parent's shared swarm", busySafe: true, run: func(ctx *replCommandContext, args []string) replCommandResult {
		if ctx.state == nil || ctx.state.swarm == nil {
			return replCommandResult{err: ctx.replyLine("no parent swarm runtime is attached")}
		}
		runtime := ctx.state.swarm
		opCtx := ctx.operationContext()
		if len(args) > 2 {
			var err error
			switch args[1] {
			case "stop":
				err = runtime.StopMember(opCtx, args[2])
			case "cleanup":
				id := args[2]
				if id == "all" {
					id = ""
				}
				err = runtime.Cleanup(opCtx, id)
			case "cancel-workflow":
				err = runtime.CancelWorkflow(args[2])
			case "acknowledge-workflow":
				err = runtime.AcknowledgeWorkflow(opCtx, args[2])
			case "resume":
				switch len(args) {
				case 3:
					err = runtime.Resume(opCtx, args[2], 0)
				case 4:
					n, e := strconv.Atoi(args[3])
					if e != nil || n <= 0 {
						err = fmt.Errorf("resume requires a positive count of additional model calls")
					} else {
						err = runtime.ResumeWithIterations(opCtx, args[2], n)
					}
				default:
					err = fmt.Errorf("usage: /swarm resume ID [ADDITIONAL_ITERATIONS]")
				}
			case "grant":
				n, e := strconv.Atoi(args[2])
				if e != nil || n <= 0 {
					err = fmt.Errorf("grant requires a positive execution count")
				} else {
					err = runtime.Resume(opCtx, "", n)
				}
			default:
				err = fmt.Errorf("unknown swarm control")
			}
			if err != nil {
				return replCommandResult{err: err}
			}
			return replCommandResult{err: ctx.replyLine("swarm control recorded")}
		}
		if ctx.openSwarm != nil {
			section := ""
			if len(args) == 2 {
				section = args[1]
			}
			ctx.openSwarm(section)
			return replCommandResult{}
		}
		state, err := runtime.State(opCtx)
		if err != nil {
			return replCommandResult{err: err}
		}
		var value any = state
		if len(args) == 2 {
			switch args[1] {
			case "members":
				value = state.Members
			case "tasks":
				value = state.Tasks
			case "messages":
				value = state.Messages
			case "publications":
				value = state.Publications
			case "workflows":
				value = state.Workflows
			case "previews":
				value = state.Previews
			case "raw":
				value = state
			default:
				return replCommandResult{err: fmt.Errorf("unknown swarm view")}
			}
		}
		return replCommandResult{err: ctx.replyLine(tools.Result(value))}
	}})
	r.register(replCommand{name: "/workflow", usage: "/workflow <script.js> <input.json>", summary: "run a saved JavaScript workflow using the shared swarm runtime", run: func(ctx *replCommandContext, args []string) replCommandResult {
		if len(args) != 3 || ctx.state == nil || ctx.state.swarm == nil {
			return replCommandResult{err: fmt.Errorf("usage: /workflow <script.js> <input.json>")}
		}
		// A slash command is explicit user input. Normal model calls use the
		// registry's filesystem tools and submit source to workflow_run.
		source, err := os.ReadFile(args[1])
		if err != nil {
			return replCommandResult{err: err}
		}
		input, err := os.ReadFile(args[2])
		if err != nil {
			return replCommandResult{err: err}
		}
		value, err := schema.DecodeJSON(string(input))
		if err != nil {
			return replCommandResult{err: err}
		}
		id, err := ctx.state.swarm.StartWorkflow(ctx.operationContext(), string(source), value)
		if err != nil {
			return replCommandResult{err: err}
		}
		return replCommandResult{err: ctx.replyLine("Workflow started: " + id + ". Inspect it with /swarm workflows.")}
	}})
}

func swarmInspectorText(s *swarm.State, section string) string {
	var b strings.Builder
	jsonText := func(value any) string {
		data, _ := json.MarshalIndent(value, "", "  ")
		return string(data)
	}
	switch section {
	case "raw":
		return jsonText(s)
	case "tasks":
		for _, id := range swarmRecordIDs(s.Tasks) {
			task := s.Tasks[id]
			fmt.Fprintf(&b, "%s · revision %d\n%s\n%s\nOwner: %s\nAcceptance: %s\n", task.Status, task.Revision, id, task.Description, task.Owner, task.Criteria)
			if len(task.Dependencies) > 0 {
				fmt.Fprintf(&b, "Dependencies: %s\n", strings.Join(task.Dependencies, ", "))
			}
			if task.Feedback != "" {
				fmt.Fprintf(&b, "Feedback: %s\n", task.Feedback)
			}
			if task.Snapshot != "" {
				fmt.Fprintf(&b, "Snapshot: %s\n", task.Snapshot)
			}
			if task.Result != nil {
				fmt.Fprintf(&b, "Result: %s\n", jsonText(task.Result))
			}
			b.WriteByte('\n')
		}
	case "messages":
		for _, id := range swarmRecordIDs(s.Messages) {
			mail := s.Messages[id]
			status := "pending delivery"
			if mail.Delivered {
				status = "delivered"
			}
			fmt.Fprintf(&b, "%s · %s\n%s → %s\n%s\n%s\n", mail.Kind, status, mail.From, mail.To, id, mail.Text)
			if mail.ReplyTo != "" {
				fmt.Fprintf(&b, "Reply to: %s\n", mail.ReplyTo)
			}
			b.WriteByte('\n')
		}
	case "publications":
		for _, id := range swarmRecordIDs(s.Publications) {
			pub := s.Publications[id]
			fmt.Fprintf(&b, "%s\nPublished by %s\n%s\n", id, pub.Author, pub.Text)
			if pub.Supersedes != "" {
				fmt.Fprintf(&b, "Corrects: %s\n", pub.Supersedes)
			}
			if len(pub.Sources) > 0 {
				fmt.Fprintf(&b, "Sources: %s\n", strings.Join(pub.Sources, ", "))
			}
			if pub.Snapshot != "" {
				fmt.Fprintf(&b, "Snapshot: %s\n", pub.Snapshot)
			}
			for _, artifact := range pub.Artifacts {
				fmt.Fprintf(&b, "Artifact: %s\n", artifact.ID)
			}
			b.WriteByte('\n')
		}
	case "workflows":
		for _, id := range swarmRecordIDs(s.Workflows) {
			w := s.Workflows[id]
			fmt.Fprintf(&b, "%s · %s\n%s\n%d saved steps\n", w.Name, w.Status, id, len(w.Steps))
			if w.Error != nil {
				fmt.Fprintf(&b, "Error (%s): %s\n", w.Error.Code, w.Error.Message)
			}
			if w.Acknowledged {
				b.WriteString("Failure acknowledged by parent\n")
			}
			if w.Output != nil {
				fmt.Fprintf(&b, "Result:\n%s\n", jsonText(w.Output))
			}
			for index, step := range w.Steps {
				fmt.Fprintf(&b, "  %d. %s · %s\n", index+1, step.Kind, step.Status)
				if step.Error != nil {
					fmt.Fprintf(&b, "     %s\n", step.Error.Message)
				}
			}
			b.WriteString("Source, inputs and full step values are available in raw.\n\n")
		}
	case "previews":
		for _, id := range swarmRecordIDs(s.Previews) {
			preview := s.Previews[id]
			status := "ready for accepted task integration"
			if preview.Conflicts != "" {
				status = "conflicts require review"
			}
			fmt.Fprintf(&b, "%s\n%s\nCandidate: %s\nDirectory: %s\n", status, id, preview.Candidate.ID, preview.Checkout.Path)
			if preview.Conflicts != "" {
				fmt.Fprintf(&b, "%s\n", preview.Conflicts)
			}
			b.WriteByte('\n')
		}
	case "members", "":
	default:
		return "Unknown swarm section."
	}
	if section != "members" && section != "" {
		if b.Len() == 0 {
			return "No " + section + " yet."
		}
		return b.String()
	}
	fmt.Fprintf(&b, "%d members · %d tasks · %d publications · %d workflows\n\n", len(s.Members), len(s.Tasks), len(s.Publications), len(s.Workflows))
	for _, run := range s.Runs {
		fmt.Fprintf(&b, "Run %s\n%s · %d / %d logical executions\n\n", run.ID, run.Status, run.Starts, run.Limit)
	}
	ids := make([]string, 0, len(s.Members))
	for id := range s.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := s.Members[id]
		label := m.Label
		if label == "" {
			label = m.Name
		}
		status, _ := swarmMemberActivity(s, m)
		fmt.Fprintf(&b, "%s — %s\n%s\nTask: %s\n", label, status, m.ID, m.Task)
		if e := s.Executions[m.Execution]; e != nil {
			fmt.Fprintf(&b, "Model calls: %d / %d\n", e.Iterations, e.Request.MaxIterations)
			if e.Error != "" {
				fmt.Fprintf(&b, "Reason: %s\n", e.Error)
			}
		}
		if c := s.Contexts[m.Context]; c != nil {
			fmt.Fprintf(&b, "Directory: %s\n", c.Root)
		}
		if m.Controller != "" {
			fmt.Fprintf(&b, "Workflow reservation: %s\n", m.Controller)
		}
		b.WriteByte('\n')
	}
	if len(ids) == 0 {
		b.WriteString("No members yet. Agents join when you delegate work.\n")
	}
	b.WriteString("Use Agents to inspect private conversations. /swarm stop ID pauses a member; /swarm resume ID continues it. If its iteration allowance is exhausted, /swarm resume ID N grants N additional model calls.\n")
	return b.String()
}

func swarmRecordIDs[T any](records map[string]T) []string {
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
