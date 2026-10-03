package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// Compaction keeps a conversation within its context budget. Requests carry
// the history as contextView shows it, a pure function of the durable history
// and the compaction markers in it (messages.Compaction), so a conversation's
// requests keep a stable prefix between compactions, and every run of it, in
// any agent, sends the same thing. When a request outgrows compactAt of its
// budget, the agent compacts before sending it, in one of two tiers:
//
//   - Clearing: tool results older than the newest protectedResults of the
//     budget, which the model has already read, become short notes (a receipt
//     for stored output, a recall tool's stub, or a note of what was cleared).
//     This needs no model call, and stands when it brings the request under
//     clearTarget of the budget, or under compactAt when the request does
//     not fit or a provider rejected it.
//   - Summarizing: the compaction model (AgentConfig.CompactionModel, or the
//     request's own model) summarizes the conversation, which requests then
//     carry in place of everything before the marker. A turn in progress that
//     is no more than keptTurn of the budget stays verbatim after the summary.
//
// A provider's rejection of a request as too long for its context window
// compacts at once, or, when the output reserve is what did not fit, cuts
// it, and sends the request again (see learnOverflow). The markers leave the
// history whole: read_transcript still pages and searches everything they
// left out of requests.
const (
	compactAt        = 0.9
	clearTarget      = 0.6
	protectedResults = 0.2
	keptTurn         = 0.25
)

// summaryPrompt is the compaction model's system prompt.
const summaryPrompt = `You compact a conversation between a user and an AI coding agent so the agent can continue it without the original messages. Write a summary the agent can work from alone. Cover, in this order:

1. The user's requests and instructions, quoting each one verbatim, the latest in full.
2. Decisions made and constraints learned, with the reasons given.
3. Work done: files read, created or changed (with paths), commands run and what they showed, and any errors and how they were resolved.
4. Facts the agent will need again: names, values, identifiers, paths, snippets of code.
5. Where the work stands: what is finished, what remains, and the exact next step, if the conversation ended mid-task.

Write only the summary, in plain prose and lists. Do not answer the user or continue the work yourself.`

// contextView is history as requests carry it: without internal messages,
// with the last summary in place of the conversation it covers, and with the
// tool results the clear markers cover cleared. The result is a fresh slice;
// messages share their content with history.
func contextView(history []messages.ChatMessage, p projectionTools) []messages.ChatMessage {
	summary := lastSummary(history, len(history))
	cleared := clearedThrough(history)
	out := make([]messages.ChatMessage, 0, len(history)+1)
	start := 0
	if summary >= 0 {
		c, _ := history[summary].Compaction()
		start = summary + 1
		if turn := keptTurnStart(history, summary); c.KeepsTurn && turn >= 0 {
			start = turn
		}
		// While the summary covers the request in progress (no real user
		// message follows it), the images the model is working from, the
		// request's and those its last tool calls returned, go on as the
		// latest real user message's, which the projection sends.
		current, reply := -1, len(history)
		if start == summary+1 && lastIndex(history[summary+1:], isRealUser) < 0 {
			current, reply = lastIndex(history[:summary], isRealUser), lastIndex(history[:summary], isAssistant)
		}
		var images, working []messages.ContentPart
		for i, msg := range history[:start] {
			if msg.Role == messages.MessageRoleSystem {
				out = append(out, msg)
			}
			for _, part := range msg.Parts {
				switch {
				case !isImagePart(part):
				case current >= 0 && (i == current || msg.Role == messages.MessageRoleTool && i > reply):
					working = append(working, part)
				default:
					images = append(images, part)
				}
			}
		}
		// The summarized messages' other images stay referable: the
		// projection sends one only when the latest request names it.
		out = append(out, summaryMessage(c.Summary, images, p))
		if len(working) > 0 {
			out = append(out, messages.ChatMessage{
				Role:  messages.MessageRoleUser,
				Parts: append([]messages.ContentPart{{Type: "text", Text: "Images of the request in progress, which the summary above covers:"}}, working...),
			})
		}
	}
	for i := start; i < len(history); i++ {
		msg := history[i]
		if msg.Role == messages.MessageRoleInternal {
			continue
		}
		if i <= cleared {
			msg = clearedMessage(msg, p)
		}
		out = append(out, msg)
	}
	return out
}

