package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// The tool loop under a context budget.
//
// A run under MaxContextTokens never fails for want of room once its first
// request fits. The agent holds one invariant: every state it commits to can
// be finished, meaning a request without tools (or with only the response
// tool), carrying the system messages and the active exchange in its final
// form (finalForm), fits the budget, and so does the next request with tools.
// The first request's state can be finished, since its active exchange is only
// the new input. Everything the run commits afterwards is checked first:
//
//   - A tool batch is planned before it runs (planBatch). Every call is charged
//     up front for the least it can leave behind, run or not; calls the room
//     cannot take are refused, and a batch that cannot fit even refused, or
//     in which no call would run, is dropped unrun, and the next model call
//     finishes the run without tools.
//   - Once the batch has run, its actual results are checked against the same
//     room (fitResults). A result that does not fit, because of media, a tool
//     that ignored its page bound, or anything the plan could not foresee,
//     is replaced by a note no larger than what the plan charged for it, so
//     the check always succeeds.
//   - Input committed between model calls (admitted peer input, continuation
//     input, the response-tool reminder) is stored as an artifact behind a
//     receipt when it would not fit whole (boundInput). Input after a final
//     answer that cannot fit even as a receipt ends the run with
//     ErrContextExhausted, the answer intact; before any answer, admitted
//     input that cannot fit stays staged for a later boundary.
//
// Nothing is reserved on a guess: every check measures the state it commits.
// Checks are in the projection's estimates, so the guarantee is as good as
// they are.
//
// Within that, a batch's pages share the room the next request has left. The
// projection keeps every recall result the model has not read yet verbatim, so
// the pages must fit together. Pages have a useful floor and a ceiling. Below
// the floor, paging costs more than it saves: every page is another model call
// that re-sends the whole context. So parallel reads run only as many as can
// each have a floor-sized page, and the rest are refused; a lone read may take
// less when that is all the room there is, down to tools.PageMinBytes. Above
// the ceiling, output would have been stored as an artifact had it not been a
// recall. Without an ArtifactStore nothing can shrink to a receipt, so ordinary
// results are paged like reads: truncated to the batch's page bound.

// pageNoteTokens covers a pager's continuation and truncation notes.
const pageNoteTokens = 24

// pageEnvelopeTokens covers what a page's tool result carries besides the
// page, for a call ID of ordinary length.
const pageEnvelopeTokens = 32

// pageFloorBytes is the useful floor of a page.
const pageFloorBytes = 4 << 10

// PageFloorTokens is the room a useful page takes in a request. A host sizing
// a context budget reserves at least this much for tool output beyond
// ContextFloorTokens.
const PageFloorTokens = pageFloorBytes/4 + pageEnvelopeTokens

// DefaultInlineToolResultTokens is the default AgentConfig.InlineToolResultTokens.
const DefaultInlineToolResultTokens = toolInlineTokenLimit

// ErrToolCallRefused marks a tool call refused, and not run, because the
// context budget had no room for its result.
var ErrToolCallRefused = errors.New("tool call refused for want of context room")

// ErrContextExhausted ends a run whose final answer was reopened with input
// the context budget has no room for. The answer stands; the input was not
// admitted.
var ErrContextExhausted = errors.New("the context budget has no room for input after the final answer")

// receiptReserveRef has the longest counts a receipt spells out, so its
// receipt is the largest a result stored as an artifact can stand as.
var receiptReserveRef = artifacts.Ref{ID: prospectiveArtifactID, Bytes: 1 << 40, Lines: 1 << 30}

// reasoningReplayKeys are the metadata keys under which providers keep
// reasoning they replay: signed, encrypted or signature-bound state that is
// sent back whole and belongs to the tool calls it preceded.
var reasoningReplayKeys = []string{
	anthropic.ThinkingBlocksKey,
	openai.ResponsesReasoningItemsKey,
	openai.ResponsesReasoningModelKey,
	gemini.ThoughtSignaturesKey,
	openrouter.MetadataKey,
}

