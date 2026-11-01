package swarm

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

const (
	liveTranscriptMessages = 128
	liveTranscriptBytes    = 256 << 10
	liveMessageBytes       = 32 << 10
	liveMessageItems       = 64
	liveSettledTranscripts = 16
)

// LiveTranscript is a bounded, display-only tail, never model history or replay
// authority. Read it BEFORE ReadView and append Messages only when Ready,
// SessionID/Execution/Generation match and view.Sequence == BaseSequence. A
// mismatch requires a fresh saved view, not concatenating arbitrary intent.
// Epoch changes on every callback binding, including a resumed execution slice.
// Revision changes without waking agents; displays may coalesce polling ticks.
// Truncated requires a visible omission marker. Incomplete marks non-durable
// output retained at settlement, which must never look like a committed answer.
// The last message matching ProvisionalDisplayID is still a streaming attempt.
// All returned messages are independent copies with no inline binary payloads.
// The tail keeps at most 128 messages and 256 KiB of strings; each message has
// at most 32 KiB, 64 calls/parts/text blocks, and 8 KiB per payload string.
// Identities are capped at 256 bytes and reserved before payloads. Artifact
// parts whose complete handle cannot fit are omitted, never kept with a clipped
// handle. Eviction prefers old messages other than the latest tool declaration.
type LiveTranscript struct {
	SessionID, Execution                  string
	Generation                            int
	Epoch, Revision                       uint64
	BaseSequence                          int64
	Ready, Settled, Incomplete, Truncated bool
	ProvisionalDisplayID                  string
	Messages                              []messages.ChatMessage
}

type liveTranscriptEntry struct {
	index   int // raw generated index; -1 for an immediate tool result
	message messages.ChatMessage
	bytes   int
	pinned  bool // latest tool declaration survives ordinary tail eviction
}

type liveTranscript struct {
	mu                 sync.Mutex
	snapshot           LiveTranscript
	acked              int
	entries            []liveTranscriptEntry
	provisional        *messages.ChatMessage
	provisionalBytes   int
	iteration, attempt int
}

type transcriptBinding struct {
	live  *liveTranscript
	epoch uint64
}