// RequestView is history as an agent with tools sends it: compaction
// markers applied, summaries in place of what they cover and cleared tool
// results as their notes, without internal messages. history is only read.
func RequestView(history []messages.ChatMessage, tools []tools.Tool) []messages.ChatMessage {
	return contextView(history, projectionToolsFor(tools))
}

// CompactionPoint is the size over which a request budgeted at budget tokens
// is compacted before it is sent, 0 for an unlimited budget.
func CompactionPoint(budget int) int {
	if budget <= 0 {
		return 0
	}
	return int(compactAt * float64(budget))
}

// lastSummary is the index of the last summary marker before end, or -1.
func lastSummary(history []messages.ChatMessage, end int) int {
	return lastIndex(history[:end], func(msg messages.ChatMessage) bool {
		c, ok := msg.Compaction()
		return ok && c.Summary != ""
	})
}

// keptTurnStart is where the turn in progress before end begins: the last
// real user message after the last summary before end, or -1. A summary
// marker at end that keeps its turn keeps the messages from there.
func keptTurnStart(history []messages.ChatMessage, end int) int {
	if u := lastIndex(history[:end], isRealUser); u > lastSummary(history, end) {
		return u
	}
	return -1
}

// clearedThrough is the index of the newest tool result a clear marker
// covers, or -1. A marker names the last result with its tool call id
// before it.
func clearedThrough(history []messages.ChatMessage) int {
	through := -1
	for m, msg := range history {
		c, ok := msg.Compaction()
		if !ok || c.ClearThrough == "" {
			continue
		}
		for i := m - 1; i > through; i-- {
			if history[i].Role == messages.MessageRoleTool && history[i].ToolCallID == c.ClearThrough {
				through = i
				break
			}
		}
	}
	return through
}

// summaryMessage is how requests carry a summary: a synthetic user message,
// with the images of the messages it covers.
func summaryMessage(summary string, images []messages.ContentPart, p projectionTools) messages.ChatMessage {
	note := "[The conversation before this point was compacted into the summary below."
	if p.transcriptReadable {
		note += " Its full text remains readable: call read_transcript to page or search it."
	}
	msg := messages.ChatMessage{
		Role:     messages.MessageRoleUser,
		Content:  note + "]\n\n" + summary,
		Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true},
	}
	if len(images) > 0 {
		// Providers send a message's parts in place of its content once it
		// has any, and preparation for a model that cannot view images turns
		// the images into text parts: the summary leads the parts.
		promoteMessageContentToTextPart(&msg)
		msg.Parts = append(msg.Parts, images...)
	}
	return msg
}

// clearedMessage is msg as a clear marker leaves it: a tool result as its
// note, when that is smaller; anything else as it is.
func clearedMessage(msg messages.ChatMessage, p projectionTools) messages.ChatMessage {
	if msg.Role == messages.MessageRoleTool {
		if content, ok := clearedForm(msg, p); ok {
			msg.Content = content
		}
	}
	return msg
}

// clearedForm is the note a cleared tool result stands as, and whether it
// is smaller than the result. Denials and interruptions stay as they are.
func clearedForm(msg messages.ChatMessage, p projectionTools) (string, bool) {
	if msg.Content == ToolDeniedContent || msg.Content == ToolInterruptedContent {
		return "", false
	}
	var content string
	if stub, ok := p.recall.stub(msg.ToolName); ok {
		content = appendArtifactDescriptors(stub, msg, "")
	} else if ref := textArtifactRef(msg); ref != nil {
		content = appendArtifactDescriptors(artifactReceipt(*ref), msg, ref.ID)
	} else {
		content = clearedNote(msg, p)
	}
	if estimatedStringTokens(content) >= estimatedStringTokens(msg.Content) {
		return "", false
	}
	return content, true
}