// ContextFloorTokens estimates the least req can be projected to: its tool
// schemas, its system messages with the omission marker the projection may
// add, and its active exchange with every tool result demoted to its receipt
// or recall stub. Earlier exchanges can all be omitted, so they add nothing.
// What req.MaxContextTokens holds beyond this is room for content that is not
// in req yet. With only a system prompt and tools, it is what every request
// with them carries whole. Results demote to receipts only when req offers
// read_artifact, since a receipt is of no use without it.
func ContextFloorTokens(req *CompletionRequest) int {
	agentTools := projectionToolsFor(req.Tools)
	agentTools.replaysReasoning = providerReplaysReasoning(req)
	return estimateToolSchemaTokens(req.Tools) + contextFloorMessageTokens(req.Messages, agentTools, agentTools.artifactsReadable, len(req.Messages))
}

// MinContextTokens is the least budget in which a request like req can read
// one useful page: ContextFloorTokens and PageFloorTokens. A host can refuse a
// budget below it.
func MinContextTokens(req *CompletionRequest) int {
	return ContextFloorTokens(req) + PageFloorTokens
}

// contextFloorMessageTokens is ContextFloorTokens for the messages alone. Tool
// results from unread on have not been read by the model: the projection
// sends an unread recall result whole.
func contextFloorMessageTokens(history []messages.ChatMessage, agentTools projectionTools, hasStore bool, unread int) int {
	marker := projectionMarker(hasStore && agentTools.artifactsListable && agentTools.artifactsReadable, agentTools.transcriptReadable)
	return activeExchangeTokens(history, marker, func(i int, msg messages.ChatMessage) int {
		return floorTokens(msg, agentTools, hasStore, i >= unread)
	})
}

// floorTokens is the least msg, in the active exchange, can be projected to.
// An unread recall result is sent whole; a read one demotes to its stub. The
// projection spills any ordinary result its receipt undercuts, however small
// (spillActiveToolResults), so the floor charges that receipt.
func floorTokens(msg messages.ChatMessage, agentTools projectionTools, hasStore, unread bool) int {
	tokens := agentTools.estimate(msg)
	stub, isRecall := agentTools.recall.stub(msg.ToolName)
	if isRecall {
		if unread || stub == "" {
			return tokens
		}
		if plan := planToolDemotion(msg, hasStore, stub); plan.ok {
			tokens += plan.tokens - estimatedStringTokens(msg.Content)
		}
		return tokens
	}
	if receipt, ok := activeReceiptTokens(msg, hasStore); ok {
		tokens += receipt - estimatedStringTokens(msg.Content)
	}
	return tokens
}

// activeReceiptTokens is what an ordinary result costs once
// spillActiveToolResults demotes it to its receipt, and whether it would: a
// result is spilled when it is or can be stored and its receipt is smaller.
func activeReceiptTokens(msg messages.ChatMessage, hasStore bool) (int, bool) {
	if msg.Role != messages.MessageRoleTool || msg.Content == ToolDeniedContent || msg.Content == "" {
		return 0, false
	}
	ref := textArtifactRef(msg)
	if ref == nil {
		if !hasStore {
			return 0, false
		}
		prospective := prospectiveTextRef(msg.Content)
		ref = &prospective
	}
	receipt := estimatedStringTokens(appendArtifactDescriptors(artifactReceipt("tool output", *ref), msg, ref.ID))
	return receipt, receipt < estimatedStringTokens(msg.Content)
}

// activeExchangeTokens is what history's system messages cost, with marker
// when the projection has not added one, plus cost over the messages of the
// active exchange, from the last real user message on. Earlier exchanges can
// all be omitted, so they add nothing.
func activeExchangeTokens(history []messages.ChatMessage, marker string, cost func(i int, msg messages.ChatMessage) int) int {
	total := systemTokens(history, marker)
	users := realUserIndexes(history)
	if len(users) == 0 {
		return total
	}
	for i := users[len(users)-1]; i < len(history); i++ {
		if msg := history[i]; msg.Role != messages.MessageRoleSystem && msg.Role != messages.MessageRoleInternal {
			total += cost(i, msg)
		}
	}
	return total
}

