package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

// The budget stress test plays randomized tool loops against the agent and
// checks, on every request the model receives and on every run's result, the
// guarantees a run under MaxContextTokens makes: once the first request fits,
// every request fits, and once the provider has reported a request's count,
// every later one fits in the provider's count too; tool calls and results
// stay paired; a request that offers no tools carries none; a request the
// provider rejects as too long is followed by a smaller one, at most twice,
// and never rejected again when the rejection stated its counts; and the run
// ends in an answer or an error it is allowed to end in, never for want of
// room.
//
// The seed corpus runs with every go test. To search further:
//
//	go test -run '^$' -fuzz FuzzAgentBudget -fuzztime 5m ./llm
//
// A failing seed is saved under testdata/fuzz and replays with go test.

const stressSeeds = 200

func FuzzAgentBudget(f *testing.F) {
	for seed := range uint64(stressSeeds) {
		f.Add(seed)
	}
	catalog := stressSkills(f)
	f.Fuzz(func(t *testing.T, seed uint64) {
		runStressScenario(t, seed, catalog)
	})
}

// stressSkills is a catalog of skills whose instructions run from a line to
// tens of kilobytes.
func stressSkills(f *testing.F) *skills.Catalog {
	root := f.TempDir()
	for name, body := range map[string]int{"brief": 200, "manual": 6_000, "tome": 40_000} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			f.Fatal(err)
		}
		content := "---\nname: " + name + "\ndescription: a " + name + " skill\n---\n" + strings.Repeat("Follow the steps. ", body/18)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			f.Fatal(err)
		}
	}
	catalog, err := skills.LoadCatalog([]string{root})
	if err != nil || catalog == nil {
		f.Fatalf("load stress skills: %v", err)
	}
	return catalog
}

// stressScenario is one randomized run: its budget, configuration, starting
// history and the model's temperament.
type stressScenario struct {
	seed            uint64
	budget          int
	maxIterations   int
	store           bool
	responseTool    bool
	requireResponse bool
	admit           bool
	continueFinal   bool
	deny            bool
	// openRouter routes the model through OpenRouter, whose reasoning
	// replay the projection prices its own way.
	openRouter bool
	// noTools and noImages model a model without tool calling or image
	// input; window gives it a context window of its own.
	noTools, noImages bool
	window            int
	// skills offers a skill catalog; grow offers a tool that adds tools
	// while the run is under way, as activating a skill can.
	skills, grow bool
	// checkpoints persists the run as it goes, as a swarm member does.
	checkpoints bool
	// trueRatio is the provider's count of a request per estimated token;
	// reportUsage has it report that count with every reply.
	trueRatio   float64
	reportUsage bool
	// providerWindow is the provider's own context window, in its count,
	// rejections word what it refuses, and maxTokens is the output asked for.
	providerWindow int
	rejections     overflowWording
	maxTokens      int
	prompt         int
	history        []messages.ChatMessage

	finalChance      float64
	maxCalls         int
	maxSize          int
	misbehaveChance  float64
	reasoningChance  float64
	truncatedChance  float64
	invalidArgChance float64
}

func (s stressScenario) String() string {
	return fmt.Sprintf("seed=%d budget=%d maxIterations=%d store=%v responseTool=%v require=%v admit=%v continue=%v deny=%v openRouter=%v noTools=%v noImages=%v window=%d skills=%v grow=%v checkpoints=%v trueRatio=%.2f reportUsage=%v providerWindow=%d rejections=%s maxTokens=%d prompt=%d history=%d maxCalls=%d maxSize=%d",
		s.seed, s.budget, s.maxIterations, s.store, s.responseTool, s.requireResponse, s.admit, s.continueFinal, s.deny, s.openRouter, s.noTools, s.noImages, s.window, s.skills, s.grow, s.checkpoints, s.trueRatio, s.reportUsage, s.providerWindow, s.rejections, s.maxTokens, s.prompt, len(s.history), s.maxCalls, s.maxSize)
}

// overflowWording is how a provider words a rejection as too long.
type overflowWording int