// clearedNote is the note a cleared inline result stands as. Counting its
// lines reads the whole result, so a run keeps the notes it makes.
func clearedNote(msg messages.ChatMessage, p projectionTools) string {
	key := clearedKey{content: msg.Content, readable: p.transcriptReadable}
	note, ok := p.notes[key]
	if !ok {
		hint := "Call the tool again if you need it."
		if p.transcriptReadable {
			hint = "read_transcript can page or search it."
		}
		// Lines are counted as an artifact's are, so a note and a receipt
		// of the same output agree: a trailing newline ends a line, not
		// starts one.
		lines := strings.Count(msg.Content, "\n")
		if !strings.HasSuffix(msg.Content, "\n") {
			lines++
		}
		note = fmt.Sprintf("[Earlier tool output cleared from context: %d bytes, %d lines. %s]", len(msg.Content), lines, hint)
		if p.notes != nil {
			p.notes[key] = note
		}
	}
	return appendArtifactDescriptors(note, msg, "")
}

// countedTokens is what the next request over history, estimated at
// requestEstimate, comes to in the provider's count: the input the last
// response reported, moved by how far requestEstimate is from the recorded
// estimate of the request that response answered. What the two requests
// share (the system prompt, tool schemas, unchanged history) prices the same
// in both estimates and cancels, so only what changed in between is
// estimated: messages appended, content compaction took out, images
// selected, tools added or dropped. It reports false when the last response
// reported no count or recorded no estimate.
func countedTokens(history []messages.ChatMessage, requestEstimate int) (int, bool) {
	k := lastIndex(history, isAssistant)
	if k < 0 {
		return 0, false
	}
	total := history[k].GetInputTokens()
	sent, ok := history[k].RequestEstimate()
	if total <= 0 || !ok {
		return 0, false
	}
	return max(0, total+requestEstimate-sent), true
}

// clearPoint picks what a clear marker clears through: every tool result
// older than the newest ones, which together take up to protect tokens, and
// never one the model has not read. The marker names a result that survives
// saving, never a denial, and whose tool call id appears nowhere after it: a
// marker resolves to the last result with its id, and providers that number
// calls per response reuse ids. It returns the tool call id to clear through,
// or "", and what clearing saves.
func clearPoint(view []messages.ChatMessage, p projectionTools, protect int) (string, int) {
	read := lastIndex(view, isAssistant)
	kept := 0
	later := map[string]bool{}
	for i := len(view) - 1; i >= 0; i-- {
		msg := view[i]
		for _, call := range msg.ToolCalls {
			later[call.ID] = true
		}
		if msg.Role != messages.MessageRoleTool {
			continue
		}
		kept += estimatedStringTokens(msg.Content)
		reused := later[msg.ToolCallID]
		later[msg.ToolCallID] = true
		// A denial is no anchor: hosts strip denied exchanges before saving.
		if i > read || kept <= protect || msg.ToolCallID == "" || reused || msg.Content == ToolDeniedContent {
			continue
		}
		saved := 0
		for _, m := range view[:i+1] {
			if c := clearedMessage(m, p); c.Content != m.Content {
				saved += estimatedStringTokens(m.Content) - estimatedStringTokens(c.Content)
			}
		}
		return msg.ToolCallID, saved
	}
	return "", 0
}

// errNothingToCompact means a request a provider rejected as too long has
// nothing a compaction would cover.
var errNothingToCompact = errors.New("the conversation has nothing left to compact")

// compactionPlan is how to compact a conversation: by clearing tool results
// through a call, or by summarizing input in about target tokens, keeping the
// turn in progress verbatim or not. The zero plan compacts nothing.
type compactionPlan struct {
	clearThrough string
	summarize    bool
	input        []messages.ChatMessage
	keepsTurn    bool
	target       int
}