// systemTokens is what history's system messages cost, with marker when the
// projection has not added one.
func systemTokens(history []messages.ChatMessage, marker string) int {
	total := 0
	marked := false
	for _, msg := range history {
		if msg.Role == messages.MessageRoleSystem {
			total += estimateProjectedMessageTokens(msg)
			marked = marked || strings.Contains(msg.Content, "[Context projection:")
		}
	}
	if !marked {
		_, delta := projectionMarkerTokenDelta(history, marker)
		total += delta
	}
	return total
}

// finishTokens estimates the least a request finishing from history can be:
// its system messages with the longest omission marker, the finish tools'
// schemas, and its active exchange in final form. Earlier exchanges can all
// be omitted.
func finishTokens(history []messages.ChatMessage, agentTools projectionTools, finishSchemas int) int {
	return finishSchemas + activeExchangeTokens(history, projectionMarker(true, true), func(_ int, msg messages.ChatMessage) int {
		return agentTools.estimate(finalForm(msg, agentTools.recall))
	})
}

// finalForm is msg as a finishing request carries it: tool calls and results
// rewritten as text, a result no larger than its recall stub or a note of its
// size. An assistant message whose calls are rewritten loses the reasoning a
// provider replays with them (signed thinking, encrypted reasoning items,
// thought signatures), which belongs to calls that no longer exist; its plain
// reasoning stays, since the providers that send it (DeepSeek, Qwen) require
// it on prior assistant turns. A receipt would point at read_artifact, which a
// finishing request does not offer.
func finalForm(msg messages.ChatMessage, recall recallStubs) messages.ChatMessage {
	if len(msg.ToolCalls) == 0 && msg.Role != messages.MessageRoleTool && len(msg.Parts) == 0 {
		// Nothing to rewrite.
		return msg
	}
	m := msg.Clone()
	if len(m.ToolCalls) > 0 {
		for _, key := range reasoningReplayKeys {
			delete(m.Metadata, key)
		}
	}
	if m.Role == messages.MessageRoleTool {
		content := m.Content
		if stub, isRecall := recall.stub(m.ToolName); isRecall {
			content = stub
		} else {
			size := int64(len(m.Content))
			if ref := textArtifactRef(m); ref != nil {
				size = ref.Bytes
			}
			content = omittedToolOutput(size)
		}
		if estimatedStringTokens(content) < estimatedStringTokens(m.Content) {
			m.Content = content
		}
		m.Parts = nil
	}
	flattenToolExchange(&m)
	return m
}

// keptForm is msg as a finishing request with room for the run's results
// carries it: finalForm, except that a result the run holds inline is written
// as text whole. A result still stored as an artifact is noted, since its
// receipt would point at read_artifact, which the request does not offer.
func keptForm(msg messages.ChatMessage, recall recallStubs) messages.ChatMessage {
	if msg.Role != messages.MessageRoleTool || textArtifactRef(msg) != nil || msg.Content == "" {
		return finalForm(msg, recall)
	}
	m := msg.Clone()
	m.Parts = nil
	flattenToolExchange(&m)
	return m
}

// restoredSpill is msg with the text a projection spilled to store read back
// inline, for a finishing request with room for it: msg itself when it holds
// no spill or store cannot return it.
func restoredSpill(ctx context.Context, store artifacts.Store, msg messages.ChatMessage) messages.ChatMessage {
	ref := textArtifactRef(msg)
	if msg.Role != messages.MessageRoleTool || ref == nil || store == nil {
		return msg
	}
	data, err := readArtifactBytes(ctx, store, ref.ID, ref.Bytes)
	if err != nil {
		return msg
	}
	m := msg.Clone()
	m.Content, m.Parts = string(data), nil
	return m
}

func omittedToolOutput(bytes int64) string {
	return fmt.Sprintf("[tool output omitted to fit the context budget; %d bytes]", bytes)
}

func refusalText(name string) string {
	return fmt.Sprintf("Not run: the context budget has no room for this result. Call %s again after reading the others, or answer with what you have.", name)
}