const (
	deepSeekWording overflowWording = iota
	openRouterWording
	anthropicWording
	openAIWording
	geminiWording
)

func (w overflowWording) String() string {
	return [...]string{"deepseek", "openrouter", "anthropic", "openai", "gemini"}[w]
}

// counted reports whether the wording states the input the provider counted.
func (w overflowWording) counted() bool { return w <= anthropicWording }

// inputOnly reports whether the provider holds the input alone to the window.
func (w overflowWording) inputOnly() bool { return w == anthropicWording || w == geminiWording }

// reject words a rejection of input and output tokens against window.
func (w overflowWording) reject(window, input, tools, output int) error {
	switch w {
	case deepSeekWording:
		return fmt.Errorf("openai api error 400 (invalid_request_error): This model's maximum context length is %d tokens. However, you requested %d tokens (%d in the messages, %d in the completion). Please reduce the length of the messages or completion.", window, input+output, input, output)
	case openRouterWording:
		return fmt.Errorf("openai api error 400 (context_length_exceeded): This endpoint's maximum context length is %d tokens. However, you requested about %d tokens (%d of text input, %d of tool input, %d in the output). Please reduce the length of either one, or use the context-compression plugin to compress your prompt automatically.", window, input+output, input-tools, tools, output)
	case anthropicWording:
		return fmt.Errorf("anthropic api error 400 (invalid_request_error): prompt is too long: %d tokens > %d maximum", input, window)
	case openAIWording:
		return errors.New("openai api error 400 (context_length_exceeded): Your input exceeds the context window of this model. Please adjust your input and try again.")
	}
	return fmt.Errorf("gemini api error 400 (INVALID_ARGUMENT): The input token count exceeds the maximum number of tokens allowed %d.", window)
}

// logSize is a size spread evenly over orders of magnitude up to limit.
func logSize(rng *rand.Rand, limit int) int {
	if limit <= 1 {
		return limit
	}
	return int(math.Exp(rng.Float64() * math.Log(float64(limit))))
}

func words(rng *rand.Rand, bytes int) string {
	vocabulary := []string{"alpha", "beta", "gamma", "delta", "context", "budget", "page", "the", "a", "of", "résumé", "日本語", "{\"k\":1}", "\n"}
	var b strings.Builder
	for b.Len() < bytes {
		b.WriteString(vocabulary[rng.IntN(len(vocabulary))])
		b.WriteByte(' ')
	}
	return b.String()
}

func newStressScenario(seed uint64) stressScenario {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	s := stressScenario{
		seed:             seed,
		budget:           1_500 + logSize(rng, 40_000),
		maxIterations:    1 + rng.IntN(14),
		store:            rng.Float64() < 0.8,
		responseTool:     rng.Float64() < 0.25,
		admit:            rng.Float64() < 0.3,
		continueFinal:    rng.Float64() < 0.3,
		deny:             rng.Float64() < 0.2,
		prompt:           logSize(rng, 6_000),
		finalChance:      0.05 + rng.Float64()*0.3,
		maxCalls:         1 + logSize(rng, 60),
		maxSize:          100 + logSize(rng, 80_000),
		misbehaveChance:  rng.Float64() * 0.3,
		reasoningChance:  rng.Float64() * 0.6,
		truncatedChance:  rng.Float64() * 0.05,
		invalidArgChance: rng.Float64() * 0.1,
	}
	s.requireResponse = s.responseTool && rng.Float64() < 0.5
	s.openRouter = rng.Float64() < 0.2
	// A model that cannot call tools cannot end in a response tool.
	s.noTools = !s.responseTool && rng.Float64() < 0.1
	s.noImages = rng.Float64() < 0.15
	if rng.Float64() < 0.2 {
		s.window = s.budget/2 + rng.IntN(s.budget)
	}
	s.skills = rng.Float64() < 0.2
	s.grow = rng.Float64() < 0.2
	s.checkpoints = rng.Float64() < 0.4
	s.trueRatio = 0.5 + 2*rng.Float64()
	s.reportUsage = rng.Float64() < 0.9
	if rng.Float64() < 0.3 {
		s.providerWindow = s.budget/3 + rng.IntN(s.budget*2)
		s.rejections = overflowWording(rng.IntN(5))
	}
	if rng.Float64() < 0.4 {
		s.maxTokens = logSize(rng, s.budget)
	}

	if s.prompt > 0 {
		s.history = append(s.history, messages.ChatMessage{Role: messages.MessageRoleSystem, Content: words(rng, s.prompt)})
	}
	for exchange := range rng.IntN(6) {
		s.history = append(s.history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: words(rng, logSize(rng, 20_000))})
		if rng.Float64() < 0.5 {
			id := fmt.Sprintf("old%d", exchange)
			call := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: words(rng, logSize(rng, 500)),
				ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "fetch", Arguments: `{"size":10}`}}}
			if rng.Float64() < 0.5 {
				call.Metadata = map[string]any{anthropic.ThinkingBlocksKey: []any{map[string]any{"thinking": words(rng, logSize(rng, 3_000)), "signature": strings.Repeat("s", 200)}}}
			}
			s.history = append(s.history, call, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: "fetch", Content: words(rng, logSize(rng, 40_000))})
		}
		s.history = append(s.history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: words(rng, logSize(rng, 4_000))})
	}
	s.history = append(s.history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: words(rng, logSize(rng, 2_000))})
	return s
}