func (p compactionPlan) none() bool { return p.clearThrough == "" && !p.summarize }

// summaryTarget is about how long a summary on model may be for req: a tenth
// of req's budget, within reason, and, on req's own model, no more than half
// req's output limit, which the summary's thinking shares.
func summaryTarget(req *CompletionRequest, model string) int {
	target := 4_096
	if budget := req.MaxContextTokens; budget > 0 {
		target = min(max(budget/10, 256), 4_096)
	}
	if model == req.Model && req.MaxTokens > 0 {
		target = min(target, max(req.MaxTokens/2, 128))
	}
	return target
}

// planCompaction decides how to compact the run's conversation ahead of req,
// which comes to size tokens. Each plan is priced by the request it would
// leave: req's size moved by how far the history the plan's marker leaves
// requests carrying estimates from what they carry now. Clearing old tool
// results stands when it brings the request under clearTarget of its budget,
// or under compactAt when the request does not fit as it is. A summary,
// keeping the turn in progress when it is small or, when fresh, because it
// holds input the caller has not cleared yet, must bring the request under
// compactAt, the trigger, so the next request does not compact again; when
// the request does not fit its budget as it is (or a provider rejected it,
// force), within the budget is enough. Fresh input is never summarized. When
// no summary meets its goal, a clear that brings the request within budget
// is taken; otherwise nothing is compacted: the request goes as it is, or
// fails the budget check.
func (r *agentRun) planCompaction(req *CompletionRequest, size int, force, fresh bool) compactionPlan {
	p := r.projectionTools(r.loopTools())
	budget := req.MaxContextTokens
	estimate := messageEstimator(req)
	viewTokens := func(view []messages.ChatMessage) int {
		n := 0
		for _, msg := range view {
			n += estimate(msg)
		}
		return n
	}
	view := contextView(r.msgs, p)
	now := viewTokens(view)
	after := func(c messages.Compaction) int {
		return size + viewTokens(contextView(append(slices.Clip(r.msgs), c.Message()), p)) - now
	}
	// Clearing usually aims well under the trigger, so it lasts; a request
	// that must compact now takes it once it lands under the trigger, and,
	// when no summary can do better, once it fits.
	clear, cleared := compactionPlan{}, size
	if budget > 0 {
		goal := clearTarget
		if force || size > budget {
			goal = compactAt
		}
		if through, saved := clearPoint(view, p, int(protectedResults*float64(budget))); through != "" {
			if size-saved <= int(goal*float64(budget)) {
				return compactionPlan{clearThrough: through}
			}
			clear, cleared = compactionPlan{clearThrough: through}, size-saved
		}
	}
	target := summaryTarget(req, r.agent.compactionModel(req))
	placeholder := strings.Repeat("x", target*4)
	limit := budget
	if limit <= 0 {
		limit = size
	}
	goal := int(compactAt * float64(limit))
	if force || size > limit {
		goal = limit
		if force && budget <= 0 {
			goal = size - 1
		}
	}
	// A summary needs something to cover: a conversation before the turn it
	// keeps, or the turn it does not.
	summarizable := func(input []messages.ChatMessage) bool {
		return slices.ContainsFunc(input, func(msg messages.ChatMessage) bool { return msg.Role != messages.MessageRoleSystem })
	}
	// A summary that failed earlier in the run is tried again only for a
	// request a provider rejected.
	canSummarize := force || !r.summaryFailed
	if u := keptTurnStart(r.msgs, len(r.msgs)); u >= 0 && canSummarize {
		turn := viewTokens(contextView(r.msgs[u:], p))
		prefix := contextView(r.msgs[:u], p)
		if (fresh || turn <= int(keptTurn*float64(limit))) && summarizable(prefix) && after(messages.Compaction{Summary: placeholder, KeepsTurn: true}) <= goal {
			return compactionPlan{summarize: true, input: prefix, keepsTurn: true, target: target}
		}
	}
	if !fresh && canSummarize && summarizable(view) && after(messages.Compaction{Summary: placeholder}) <= goal {
		return compactionPlan{summarize: true, input: view, target: target}
	}
	if budget > 0 && cleared <= budget {
		return clear
	}
	return compactionPlan{}
}