// IsToolRefusal reports whether msg is the result of a call the context budget
// refused (ErrToolCallRefused): the call was never run nor put to approval.
func IsToolRefusal(msg messages.ChatMessage) bool {
	return msg.Role == messages.MessageRoleTool && msg.Content == refusalText(msg.ToolName)
}

func droppedText(name string) string {
	return fmt.Sprintf("[%s ran, but the context budget has no room for its result]", name)
}

// batchBudget is what planning a batch and fitting its results measure
// against: the request the batch's response answered.
type batchBudget struct {
	// req is the request as sent: projected messages and clamped budget.
	req          *CompletionRequest
	agentTools   projectionTools
	hasStore     bool
	inlineTokens int
	// responseTool must run if called: a run that requires it cannot end
	// without it.
	responseTool string
	// nextSchemas is what the tools of the next request with tools cost,
	// finishSchemas what a finishing request's cost. A reply to a finishing
	// request is followed by a request with every tool again.
	nextSchemas, finishSchemas int
	// sandbox is what the sandbox context each request with tools adds to
	// the system prompt costs; the durable history never carries it.
	sandbox int
	// caps are the model's capabilities, as the request resolved them.
	// Preparation adapts history to them before projection, writing images
	// and tool exchanges as text for a model without them.
	caps *ModelCapabilities
	// imageRecall reports whether a recall call will return an image, which
	// the next request carries whole.
	imageRecall func(messages.ChatMessageToolCall) bool
	// memo, when the run keeps one, holds what its history costs the room
	// checks, so each measures only what it appends.
	memo *roomMemo
}

// roomMemo keeps what each message of a run's history costs its batches'
// room checks, measured once: every check measures the same history with
// something appended, and the history is append-only until a message is
// rewritten in place, as a durable spill does, or what the costs depend on
// changes; either resets it.
type roomMemo struct {
	key   string
	costs []roomCost
	// ratio is what the costs' measured parts are scaled by (see rescale).
	ratio float64
}

// roomCost is what a message costs the next request with tools, read and
// unread, and a finishing request. A cost is its message's price plus
// offsets for the forms the checks assume, so a measured price moves it by
// the same amount: inline and final are what the message's adapted and final
// forms were measured at, zero for an estimate, and inlineForm and finalForm
// are those forms, to remeasure when a form learns a price.
type roomCost struct {
	read, unread, finish  int
	inline, final         int
	inlineForm, finalForm uint64
}

// reset empties m unless it was measured under key, and scales it to ratio.
func (m *roomMemo) reset(key string, ratio float64) {
	if m.key != key {
		m.key, m.costs = key, nil
	}
	m.rescale(ratio)
}

// rescale moves the costs' measured parts to ratio from the ratio they were
// scaled by.
func (m *roomMemo) rescale(ratio float64) {
	if m.ratio == ratio {
		return
	}
	for i := range m.costs {
		c := &m.costs[i]
		if c.inline > 0 {
			d := scaledCount(c.inline, ratio) - scaledCount(c.inline, m.ratio)
			c.read += d
			c.unread += d
		}
		if c.final > 0 {
			c.finish += scaledCount(c.final, ratio) - scaledCount(c.final, m.ratio)
		}
	}
	m.ratio = ratio
}

// remeasure measures again the messages of history whose forms learned
// prices.
func (m *roomMemo) remeasure(b batchBudget, history []messages.ChatMessage, learned map[uint64]int) {
	for i := range m.costs {
		if i >= len(history) {
			return
		}
		_, inline := learned[m.costs[i].inlineForm]
		_, final := learned[m.costs[i].finalForm]
		if inline || final {
			m.costs[i] = m.cost(b, history[i])
		}
	}
}

// cost is what msg costs the room checks.
func (m *roomMemo) cost(b batchBudget, msg messages.ChatMessage) roomCost {
	if msg.Role == messages.MessageRoleSystem || msg.Role == messages.MessageRoleInternal {
		return roomCost{}
	}
	adapted := b.adapt([]messages.ChatMessage{msg})[0]
	final := finalForm(adapted, b.agentTools.recall)
	c := roomCost{
		read:       floorTokens(adapted, b.agentTools, b.hasStore, false),
		unread:     floorTokens(adapted, b.agentTools, b.hasStore, true),
		finish:     b.agentTools.estimate(final),
		inlineForm: formFingerprint(adapted),
		finalForm:  formFingerprint(final),
	}
	c.inline, _ = b.agentTools.measured.count(c.inlineForm)
	c.final, _ = b.agentTools.measured.count(c.finalForm)
	return c
}