// sizedTool returns as many bytes as its call's size argument asks for, and
// an image when it is the shot tool.
type sizedTool struct {
	tools.NativeTool
	name  string
	image []byte
}

func (t *sizedTool) GetName() string { return t.name }
func (t *sizedTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(t.name, "returns output of the size asked for", schema.Params{"size": schema.Int("bytes of output")})
}
func (t *sizedTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	output, err := t.ExecuteOutput(ctx, args)
	return output.Text, err
}
func (t *sizedTool) ExecuteOutput(_ context.Context, args map[string]any) (tools.ToolOutput, error) {
	size := tools.Args(args).Int("size", 100)
	output := tools.ToolOutput{Text: strings.Repeat("output of "+t.name+" ", max(1, size/16))}
	if t.image != nil {
		output.Media = []tools.ToolMedia{{Data: t.image, MIMEType: "image/png", Name: "shot.png"}}
	}
	return output, nil
}

// sizedRecallTool is a recall tool that does not page what it returns.
type sizedRecallTool struct{ sizedTool }

func (t *sizedRecallTool) RecallStub() string {
	return "[remember result elided; call remember again to re-read.]"
}

var stressArtifactID = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// chaosLLM answers with randomized replies and checks every request it gets.
type chaosLLM struct {
	s   stressScenario
	rng *rand.Rand

	mu         sync.Mutex
	calls      int
	violations []string
	// trace records each request and reply, for a failure's report.
	trace []string
	// reported is set once a reply has reported the provider's count, and
	// rejected is the estimate and output reserve of each request rejected
	// since the last one accepted.
	reported bool
	rejected [][2]int
	// firstOver is set when the provider counted the first request over the
	// budget it was sent under.
	firstOver bool
}

func (l *chaosLLM) violate(format string, args ...any) {
	l.violations = append(l.violations, fmt.Sprintf("request %d: ", l.calls)+fmt.Sprintf(format, args...))
}

// requestTokens estimates req as the projection does, OpenRouter's replay
// pricing included.
func requestTokens(req *CompletionRequest) int {
	cache := &projectionCache{openRouter: req.IsOpenRouter(), replayModel: targetForRequest(req).Model, replayEndpoint: openrouter.Endpoint(req.BaseURL)}
	tokens := estimateToolSchemaTokens(req.Tools)
	for _, msg := range req.Messages {
		tokens += cache.estimate(msg)
	}
	return tokens
}