// compact carries out plan, appending its marker to the run, so the run's
// checkpoint persists it with the request. inPlace lets the request's own
// model summarize req sent as it stands (see summarizeInPlace). It returns
// what it did (see CompactionNote), for the note the caller makes once the
// compacted request is known to fit.
func (r *agentRun) compact(ctx context.Context, req *CompletionRequest, plan compactionPlan, inPlace bool) (string, error) {
	if !plan.summarize {
		marker := messages.Compaction{ClearThrough: plan.clearThrough}
		r.append(marker.Message())
		return CompactionNote(marker, ""), nil
	}
	r.adapt(RequestAdaptation{Feature: FeatureCompaction, Message: "Compacting the conversation · summarizing earlier exchanges"})
	p := r.projectionTools(r.loopTools())
	summarize := func(model string, target int) (string, error) {
		summary, response, err := r.agent.summarize(ctx, req, model, plan.input, p, target)
		if response != nil {
			// A summary refused after it was made was billed all the same.
			r.recordSummary(req, model, response)
		}
		return summary, err
	}
	model := r.agent.compactionModel(req)
	summary, transcript, err := "", true, error(nil)
	if model == req.Model && inPlace && req.ResponseSchema == nil {
		summary, transcript, err = r.summarizeInPlace(ctx, req, plan)
	}
	if transcript {
		summary, err = summarize(model, plan.target)
	}
	if err != nil && model != req.Model && ctx.Err() == nil {
		// The request's own model can take what it was about to be sent.
		r.adapt(RequestAdaptation{Feature: FeatureCompactionFailure, Message: fmt.Sprintf("Compaction model %s failed; summarizing with %s: %v", model, req.Model, err)})
		model = req.Model
		summary, err = summarize(model, summaryTarget(req, model))
	}
	if err != nil {
		return "", fmt.Errorf("compact the conversation: %w", err)
	}
	marker := messages.Compaction{Summary: summary, KeepsTurn: plan.keepsTurn}
	r.append(marker.Message())
	if model == req.Model {
		model = ""
	}
	return CompactionNote(marker, model), nil
}

// The features OnAdaptation reports compactions under: one that happened or
// is under way, and one that failed or was withdrawn.
const (
	FeatureCompaction        = "compaction"
	FeatureCompactionFailure = "compaction-failure"
)

// CompactionNote says what compaction c did, as transcripts show it: its
// tool results cleared, or a summary made, on model when that is not the
// conversation's own ("").
func CompactionNote(c messages.Compaction, model string) string {
	switch {
	case c.Summary == "":
		return "Context compacted · tool results cleared"
	case model != "":
		return "Context compacted · summarized with " + ModelName(model)
	}
	return "Context compacted · summarized"
}

// groupDigits renders n with its thousands separated by commas.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// summarizeInPlace asks req's own model to summarize the conversation req
// carries, sent as req is (see inPlaceSummaryRequest). It reports true when
// the transcript form is to be tried instead: the request failed, perhaps
// for a shape the transcript does not share, such as two user turns in a
// row, unless the run was canceled; or the model called a tool.
func (r *agentRun) summarizeInPlace(ctx context.Context, req *CompletionRequest, plan compactionPlan) (string, bool, error) {
	ask := inPlaceSummaryRequest(req, plan.keepsTurn, plan.target)
	r.keyPromptCache(ask)
	response, err := r.agent.complete(ctx, ask)
	if err != nil {
		return "", ctx.Err() == nil, err
	}
	r.recordSummary(req, req.Model, response)
	if len(response.ToolCalls) > 0 {
		return "", true, nil
	}
	summary, err := summaryText(response)
	return summary, false, err
}