// measured extends m over prefix and returns its costs.
func (m *roomMemo) measured(b batchBudget, prefix []messages.ChatMessage) []roomCost {
	if len(prefix) < len(m.costs) {
		m.costs = nil
	}
	for _, msg := range prefix[len(m.costs):] {
		m.costs = append(m.costs, m.cost(b, msg))
	}
	return m.costs
}

// memoKey is what b's measure of a message depends on: the tools offered,
// the store, and the model with what its capabilities adapt.
func (b batchBudget) memoKey(offered []tools.Tool) string {
	var key strings.Builder
	for _, tool := range offered {
		key.WriteString(tool.GetName())
		key.WriteByte(0)
	}
	noImages, noTools := false, false
	if b.caps != nil {
		noImages, noTools = omitsImages(*b.caps), b.caps.Tools != nil && !*b.caps.Tools
	}
	fmt.Fprintf(&key, "%s\x00%v\x00%v\x00%v", b.req.Model, b.hasStore, noImages, noTools)
	return key.String()
}

// adapt is history as preparation adapts it to the model before projection.
func (b batchBudget) adapt(history []messages.ChatMessage) []messages.ChatMessage {
	if b.caps == nil || !(omitsImages(*b.caps) || (b.caps.Tools != nil && !*b.caps.Tools)) {
		return history
	}
	prepared, _, err := PrepareCapabilities(&CompletionRequest{Model: b.req.Model, Messages: history}, *b.caps, false)
	if err != nil {
		return history
	}
	return prepared.Messages
}

// tokens is what msg costs a request once adapted to the model.
func (b batchBudget) tokens(msg messages.ChatMessage) int {
	return b.agentTools.estimate(b.adapt([]messages.ChatMessage{msg})[0])
}

// room is what the next request with tools, and a finishing request, have
// left over the durable history. Recall results after its last assistant
// message are unread, so the next request carries them whole.
func (b batchBudget) room(history []messages.ChatMessage) (next, finish int) {
	budget := b.req.MaxContextTokens
	history = b.adapt(history)
	unread := lastIndex(history, isAssistant) + 1
	next = budget - b.nextSchemas - b.sandbox - contextFloorMessageTokens(history, b.agentTools, b.hasStore, unread)
	finish = budget - finishTokens(history, b.agentTools, b.finishSchemas)
	return next, finish
}

// roomWith is room for prefix with extra appended: prefix as the memo
// measured it, and extra measured now. The active exchange starts at the
// last real user message of the two, and the unread results follow the last
// assistant message; where extra holds neither, prefix's stand.
func (b batchBudget) roomWith(prefix, extra []messages.ChatMessage) (next, finish int) {
	if b.memo == nil || slices.ContainsFunc(extra, func(msg messages.ChatMessage) bool { return msg.Role == messages.MessageRoleSystem }) {
		return b.room(append(prefix[:len(prefix):len(prefix)], extra...))
	}
	budget := b.req.MaxContextTokens
	marker := projectionMarker(b.hasStore && b.agentTools.artifactsListable && b.agentTools.artifactsReadable, b.agentTools.transcriptReadable)
	next = budget - b.nextSchemas - b.sandbox - systemTokens(prefix, marker)
	finish = budget - b.finishSchemas - systemTokens(prefix, projectionMarker(true, true))
	extra = b.adapt(extra)
	start, unread := lastIndex(extra, isRealUser), lastIndex(extra, isAssistant)+1
	if start < 0 {
		from := lastIndex(prefix, isRealUser)
		if from < 0 {
			// No exchange anywhere: the system messages are all.
			return next, finish
		}
		read := len(prefix)
		if unread == 0 {
			read = lastIndex(prefix, isAssistant) + 1
		}
		for i, c := range b.memo.measured(b, prefix)[from:] {
			if i+from < read {
				next -= c.read
			} else {
				next -= c.unread
			}
			finish -= c.finish
		}
		start = 0
	}
	for j := start; j < len(extra); j++ {
		if msg := extra[j]; msg.Role != messages.MessageRoleInternal {
			next -= floorTokens(msg, b.agentTools, b.hasStore, j >= unread)
			finish -= b.agentTools.estimate(finalForm(msg, b.agentTools.recall))
		}
	}
	return next, finish
}