// check holds the request to what the agent guarantees a provider.
func (l *chaosLLM) check(req *CompletionRequest) {
	tokens := requestTokens(req)
	if req.MaxContextTokens > 0 && tokens > req.MaxContextTokens {
		l.violate("estimated %d tokens over the %d-token budget", tokens, req.MaxContextTokens)
	}
	// Once the provider has reported a request's count, the calibration keeps
	// every later request within the budget in the provider's count too.
	if counted := int(float64(tokens) * l.s.trueRatio); l.reported && l.s.window == 0 && counted > l.s.budget+l.s.budget/100+1 {
		l.violate("provider counts %d (estimate %d x %.2f) over the %d-token budget", counted, tokens, l.s.trueRatio, l.s.budget)
	}
	if n := len(l.rejected); n > 0 {
		last := l.rejected[n-1]
		if tokens >= last[0] && reserveOf(req, last[1]) >= last[1] {
			l.violate("estimated %d tokens reserving %d after a request of %d reserving %d was rejected", tokens, req.MaxTokens, last[0], last[1])
		}
		if n > maxOverflowRetries {
			l.violate("sent after %d rejections in a row", n)
		}
	}
	for _, violation := range transcriptViolations(req.Messages) {
		l.violate("%s", violation)
	}
	calls := 0
	for _, msg := range req.Messages {
		calls += len(msg.ToolCalls)
	}
	if calls > 0 && len(req.Tools) == 0 {
		l.violate("%d tool calls in a request that offers no tools", calls)
	}
}

// reserveOf is the output req reserves, the provider's default when it asks
// for none.
func reserveOf(req *CompletionRequest, fallback int) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return fallback
}

func (l *chaosLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	l.mu.Lock()
	l.check(req)
	tokens := requestTokens(req)
	counted := int(float64(tokens) * l.s.trueRatio)
	// The provider's default reserve, when the request asks for none, is a
	// thousand tokens.
	reserve := reserveOf(req, 1_000)
	if l.calls == 0 {
		l.firstOver = req.MaxContextTokens > 0 && counted > req.MaxContextTokens
	}
	charged := counted + reserve
	if l.s.rejections.inputOnly() {
		charged = counted
	}
	if l.s.providerWindow > 0 && charged > l.s.providerWindow {
		if n := len(l.rejected); n > 0 && l.s.rejections.counted() {
			l.violate("rejected again after a rejection that stated its counts: %d tokens reserving %d against %d", counted, reserve, l.s.providerWindow)
		}
		l.rejected = append(l.rejected, [2]int{tokens, reserve})
		l.trace = append(l.trace, fmt.Sprintf("request %d: %d messages, %d tools, ~%d/%d tokens, counted %d reserving %d -> rejected",
			l.calls, len(req.Messages), len(req.Tools), tokens, req.MaxContextTokens, counted, reserve))
		l.calls++
		err := l.s.rejections.reject(l.s.providerWindow, counted, int(float64(estimateToolSchemaTokens(req.Tools))*l.s.trueRatio), reserve)
		l.mu.Unlock()
		events := make(chan *messages.StreamEvent, 1)
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: err}
		close(events)
		return events
	}
	l.rejected = nil
	response := l.reply(req)
	if l.s.reportUsage {
		if response.Metadata == nil {
			response.Metadata = map[string]any{}
		}
		response.Metadata[messages.MetadataKeyInputTokens] = counted
		l.reported = true
	}
	l.trace = append(l.trace, fmt.Sprintf("request %d: %d messages, %d tools, ~%d/%d tokens, counted %d -> %s, %d calls, %d bytes",
		l.calls, len(req.Messages), len(req.Tools), tokens, req.MaxContextTokens, counted, response.StopReason, len(response.ToolCalls), len(response.Content)))
	l.calls++
	l.mu.Unlock()
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(ctx, input)
}