// inPlaceSummaryRequest asks req's own model for a summary of the
// conversation req carries, in about target tokens, appended to req as it
// stands: with the same system prompt, tools, settings and cache keys, the
// provider's prompt cache covers all the conversation the last request sent.
// With keepsTurn the summary leaves out the turn in progress, which stays
// verbatim after it.
func inPlaceSummaryRequest(req *CompletionRequest, keepsTurn bool, target int) *CompletionRequest {
	ask := "Do not call any tools or continue the work: this request is for a summary of the conversation above.\n\n" +
		summaryPrompt + fmt.Sprintf("\n\nSummarize the conversation above in at most %d words.", target*3/4)
	if keepsTurn {
		ask += " Leave out the latest user request and the work after it: they stay verbatim after your summary."
	}
	summary := *req
	summary.Messages = append(slices.Clip(req.Messages), messages.ChatMessage{Role: messages.MessageRoleUser, Content: ask})
	return &summary
}

// summaryText is the summary response carries, or why it is refused.
func summaryText(response *messages.ChatMessage) (string, error) {
	summary := strings.TrimSpace(response.GetContent())
	switch {
	case response.StopReason == messages.StopReasonMaxTokens:
		// Its last section, where the work stands, is what was cut.
		return "", errors.New("the compaction model's summary was cut off at its output limit")
	case summary == "":
		return "", errors.New("the compaction model returned an empty summary")
	}
	return summary, nil
}

// recordSummary keeps what a summary on model cost: a usage record in the
// run, naming model when it is not req's, and OnCompactionUsage.
func (r *agentRun) recordSummary(req *CompletionRequest, model string, response *messages.ChatMessage) {
	usage := response.UsageRecord()
	if model != req.Model {
		usage.Metadata[messages.MetadataKeyUsageModel] = model
	}
	r.promptCache.ReadInputTokens += response.GetCacheReadInputTokens()
	r.promptCache.WriteInputTokens += response.GetCacheWriteInputTokens()
	if _, ok := usage.Metadata[messages.MetadataKeyCacheReadInputTokens]; !ok {
		// A one-off request its provider reported no cache use for read
		// nothing from a cache; unreported, it would make the conversation's
		// cache rate unknown.
		usage.SetPromptCacheUsage(0, 0)
	}
	r.append(usage)
	if r.cb != nil && r.cb.OnCompactionUsage != nil {
		cost, reported := response.GetReportedCost()
		r.cb.OnCompactionUsage(usage.UsageModel(), UsageUpdate{
			InputTokens: response.GetInputTokens(), OutputTokens: response.GetOutputTokens(),
			CacheReadInputTokens: response.GetCacheReadInputTokens(), CacheWriteInputTokens: response.GetCacheWriteInputTokens(),
			ReportedCostUSD: cost, CostReported: reported,
		})
	}
}

// summarize asks model to summarize history, rendered as a
// transcript without its system messages, in about target tokens, for the
// conversation req continues. A transcript too long for the model is sent
// again with the tool results the model has read cleared, then with its
// middle left out, cut to what the model's window leaves when that is known
// and to half that, else to a half and a quarter; no attempt repeats the one
// before. The first attempt is the first that fits when the model's window
// is known.
func (a *Agent) summarize(ctx context.Context, req *CompletionRequest, model string, history []messages.ChatMessage, p projectionTools, target int) (string, *messages.ChatMessage, error) {
	// Requests keep the system messages verbatim beside the summary, so the
	// summary neither pays for them nor restates them.
	history = slices.DeleteFunc(slices.Clone(history), func(msg messages.ChatMessage) bool {
		return msg.Role == messages.MessageRoleSystem
	})
	// Results after the model's last reply are what it is about to act on,
	// so they stay whole: cleared, a summary covering them would send it to
	// fetch the same things again. For the same reason recall results are
	// rendered whole, not elided as read_transcript shows them.
	full := renderTranscript(history, nil)
	read := slices.Clone(history)
	for i := range lastIndex(read, isAssistant) {
		read[i] = clearedMessage(read[i], p)
	}
	cleared := renderTranscript(read, nil)
	limit := a.summaryLimit(ctx, req, model, target)
	cuts := []int{len(cleared) / 2, len(cleared) / 4}
	if limit > 0 {
		// The limit rests on an estimate of four bytes a token, which
		// dense text such as code undercuts.
		cuts = []int{limit * 4, limit * 2}
	}
	candidates := []string{cleared}
	for _, n := range cuts {
		candidates = append(candidates, omitMiddle(cleared, n))
	}
	attempts := []string{full}
	for _, transcript := range candidates {
		if transcript != attempts[len(attempts)-1] {
			attempts = append(attempts, transcript)
		}
	}
	first := 0
	if limit > 0 {
		for first < len(attempts)-1 && estimatedStringTokens(attempts[first]) > limit {
			first++
		}
	}
	var err error
	for _, transcript := range attempts[first:] {
		var response *messages.ChatMessage
		response, err = a.complete(ctx, summaryRequest(req, model, transcript, target))
		if err == nil {
			summary, err := summaryText(response)
			return summary, response, err
		}
		if _, overflow := contextOverflow(err); !overflow {
			return "", nil, err
		}
	}
	return "", nil, err
}