// fits reports whether the run can commit history with extra appended: the
// next request with tools, and a finishing request, both have room for it.
func (b batchBudget) fits(history []messages.ChatMessage, extra ...messages.ChatMessage) bool {
	next, finish := b.roomWith(history, extra)
	return next >= 0 && finish >= 0
}

// batchPlan is the plan for one tool batch.
type batchPlan struct {
	// finish drops the batch: the next model call finishes the run.
	finish bool
	// bytes bounds each page the batch's tools return; zero leaves the
	// default bound and marks a run without a budget.
	bytes int
	// refused holds the calls not run, by ID.
	refused map[string]bool
	// truncate bounds ordinary results to bytes, for a run without a store.
	truncate bool
	// outer is the page bound in force before the plan lowered it.
	outer int
}

func callResult(call messages.ChatMessageToolCall, content string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: content}
}

// finishedResultTokens is what call's result, content exactly, costs a
// finishing request, which carries tool results as text.
func finishedResultTokens(call messages.ChatMessageToolCall, content string) int {
	result := callResult(call, content)
	flattenToolExchange(&result)
	return estimateProjectedMessageTokens(result)
}

// planBatch plans the batch response calls for, answering the durable
// history before it.
func planBatch(b batchBudget, before []messages.ChatMessage, response *messages.ChatMessage) batchPlan {
	if b.req.MaxContextTokens <= 0 || response == nil || len(response.ToolCalls) == 0 {
		return batchPlan{}
	}
	room, finishRoom := b.roomWith(before, []messages.ChatMessage{*response})

	// Every call is charged up front for the least it can leave: a refusal
	// if it does not run, a note if its result does not fit once it has.
	// The costs depend only on the call's name and the length of its ID, so
	// a batch of like calls is measured once.
	type cost struct {
		reserve, finishReserve int
		least, finishLeast     int
		page                   bool
		envelope               int
	}
	type costKey struct {
		name  string
		idLen int
	}
	measured := make(map[costKey]cost)
	costs := make([]cost, len(response.ToolCalls))
	for i, call := range response.ToolCalls {
		key := costKey{call.Name, len(call.ID)}
		c, ok := measured[key]
		if !ok {
			refusal, dropped := callResult(call, refusalText(call.Name)), callResult(call, droppedText(call.Name))
			c = cost{
				reserve:       max(b.tokens(refusal), b.tokens(dropped)),
				finishReserve: max(b.agentTools.estimate(finalForm(refusal, b.agentTools.recall)), b.agentTools.estimate(finalForm(dropped, b.agentTools.recall))),
				envelope:      b.tokens(callResult(call, "")) + pageNoteTokens,
			}
			// A finished result is at most its stub or a note of its size,
			// which is longest for the largest size, as a finishing request
			// carries it: written as text.
			c.finishLeast = finishedResultTokens(call, omittedToolOutput(1<<40))
			stub, isRecall := b.agentTools.recall.stub(call.Name)
			if isRecall {
				c.finishLeast = max(c.finishLeast, finishedResultTokens(call, stub))
			}
			c.page = isRecall || !b.hasStore
			if !c.page {
				c.least = b.tokens(callResult(call, artifactReceipt("tool output", receiptReserveRef)))
			}
			measured[key] = c
		}
		costs[i] = c
		room -= c.reserve
		finishRoom -= c.finishReserve
	}
	if room < 0 || finishRoom < 0 {
		return batchPlan{finish: true}
	}

	plan := batchPlan{truncate: !b.hasStore}
	refuse := func(call messages.ChatMessageToolCall) bool {
		if call.Name == b.responseTool {
			return false
		}
		if plan.refused == nil {
			plan.refused = make(map[string]bool)
		}
		plan.refused[call.ID] = true
		return true
	}

	// Calls that shrink to receipts run in order while they fit, the
	// response tool first.
	order := make([]int, 0, len(costs))
	for i, call := range response.ToolCalls {
		if call.Name == b.responseTool {
			order = append([]int{i}, order...)
		} else {
			order = append(order, i)
		}
	}
	var pages []int
	for _, i := range order {
		c, call := costs[i], response.ToolCalls[i]
		if c.page {
			pages = append(pages, i)
			continue
		}
		extra, finishExtra := max(0, c.least-c.reserve), max(0, c.finishLeast-c.finishReserve)
		if extra <= room && finishExtra <= finishRoom {
			room -= extra
			finishRoom -= finishExtra
		} else if !refuse(call) {
			return batchPlan{finish: true}
		}
	}

	// Paged calls run as many as each get a useful page, or one alone with
	// whatever page fits. An image a read returns is carried whole.
	// need is what each paged call needs to run: a floor-sized page, or the
	// least page when it runs alone, and an image whole when it returns one.
	need := make([]int, len(pages))
	alone := make([]int, len(pages))
	for p, i := range pages {
		c := costs[i]
		need[p], alone[p] = c.envelope+pageFloorBytes/4, c.envelope+tools.PageMinBytes/4
		if b.imageRecall != nil && b.imageRecall(response.ToolCalls[i]) {
			image := c.envelope + messages.EstimatedImageTokens + estimatedStringTokens(artifactReceipt("tool output", receiptReserveRef))
			need[p], alone[p] = max(need[p], image), max(alone[p], image)
		}
	}
	running, envelope := 0, 0
	for k := len(pages); k > 0 && running == 0; k-- {
		extra, finishExtra := 0, 0
		for p, i := range pages[:k] {
			page := need[p]
			if k == 1 {
				page = alone[p]
			}
			extra += max(0, page-costs[i].reserve)
			finishExtra += max(0, costs[i].finishLeast-costs[i].finishReserve)
		}
		if extra <= room && finishExtra <= finishRoom {
			running = k
			room -= extra
			for _, i := range pages[:k] {
				envelope = max(envelope, costs[i].envelope)
			}
		}
	}
	for _, i := range pages[running:] {
		if !refuse(response.ToolCalls[i]) {
			return batchPlan{finish: true}
		}
	}
	if len(plan.refused) == len(response.ToolCalls) {
		// Nothing would run: committing only refusals spends a model call to
		// be told so, and the history they add leaves less room still.
		return batchPlan{finish: true}
	}
	page := tools.PageMinBytes/4 + envelope
	if running > 0 {
		k := running
		if k > 1 {
			page = pageFloorBytes/4 + envelope
		}
		page += room / k
	} else {
		// No paged call runs; other pagers still page into what is left.
		page = room
	}
	ceiling := max(tools.PageMinBytes, min(tools.PageMaxBytes, b.inlineTokens*4))
	plan.bytes = min(ceiling, max(tools.PageMinBytes, (page-envelope)*4))
	return plan
}