func (l *chaosLLM) reply(req *CompletionRequest) messages.ChatMessage {
	rng, s := l.rng, l.s
	offered := map[string]bool{}
	for _, tool := range req.Tools {
		offered[tool.GetName()] = true
	}
	response := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, Content: words(rng, logSize(rng, s.maxSize/20))}
	if rng.Float64() < s.reasoningChance {
		response.Reasoning = words(rng, logSize(rng, 4_000))
		response.Metadata = map[string]any{anthropic.ThinkingBlocksKey: []any{map[string]any{"thinking": response.Reasoning, "signature": strings.Repeat("s", 300)}}}
		if s.openRouter {
			// Attributed to this route, so the projection replays it.
			response.Metadata = map[string]any{openrouter.MetadataKey: map[string]any{
				"endpoint":          openrouter.Endpoint(req.BaseURL),
				"requested_model":   targetForRequest(req).Model,
				"reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": words(rng, logSize(rng, 8_000))}},
			}}
		}
	}
	// Sorted, so a seed replays the same run.
	names := slices.Sorted(maps.Keys(offered))
	finishing := len(req.Tools) == 0 || (len(req.Tools) == 1 && offered["respond"])
	// A model may call tools a request does not offer; the agent must cope.
	if len(names) == 0 && rng.Float64() < s.misbehaveChance {
		names = []string{"fetch", "read_transcript"}
		finishing = false
	}
	if len(names) == 0 || (!finishing && rng.Float64() < s.finalChance) {
		response.StopReason = messages.StopReasonEndTurn
		response.Content = words(rng, logSize(rng, s.maxSize))
		if offered["respond"] && rng.Float64() < 0.6 {
			response.StopReason = messages.StopReasonToolUse
			response.ToolCalls = []messages.ChatMessageToolCall{{ID: fmt.Sprintf("r%d", l.calls), Name: "respond", Arguments: `{"size":50}`}}
		}
		return response
	}
	if finishing && offered["respond"] {
		response.ToolCalls = []messages.ChatMessageToolCall{{ID: fmt.Sprintf("r%d", l.calls), Name: "respond", Arguments: `{"size":50}`}}
		return response
	}
	ids := stressArtifactID.FindAllString(fmt.Sprint(req.Messages), -1)
	for i := range 1 + rng.IntN(s.maxCalls) {
		name := names[rng.IntN(len(names))]
		if rng.Float64() < 0.03 {
			name = "missing"
		}
		var args string
		switch name {
		case BuiltinReadTranscript:
			args = []string{`{}`, fmt.Sprintf(`{"offset":%d}`, 1+rng.IntN(2_000)), `{"query":"alpha"}`, fmt.Sprintf(`{"byte_offset":%d}`, rng.IntN(100_000))}[rng.IntN(4)]
		case BuiltinReadArtifact:
			id := "sha256:" + strings.Repeat("0", 64)
			if len(ids) > 0 {
				id = ids[rng.IntN(len(ids))]
			}
			args = fmt.Sprintf(`{"id":%q}`, id)
		case BuiltinListArtifacts:
			args = `{}`
		case "activate_skill":
			args = fmt.Sprintf(`{"name":%q}`, []string{"brief", "manual", "tome", "absent"}[rng.IntN(4)])
		default:
			args = fmt.Sprintf(`{"size":%d,"pad":%q}`, logSize(rng, s.maxSize), words(rng, logSize(rng, s.maxSize/50)))
		}
		if rng.Float64() < s.invalidArgChance {
			args = args[:len(args)/2]
		}
		response.ToolCalls = append(response.ToolCalls, messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d_%d", l.calls, i), Name: name, Arguments: args})
	}
	if rng.Float64() < s.truncatedChance {
		response.StopReason = messages.StopReasonMaxTokens
	}
	return response
}

