package swarm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// scaleMark names an agent in its task and tags the mail the agents pass
// around, so the scripted model can tell from a request alone who is asking
// and how much mail it has been shown.
const (
	scaleAgentMark = "scale-agent-"
	scaleMailMark  = "scale-ping"
)

// scaleRing is the scripted model for BenchmarkSwarmAgents. The agents form
// a ring: on each round an agent mails the next one and parks until the
// previous one's mail arrives, so every round is a model call, a mail record,
// a park, a wake and a delivery for every agent. After rounds rounds it
// answers.
type scaleRing struct {
	rounds  int
	latency time.Duration
	// started closes once every agent is spawned, so the measured run begins
	// with all of them live and each knows its neighbour's ID.
	started chan struct{}
	members []string
	calls   atomic.Int64
}

func (m *scaleRing) ChatCompletionStream(ctx context.Context, req *llm.CompletionRequest, p llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage, 1)
	ch <- m.reply(ctx, req)
	close(ch)
	return p.ProcessMessagesToEvents(ctx, ch)
}

func (m *scaleRing) reply(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
	select {
	case <-m.started:
	case <-ctx.Done():
		return answer("canceled")
	}
	m.calls.Add(1)
	if m.latency > 0 {
		select {
		case <-time.After(m.latency):
		case <-ctx.Done():
			return answer("canceled")
		}
	}
	agent, sent, seen := -1, 0, 0
	for _, message := range req.Messages {
		if message.Role == messages.MessageRoleAssistant {
			if len(message.ToolCalls) > 0 {
				sent++
			}
			continue
		}
		if agent < 0 {
			if _, after, ok := strings.Cut(message.Content, scaleAgentMark); ok {
				end := 0
				for end < len(after) && after[end] >= '0' && after[end] <= '9' {
					end++
				}
				agent, _ = strconv.Atoi(after[:end])
			}
		}
		if message.Role != messages.MessageRoleTool {
			seen += strings.Count(message.Content, scaleMailMark)
		}
	}
	if agent < 0 || sent >= m.rounds {
		return answer("done")
	}
	next := m.members[(agent+1)%len(m.members)]
	calls := []messages.ChatMessageToolCall{{ID: fmt.Sprintf("send-%d", sent), Name: "send_message", Arguments: tools.Result(map[string]any{"target": next, "message": scaleMailMark})}}
	// Mail that already arrived needs no wait: the agent parks only for the
	// round's mail it has not been shown.
	if seen <= sent {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("wait-%d", sent), Name: "wait_agent", Arguments: `{}`})
	}
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: calls}
}

// BenchmarkSwarmAgents runs whole swarms against a scripted model: every
// agent holds a live turn at once, and each round costs every agent a model
// call, a mail record, a park, a wake and a delivery. One iteration is one
// swarm run to completion; run it with -benchtime=1x.
//
// SCALE_LATENCY sets the scripted model's delay per call (default 0, which
// leaves the runtime as the only cost), SCALE_ROUNDS the rounds per agent and
// SCALE_LIMIT how long one swarm may take before the run fails (default 2m).
func BenchmarkSwarmAgents(b *testing.B) {
	rounds := 3
	if v, err := strconv.Atoi(os.Getenv("SCALE_ROUNDS")); err == nil && v > 0 {
		rounds = v
	}
	latency, _ := time.ParseDuration(os.Getenv("SCALE_LATENCY"))
	// Measure what a running process pays, not the tests' check of every read.
	failOnSharedChange = false
	b.Cleanup(func() { failOnSharedChange = true })
	for _, store := range []sessions.StoreMode{sessions.ModeMemory, sessions.ModeDisk} {
		for _, agents := range []int{100, 300, 1000, 5000} {
			name := "memory"
			if store == sessions.ModeDisk {
				name = "disk"
			}
			b.Run(fmt.Sprintf("store=%s/agents=%d", name, agents), func(b *testing.B) {
				for b.Loop() {
					runScaleRing(b, store, agents, rounds, latency)
				}
			})
		}
	}
}

func runScaleRing(b *testing.B, mode sessions.StoreMode, agents, rounds int, latency time.Duration) {
	b.StopTimer()
	// A run that falls this far behind is reported, with how far it got,
	// instead of being waited for.
	limit := 2 * time.Minute
	if v, err := time.ParseDuration(os.Getenv("SCALE_LIMIT")); err == nil && v > 0 {
		limit = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	config := sessions.StoreConfig{Mode: mode}
	if mode == sessions.ModeDisk {
		config.Path = filepath.Join(b.TempDir(), "polly.db")
	}
	store, err := sessions.OpenStore(config)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{})
	if err != nil {
		b.Fatal(err)
	}
	defer parent.Close()
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	defer registry.Close()
	model := &scaleRing{rounds: rounds, latency: latency, started: make(chan struct{}), members: make([]string, agents)}
	r, err := New(Config{Store: store, Parent: parent, Registry: registry, OpenTools: tools.NativeOpenTools(registry), OpenWorktrees: testOpenWorktrees(registry), Client: model, Root: b.TempDir(), Directory: filepath.Join(b.TempDir(), "runtime"), MaxConcurrent: agents, MaxExecutions: agents, MaxWorktrees: agents, Agent: llm.AgentConfig{MaxIterations: rounds + 2}})
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()

	spawning := time.Now()
	done := make([]<-chan struct{}, agents)
	for i := range agents {
		result, err := r.Spawn(ctx, subagent.Request{Label: fmt.Sprintf("Agent %d", i), Task: scaleAgentMark + strconv.Itoa(i), ReadOnly: true, Background: true})
		if err != nil {
			b.Fatalf("spawn %d of %d after %s: %v", i, agents, time.Since(spawning).Round(time.Millisecond), err)
		}
		model.members[i], done[i] = result.Session, result.Done
	}
	spawned := time.Since(spawning)

	b.StartTimer()
	running := time.Now()
	close(model.started)
	for i, ch := range done {
		select {
		case <-ch:
		case <-ctx.Done():
			b.Fatalf("stopped at the %s limit: spawning took %s, then %d of %d agents finished and the model answered %d of %d calls in %s",
				limit, spawned.Round(time.Millisecond), i, agents, model.calls.Load(), agents*(rounds+1), time.Since(running).Round(time.Millisecond))
		}
	}
	ran := time.Since(running)
	b.StopTimer()

	s, err := r.State(ctx)
	if err != nil {
		b.Fatal(err)
	}
	failed, why := 0, ""
	for _, e := range s.Executions {
		if e.Status != "completed" {
			failed++
			why = e.Status + ": " + e.Error
		}
	}
	if failed > 0 {
		b.Fatalf("%d of %d executions did not complete, one of them %s", failed, len(s.Executions), why)
	}
	if len(s.Messages) < agents*rounds {
		b.Fatalf("%d mail records for %d agents and %d rounds", len(s.Messages), agents, rounds)
	}
	calls := float64(model.calls.Load())
	b.ReportMetric(float64(spawned.Microseconds())/float64(agents), "spawn-µs/agent")
	b.ReportMetric(calls/ran.Seconds(), "model-calls/s")
	b.ReportMetric(float64((ran-time.Duration(rounds+1)*latency).Microseconds())/calls, "overhead-µs/call")
	b.StartTimer()
}