// fitResults makes the results a batch left fit the room planBatch measured:
// until they do, it replaces the result whose note saves the most room with a
// note of what the plan charged for it. A result the projection can already
// demote to a receipt saves next to nothing and is kept, and a call that did
// not run (denied, refused, interrupted) keeps its result, which says so. The
// notes always fit a finishing request: the plan charged every call at least
// as much. They fit the next request with tools too, unless the batch added
// tools whose schemas the room cannot hold; then the next request finishes,
// only its room is made, and fitResults reports false. history ends with the
// response the results answer.
func fitResults(b batchBudget, history, results []messages.ChatMessage) ([]messages.ChatMessage, bool) {
	if b.req == nil || b.req.MaxContextTokens <= 0 {
		return results, true
	}
	room := func(results []messages.ChatMessage) (next, finish int) {
		return b.roomWith(history, results)
	}
	// What replacing a result saves in the next request, where it is unread,
	// and in a finishing one; room measures the two the same way.
	notes := make([]messages.ChatMessage, len(results))
	nextSaves, finishSaves := make([]int, len(results)), make([]int, len(results))
	for i, result := range results {
		note := result.Clone()
		note.Content = droppedText(note.ToolName)
		note.Parts = slices.DeleteFunc(note.Parts, func(part messages.ContentPart) bool { return part.Artifact == nil })
		notes[i] = note
		if result.Content == ToolDeniedContent || result.Content == ToolInterruptedContent || IsToolRefusal(result) {
			continue
		}
		adapted, adaptedNote := b.adapt([]messages.ChatMessage{result})[0], b.adapt([]messages.ChatMessage{note})[0]
		nextSaves[i] = floorTokens(adapted, b.agentTools, b.hasStore, true) - floorTokens(adaptedNote, b.agentTools, b.hasStore, true)
		finishSaves[i] = b.agentTools.estimate(finalForm(adapted, b.agentTools.recall)) - b.agentTools.estimate(finalForm(adaptedNote, b.agentTools.recall))
	}
	// When not even every note would make room for the next request's
	// schemas, the next request finishes without them: only its room counts.
	noted := slices.Clone(results)
	for i := range noted {
		if nextSaves[i]+finishSaves[i] > 0 {
			noted[i] = notes[i]
		}
	}
	nextNoted, _ := room(noted)
	finishOnly := nextNoted < 0
	saves := func(i int) int {
		if finishOnly {
			return finishSaves[i]
		}
		return nextSaves[i] + finishSaves[i]
	}
	dropped := make([]bool, len(results))
	for {
		next, finish := room(results)
		if finish >= 0 && (next >= 0 || finishOnly) {
			return results, !finishOnly
		}
		// The response tool's result is replaced last.
		best := -1
		for i, result := range results {
			if dropped[i] || saves(i) <= 0 {
				continue
			}
			if best < 0 {
				best = i
				continue
			}
			bestResponse, response := results[best].ToolName == b.responseTool, result.ToolName == b.responseTool
			if bestResponse && !response || bestResponse == response && saves(i) > saves(best) {
				best = i
			}
		}
		if best < 0 {
			return results, false
		}
		dropped[best] = true
		results[best] = notes[best]
	}
}