func runStressScenario(t *testing.T, seed uint64, catalog *skills.Catalog) {
	s := newStressScenario(seed)
	rng := rand.New(rand.NewPCG(seed^0x5bd1e995, seed))
	image := independentPNG(t)

	registry := registryWith(
		&sizedTool{name: "fetch"},
		&sizedTool{name: "shot", image: image},
		&sizedRecallTool{sizedTool{name: "remember"}},
	)
	config := AgentConfig{MaxIterations: s.maxIterations, Calibration: NewCalibration()}
	if s.store {
		config.ArtifactStore = newTestArtifactStore()
	}
	if s.responseTool {
		registry.Register(&sizedTool{name: "respond"})
		config.ResponseTool = "respond"
		config.RequireResponseToolSuccess = s.requireResponse
	}
	if s.skills {
		// The skill runtime wants a bash to run skill scripts with; a
		// harmless stand-in keeps it from building a real one.
		registry.Register(&sizedTool{name: "bash"})
		if _, err := tools.NewSkillRuntime(catalog, registry); err != nil {
			t.Fatal(err)
		}
	}
	if s.grow {
		registry.Register(&growTool{registry: registry})
	}
	model := &chaosLLM{s: s, rng: rng}
	agent := NewAgent(model, registry, config)
	defer agent.Close()

	// Callbacks run on the agent's goroutines; their randomness is their own.
	cbRNG := rand.New(rand.NewPCG(seed, 7))
	var cbMu sync.Mutex
	chance := func(p float64) bool {
		cbMu.Lock()
		defer cbMu.Unlock()
		return cbRNG.Float64() < p
	}
	input := func() messages.ChatMessage {
		cbMu.Lock()
		defer cbMu.Unlock()
		msg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: words(cbRNG, logSize(cbRNG, 60_000))}
		if cbRNG.Float64() < 0.7 {
			msg.Metadata = map[string]any{messages.MetadataKeyAgentSynthetic: true}
		}
		return msg
	}
	cb := &AgentCallbacks{}
	if s.admit {
		cb.AdmitInput = func(context.Context) ([]messages.ChatMessage, error) {
			if chance(0.3) {
				return []messages.ChatMessage{input()}, nil
			}
			return nil, nil
		}
	}
	continued := 0
	if s.continueFinal {
		cb.ContinueAfterFinal = func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
			if continued < 2 && chance(0.5) {
				continued++
				return []messages.ChatMessage{input()}, nil
			}
			return nil, nil
		}
	}
	if s.deny {
		cb.ApproveToolCalls = func(_ context.Context, calls []messages.ChatMessageToolCall) ([]bool, error) {
			approved := make([]bool, len(calls))
			for i := range approved {
				approved[i] = !chance(0.2)
			}
			return approved, nil
		}
	}

	var checkpoints []AgentCheckpoint
	if s.checkpoints {
		cb.Checkpoint = func(_ context.Context, checkpoint AgentCheckpoint) error {
			cbMu.Lock()
			defer cbMu.Unlock()
			checkpoints = append(checkpoints, AgentCheckpoint{Generated: cloneMessages(checkpoint.Generated), Final: checkpoint.Final, Request: checkpoint.Request})
			return nil
		}
		cb.JournalToolBatch = func(context.Context, AgentCheckpoint) error { return nil }
		cb.BeforeToolBatch = func(context.Context, []messages.ChatMessageToolCall) error { return nil }
	}

	req := &CompletionRequest{Messages: cloneMessages(s.history), MaxContextTokens: s.budget, MaxTokens: s.maxTokens}
	if s.openRouter {
		req.Model = "openrouter/vendor/model"
	}
	if s.noTools || s.noImages || s.window > 0 {
		req.Capabilities = &ModelCapabilities{}
		if s.noTools {
			no := false
			req.Capabilities.Tools = &no
		}
		if s.noImages {
			req.Capabilities.InputModalities = []string{"text"}
		}
		if s.window > 0 {
			req.Capabilities.ContextTokens = &s.window
		}
	}
	if s.skills {
		req.Skills = catalog
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	response, err := agent.Run(ctx, req, cb)
	elapsed := time.Since(started)

	model.mu.Lock()
	violations, calls, trace, firstOver := model.violations, model.calls, model.trace, model.firstOver
	model.mu.Unlock()
	if !stressErrorAllowed(err, calls, firstOver, s) {
		violations = append(violations, fmt.Sprintf("run ended with %v after %d model calls", err, calls))
	}
	if response != nil {
		violations = append(violations, transcriptViolations(append(cloneMessages(s.history), response.AllMessages...))...)
		var roles []string
		for _, msg := range response.AllMessages {
			roles = append(roles, fmt.Sprintf("%s(%d calls,%s)", msg.Role, len(msg.ToolCalls), msg.StopReason))
		}
		trace = append(trace, "generated: "+strings.Join(roles, " "))
	}
	for i, checkpoint := range checkpoints {
		if i > 0 && len(checkpoint.Generated) < len(checkpoints[i-1].Generated) {
			violations = append(violations, fmt.Sprintf("checkpoint %d persisted %d messages after %d", i, len(checkpoint.Generated), len(checkpoints[i-1].Generated)))
		}
		for _, v := range transcriptViolations(append(cloneMessages(s.history), checkpoint.Generated...)) {
			violations = append(violations, fmt.Sprintf("checkpoint %d: %s", i, v))
		}
	}
	if elapsed > 10*time.Second {
		violations = append(violations, fmt.Sprintf("run took %s", elapsed))
	}
	if len(violations) > 0 {
		t.Fatalf("%s\n%s\n--- trace\n%s", s, strings.Join(violations[:min(len(violations), 10)], "\n"), strings.Join(trace, "\n"))
	}
}