// LiveTranscript neither acquires a session lease nor changes execution
// ownership. Finished invocations retain at most 16 bounded, frozen snapshots;
// a newer invocation supersedes the old one. A reader seeing disappearance or
// settlement must refresh saved history before retiring its previous frame.
func (r *Runtime) LiveTranscript(memberID string) (LiveTranscript, bool) {
	r.mu.Lock()
	var live *liveTranscript
	if i := r.active[memberID]; i != nil {
		live = &i.transcript
	} else {
		live = r.settledTranscripts[memberID]
	}
	r.mu.Unlock()
	if live == nil {
		return LiveTranscript{}, false
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.snapshot.Epoch == 0 {
		return LiveTranscript{}, false
	}
	out := live.snapshot
	out.Messages = make([]messages.ChatMessage, 0, len(live.entries)+1)
	for _, entry := range live.entries {
		out.Messages = append(out.Messages, entry.message.Clone())
	}
	if live.provisional != nil {
		out.ProvisionalDisplayID = live.provisional.DisplayID()
		out.Messages = append(out.Messages, live.provisional.Clone())
	}
	return out, true
}

// Called under r.mu, before removing the active invocation. It retains the
// final handoff even when the last polling tick preceded the final checkpoint.
func (r *Runtime) settleTranscript(i *invocation) {
	live := &i.transcript
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.snapshot.Epoch == 0 {
		return
	}
	live.snapshot.Settled = true
	live.snapshot.Incomplete = len(live.entries) != 0 || live.provisional != nil
	live.snapshot.Revision++
	if r.settledTranscripts == nil {
		r.settledTranscripts = make(map[string]*liveTranscript)
	}
	r.settledOrder = slices.DeleteFunc(r.settledOrder, func(id string) bool { return id == i.member })
	r.settledOrder = append(r.settledOrder, i.member)
	// Detach from the invocation allocation without copying the mutex. Settled
	// callbacks cannot mutate the retained messages, and readers clone them.
	r.settledTranscripts[i.member] = &liveTranscript{
		snapshot:    live.snapshot,
		entries:     live.entries,
		provisional: live.provisional,
	}
	for len(r.settledOrder) > liveSettledTranscripts {
		delete(r.settledTranscripts, r.settledOrder[0])
		r.settledOrder = r.settledOrder[1:]
	}
}

func (i *invocation) bindTranscript(cb *llm.AgentCallbacks, sessionID string) *transcriptBinding {
	live := &i.transcript
	live.mu.Lock()
	epoch := live.snapshot.Epoch + 1
	live.snapshot = LiveTranscript{SessionID: sessionID, Execution: i.id, Generation: i.generation, Epoch: epoch, Revision: 1}
	live.acked, live.entries, live.provisional, live.provisionalBytes = 0, nil, nil, 0
	live.mu.Unlock()
	binding := &transcriptBinding{live: live, epoch: epoch}
	update := func(fn func()) {
		live.mu.Lock()
		defer live.mu.Unlock()
		if !binding.current() {
			return
		}
		fn()
		live.bound()
		live.snapshot.Revision++
	}
	bindAbortObserver(cb, func(iteration, attempt int, _ error) {
		update(func() {
			if live.iteration == iteration && live.attempt == attempt {
				live.clearProvisional()
			}
		})
	})
	request := cb.OnModelRequest
	cb.OnModelRequest = func(iteration, attempt int) {
		update(func() {
			live.iteration, live.attempt = iteration, attempt
			live.clearProvisional()
		})
		if request != nil {
			request(iteration, attempt)
		}
	}
	start := cb.OnMessageStart
	cb.OnMessageStart = func(id string) {
		update(func() {
			live.clearProvisional()
			msg, size, truncated := boundedLiveMessage(messages.ChatMessage{Role: messages.MessageRoleAssistant, Metadata: map[string]any{messages.MetadataKeyDisplayID: id}})
			live.provisional, live.provisionalBytes = &msg, size
			live.snapshot.Truncated = live.snapshot.Truncated || truncated
		})
		if start != nil {
			start(id)
		}
	}
	text := func(chunk string, apply func(*messages.ChatMessage, string)) {
		if chunk == "" {
			return
		}
		update(func() {
			if live.provisional == nil {
				return
			}
			// Bound before concatenation, so a huge provider chunk cannot create
			// an unbounded retained string (or retain its backing allocation).
			remaining := max(0, liveMessageBytes-live.provisionalBytes)
			if len(chunk) > remaining {
				chunk = livePrefix(chunk, remaining)
				live.snapshot.Truncated = true
			}
			apply(live.provisional, chunk)
			msg, size, truncated := boundedLiveMessage(*live.provisional)
			live.provisional, live.provisionalBytes = &msg, size
			live.snapshot.Truncated = live.snapshot.Truncated || truncated
		})
	}
	content := cb.OnContent
	cb.OnContent = func(chunk string) {
		text(chunk, func(m *messages.ChatMessage, chunk string) {
			m.Content += chunk
			if n := len(m.TextBlocks); n != 0 && m.TextBlocks[n-1].Phase == messages.PhaseFinalAnswer {
				m.TextBlocks[n-1].Text += chunk
			} else {
				m.TextBlocks = append(m.TextBlocks, messages.AssistantText{Phase: messages.PhaseFinalAnswer, Text: chunk})
			}
		})
		if content != nil {
			content(chunk)
		}
	}
	reasoning := cb.OnReasoning
	cb.OnReasoning = func(chunk string) {
		text(chunk, func(m *messages.ChatMessage, chunk string) { m.Reasoning += chunk })
		if reasoning != nil {
			reasoning(chunk)
		}
	}
	commentary := cb.OnCommentary
	cb.OnCommentary = func(block messages.AssistantText, start bool) {
		text(block.Text, func(m *messages.ChatMessage, chunk string) {
			if n := len(m.TextBlocks); !start && n != 0 && m.TextBlocks[n-1].ID == block.ID && m.TextBlocks[n-1].Phase == messages.PhaseCommentary {
				m.TextBlocks[n-1].Text += chunk
			} else {
				m.TextBlocks = append(m.TextBlocks, messages.AssistantText{ID: block.ID, Phase: messages.PhaseCommentary, Text: chunk})
			}
		})
		if commentary != nil {
			commentary(block, start)
		}
	}
	accepted := cb.OnMessage
	cb.OnMessage = func(index int, msg messages.ChatMessage) {
		update(func() {
			// Admitted input is checkpointed BEFORE Agent appends/observes it.
			if index < live.acked {
				return
			}
			if live.provisional != nil && msg.DisplayID() == live.provisional.DisplayID() {
				live.clearProvisional()
			}
			live.put(index, msg)
		})
		if accepted != nil {
			accepted(index, msg)
		}
	}
	result := cb.OnToolResult
	cb.OnToolResult = func(call messages.ChatMessageToolCall, msg messages.ChatMessage) {
		update(func() { live.put(-1, msg) })
		if result != nil {
			result(call, msg)
		}
	}
	return binding
}

func (b *transcriptBinding) current() bool {
	return b.live.snapshot.Epoch == b.epoch && !b.live.snapshot.Settled
}

// checkpoint holds the display mutex through the durable transaction AND its
// acknowledgement. No reader can observe a new base with an old live suffix.
func (b *transcriptBinding) checkpoint(c llm.AgentCheckpoint, commit func() (int64, error)) error {
	if b == nil {
		_, err := commit()
		return err
	}
	live := b.live
	live.mu.Lock()
	defer live.mu.Unlock()
	if !b.current() {
		return errors.New("live transcript checkpoint was fenced")
	}
	if len(c.Generated) < live.acked {
		return errors.New("live transcript checkpoint prefix regressed")
	}
	next, err := commit()
	if err != nil {
		return err
	}
	live.snapshot.BaseSequence = next
	live.snapshot.Ready = live.snapshot.Ready || c.Request
	ids := make(map[string]bool)
	for _, msg := range c.Generated[live.acked:] {
		if id := msg.DisplayID(); id != "" {
			ids[id] = true
		}
	}
	live.acked = len(c.Generated)
	live.entries = slices.DeleteFunc(live.entries, func(e liveTranscriptEntry) bool {
		return e.index >= 0 && e.index < live.acked || ids[e.message.DisplayID()]
	})
	if live.provisional != nil && ids[live.provisional.DisplayID()] {
		live.clearProvisional()
	}
	if len(live.entries) == 0 && live.provisional == nil {
		live.snapshot.Truncated = false
	}
	live.snapshot.Revision++
	return nil
}

func (live *liveTranscript) clearProvisional() {
	live.provisional, live.provisionalBytes = nil, 0
}

func (live *liveTranscript) put(index int, input messages.ChatMessage) {
	msg, size, truncated := boundedLiveMessage(input)
	live.snapshot.Truncated = live.snapshot.Truncated || truncated
	if msg.Role == messages.MessageRoleAssistant {
		for n := range live.entries {
			live.entries[n].pinned = false
		}
	}
	entry := liveTranscriptEntry{index: index, message: msg, bytes: size, pinned: len(msg.ToolCalls) > 0}
	for n, old := range live.entries {
		if index >= 0 && old.index == index || msg.DisplayID() != "" && old.message.DisplayID() == msg.DisplayID() {
			live.entries[n] = entry
			live.order()
			return
		}
	}
	live.entries = append(live.entries, entry)
	live.order()
}

func (live *liveTranscript) order() {
	// Immediate parallel results use declaration order until their canonical
	// generated indices arrive. Provider IDs can repeat or be absent even in
	// one batch; only legacy calls without a host identity use provider IDs.
	type toolIdentity struct {
		host bool
		id   string
	}
	key := func(displayID, providerID string) toolIdentity {
		if displayID != "" {
			return toolIdentity{host: true, id: displayID}
		}
		return toolIdentity{id: providerID}
	}
	order := make(map[toolIdentity]int)
	count := 0
	for _, entry := range live.entries {
		for _, call := range entry.message.ToolCalls {
			id := key(call.DisplayID, call.ID)
			if _, exists := order[id]; !exists && id.id != "" {
				order[id] = count
			}
			count++
		}
	}
	rank := func(msg messages.ChatMessage) int {
		if n, ok := order[key(msg.DisplayID(), msg.ToolCallID)]; ok {
			return n
		}
		return count // unmatched results remain in arrival order after known calls
	}
	sort.SliceStable(live.entries, func(i, j int) bool {
		a, b := live.entries[i], live.entries[j]
		if a.index >= 0 && b.index >= 0 {
			return a.index < b.index
		}
		if a.index >= 0 {
			return true
		}
		if b.index >= 0 {
			return false
		}
		return rank(a.message) < rank(b.message)
	})
}

func (live *liveTranscript) bound() {
	bytes, count := live.provisionalBytes, len(live.entries)
	if live.provisional != nil {
		count++
	}
	for _, entry := range live.entries {
		bytes += entry.bytes
	}
	for count > liveTranscriptMessages || bytes > liveTranscriptBytes {
		n := slices.IndexFunc(live.entries, func(e liveTranscriptEntry) bool { return !e.pinned })
		if n < 0 {
			n = 0
		}
		if len(live.entries) == 0 {
			break
		}
		bytes -= live.entries[n].bytes
		live.entries = slices.Delete(live.entries, n, n+1)
		count--
		live.snapshot.Truncated = true
	}
}

func livePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Only display scalars are retained from metadata; no opaque provider state,
// nested graphs, inline images, or error objects can escape into this buffer.
func boundedLiveMessage(input messages.ChatMessage) (messages.ChatMessage, int, bool) {
	left, truncated := liveMessageBytes, false
	takeLimit := func(s string, cap int) string {
		limit := min(left, cap)
		if len(s) > limit {
			s, truncated = livePrefix(s, limit), true
		}
		left -= len(s)
		return strings.Clone(s)
	}
	take := func(s string) string { return takeLimit(s, 8<<10) }
	identity := func(s string) string { return takeLimit(s, 256) }
	msg := messages.ChatMessage{Metadata: map[string]any{messages.MetadataKeyDisplayID: identity(input.DisplayID())}}
	msg.Role, msg.ToolCallID, msg.ToolName, msg.StopReason = identity(input.Role), identity(input.ToolCallID), identity(input.ToolName), messages.StopReason(identity(string(input.StopReason)))
	// Reserve every primary call identity before provider IDs, names or
	// payloads. At most 64 identities of 256 bytes fit within the message cap;
	// budgeting whole calls in one pass can erase the last calls' host IDs.
	for n, call := range input.ToolCalls {
		if n == liveMessageItems {
			truncated = true
			break
		}
		bounded := messages.ChatMessageToolCall{DisplayID: identity(call.DisplayID)}
		if call.DisplayID == "" {
			bounded.ID = identity(call.ID)
		}
		msg.ToolCalls = append(msg.ToolCalls, bounded)
	}
	// Reserve all artifact handles before any optional descriptors. A clipped
	// ID is not a usable handle: omit the whole part instead, remembering its
	// source index so later payload copying cannot attach text to another ref.
	partSources := make([]int, 0, min(len(input.Parts), liveMessageItems))
	for n, part := range input.Parts {
		if n == liveMessageItems {
			truncated = true
			break
		}
		p := messages.ContentPart{}
		if part.ImageData != "" || part.ImageURL != "" {
			truncated = true
		}
		if ref := part.Artifact; ref != nil {
			if ref.ID == "" || len(ref.ID) > 256 || len(ref.ID) > left {
				truncated = true
				continue
			}
			p.Artifact = &artifacts.Ref{ID: identity(ref.ID), Bytes: ref.Bytes, Lines: ref.Lines}
		}
		msg.Parts = append(msg.Parts, p)
		partSources = append(partSources, n)
	}
	// Keep each part's type ahead of the potentially large human-readable
	// descriptors too, so ordinary artifact kinds survive truncation intact.
	for n, source := range partSources {
		part := input.Parts[source]
		msg.Parts[n].Type = identity(part.Type)
		if part.Artifact != nil {
			msg.Parts[n].Artifact.Kind = artifacts.Kind(identity(string(part.Artifact.Kind)))
		}
	}
	for n, source := range partSources {
		part, p := input.Parts[source], &msg.Parts[n]
		p.MimeType, p.FileName, p.Reference = identity(part.MimeType), identity(part.FileName), identity(part.Reference)
		if ref := part.Artifact; ref != nil {
			p.Artifact.MIMEType, p.Artifact.Name, p.Artifact.ImageToken, p.Artifact.Reference = identity(ref.MIMEType), identity(ref.Name), identity(ref.ImageToken), identity(ref.Reference)
		}
	}
	for n := range msg.ToolCalls {
		if msg.ToolCalls[n].DisplayID != "" {
			msg.ToolCalls[n].ID = identity(input.ToolCalls[n].ID)
		}
		msg.ToolCalls[n].Name = identity(input.ToolCalls[n].Name)
	}
	for _, key := range []string{messages.MetadataKeyIsError, messages.MetadataKeyToolSucceeded, messages.MetadataKeyAgentSynthetic, messages.MetadataKeyThinkingMillis, messages.MetadataKeyToolMillis} {
		switch value := input.Metadata[key].(type) {
		case bool, int, int64, float64:
			msg.Metadata[key] = value
		}
	}
	for n := range msg.ToolCalls {
		msg.ToolCalls[n].Arguments = take(input.ToolCalls[n].Arguments)
	}
	for n, source := range partSources {
		msg.Parts[n].Text = take(input.Parts[source].Text)
	}
	msg.Content, msg.Reasoning = take(input.Content), take(input.Reasoning)
	for n, block := range input.TextBlocks {
		if n == liveMessageItems {
			truncated = true
			break
		}
		msg.TextBlocks = append(msg.TextBlocks, messages.AssistantText{ID: take(block.ID), Phase: messages.AssistantPhase(take(string(block.Phase))), Text: take(block.Text)})
	}
	for _, key := range []string{messages.MetadataKeyDisplayReasoning, messages.MetadataKeyError} {
		if value, ok := input.Metadata[key].(string); ok {
			msg.Metadata[key] = take(value)
		}
	}
	return msg, liveMessageBytes - left, truncated
}