type batchPlanKey struct{}

// withBatchPlan bounds the pages of tools run under ctx by plan and carries
// the calls it refuses.
func withBatchPlan(ctx context.Context, plan batchPlan) context.Context {
	if plan.bytes == 0 {
		return ctx
	}
	plan.outer = tools.PageBytes(ctx)
	ctx = tools.WithPageBytes(ctx, plan.bytes)
	return context.WithValue(ctx, batchPlanKey{}, plan)
}

func batchPlanFrom(ctx context.Context) (batchPlan, bool) {
	plan, ok := ctx.Value(batchPlanKey{}).(batchPlan)
	return plan, ok
}

// withoutBatchPlan is ctx without the plan of an enclosing agent's batch, and
// with the page bound in force before that plan lowered it. A tool of that
// batch can start another agent run, whose requests and pages are its own.
func withoutBatchPlan(ctx context.Context) context.Context {
	plan, ok := batchPlanFrom(ctx)
	if !ok {
		return ctx
	}
	return tools.RestorePageBytes(context.WithValue(ctx, batchPlanKey{}, nil), plan.outer)
}

// boundPage keeps a result's text within the batch's page bound when it must
// be: a recall tool that did not page itself, or any tool in a run without a
// store, whose results cannot shrink to receipts later.
func boundPage(ctx context.Context, text string, recall bool) string {
	plan, ok := batchPlanFrom(ctx)
	if !ok || !(recall || plan.truncate) || len(text) <= plan.bytes {
		return text
	}
	note := fmt.Sprintf("\n[output truncated to fit the context budget; %d bytes in all]", len(text))
	return tools.CapPageTextContext(ctx, text[:max(0, plan.bytes-len(note))]) + note
}