// stressErrorAllowed reports whether a run may end in err. A run whose first
// request did not fit, by the estimate or by the provider's count of it, may
// fail for want of room; no other may, but for the provider's rejection of a
// request that could be made no smaller, which the requests' checks hold to
// having been answered first.
func stressErrorAllowed(err error, calls int, firstOver bool, s stressScenario) bool {
	if err == nil || errors.Is(err, ErrMaxIterations) {
		return true
	}
	var overflow *ContextOverflowError
	if errors.As(err, &overflow) {
		return s.providerWindow > 0
	}
	if (calls == 0 || firstOver) && outOfRoom(err) {
		return true
	}
	if s.continueFinal && errors.Is(err, ErrContextExhausted) {
		return true
	}
	if s.requireResponse && (strings.Contains(err.Error(), "missing successful respond result") || strings.Contains(err.Error(), "tool batch denied before required response")) {
		return true
	}
	return false
}

// transcriptViolations checks the durable transcript a run leaves: every call
// answered once, in the exchange it was made in.
func transcriptViolations(history []messages.ChatMessage) []string {
	var violations []string
	open := map[string]bool{}
	flush := func(at int) {
		for id := range open {
			violations = append(violations, fmt.Sprintf("transcript: call %s unanswered at message %d", id, at))
		}
		open = map[string]bool{}
	}
	for i, msg := range history {
		switch msg.Role {
		case messages.MessageRoleAssistant:
			flush(i)
			for _, call := range msg.ToolCalls {
				open[call.ID] = true
			}
		case messages.MessageRoleTool:
			if !open[msg.ToolCallID] {
				violations = append(violations, fmt.Sprintf("transcript: result %s at message %d answers no open call", msg.ToolCallID, i))
			}
			delete(open, msg.ToolCallID)
		case messages.MessageRoleUser:
			if isRealUser(msg) {
				flush(i)
			}
		}
	}
	flush(len(history))
	return violations
}

// growTool adds tools to the registry the agent draws on each time it runs,
// as activating a skill can: the next request offers more than the last.
type growTool struct {
	tools.NativeTool
	registry *tools.ToolRegistry
}

func (t *growTool) GetName() string { return "grow" }
func (t *growTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("grow", "adds tools", schema.Params{})
}

// Execute derives what it adds from its call's arguments alone, so parallel
// calls add the same tools whatever order they run in.
func (t *growTool) Execute(_ context.Context, args map[string]any) (string, error) {
	size := tools.Args(args).Int("size", 1)
	rng := rand.New(rand.NewPCG(uint64(size), 11))
	for i := range 1 + rng.IntN(4) {
		name := fmt.Sprintf("grown%d_%d", size, i)
		t.registry.Register(&describedTool{sizedTool: sizedTool{name: name}, description: words(rng, logSize(rng, 6_000))})
	}
	return "added tools", nil
}

// describedTool is a sizedTool with a schema as long as its description.
type describedTool struct {
	sizedTool
	description string
}

func (t *describedTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(t.name, t.description, schema.Params{"size": schema.Int("bytes of output")})
}