// summaryLimit is about how many tokens a summary transcript on model may
// take, 0 when that is unknown: req's own budget when req's model
// summarizes, else what model's window leaves for an output of twice target;
// either less room for the summary prompt.
func (a *Agent) summaryLimit(ctx context.Context, req *CompletionRequest, model string, target int) int {
	const prompt = 512
	budget := req.MaxContextTokens
	if model != req.Model {
		route := targetForRequest(&CompletionRequest{Model: model, BaseURL: req.BaseURL})
		window, err := discoverContextWindow(ctx, a, route)
		if err != nil {
			return 0
		}
		budget, _ = windowFit(route.Provider, window, 2*target)
	}
	if budget <= prompt {
		return 0
	}
	return budget - prompt
}

// compactionModel is the model that summarizes for req.
func (a *Agent) compactionModel(req *CompletionRequest) string {
	if a.config.CompactionModel != "" {
		return a.config.CompactionModel
	}
	return req.Model
}

// summaryRequest asks model for a summary of transcript in about target
// tokens, with req's endpoint settings. Another model than req's keeps none
// of req's model-specific settings, nor its key.
func summaryRequest(req *CompletionRequest, model, transcript string, target int) *CompletionRequest {
	summary := &CompletionRequest{
		Model: model, BaseURL: req.BaseURL, Timeout: req.Timeout, Deadline: req.Deadline,
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleSystem, Content: summaryPrompt},
			{Role: messages.MessageRoleUser, Content: fmt.Sprintf("<transcript>\n%s</transcript>\n\nSummarize the conversation above in at most %d words.", transcript, target*3/4)},
		},
	}
	if summary.Model == req.Model {
		summary.ModelHost, summary.APIKey, summary.Capabilities, summary.CacheSessionID = req.ModelHost, req.APIKey, req.Capabilities, req.CacheSessionID
		summary.MaxTokens, summary.ThinkingEffort, summary.Temperature = req.MaxTokens, req.ThinkingEffort, req.Temperature
	}
	return summary
}

// omitMiddle cuts text to at most size bytes, its note of the cut
// included: a quarter of them from its head, the rest from its tail, on
// UTF-8 boundaries.
func omitMiddle(text string, size int) string {
	const omitted = "\n[... middle of the transcript omitted ...]\n"
	if len(text) <= size {
		return text
	}
	size = max(0, size-len(omitted))
	head := text[:safeUTF8Boundary(text, size/4)]
	tail := text[safeUTF8TailBoundary(text, len(text)-(size-len(head))):]
	return head + omitted + tail
}

// adapt reports a note to the caller.
func (r *agentRun) adapt(note RequestAdaptation) {
	if r.cb != nil && r.cb.OnAdaptation != nil {
		r.cb.OnAdaptation(note)
	}
}
