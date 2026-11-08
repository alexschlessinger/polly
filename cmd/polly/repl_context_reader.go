package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// Capture only the read inputs used by statistics/reporting, never a copy of
// the conversation's locks, atomics, or live mutable settings container.
func contextReadState(s *conversationState) *conversationState {
	if s == nil {
		return nil
	}
	view := &conversationState{
		session: s.session, settings: s.settings.clone(), agent: s.agent, swarm: s.swarm,
		toolRegistry: s.toolRegistry, skillCatalog: s.skillCatalog,
		displayContract: s.displayContract, sandboxProfile: s.sandboxProfile,
		metadataBaseURL: s.metadataBaseURL,
	}
	// sizedBudget values are immutable once published; copy the observation,
	// not the atomic container, to preserve provider-learned budget reporting.
	if sized := s.sized.Load(); sized != nil {
		copy := *sized
		view.sized.Store(&copy)
	}
	return view
}

// One background reader owns these caches for an open popover. Snapshots copy
// the small display slices; subsequent reads cannot mutate a published result.
// Native unchanged polls read an empty tail page, not retained transcript data.
// Legacy session wrappers explicitly fall back to GetHistory, off the UI thread.
type contextSnapshotReader struct {
	history   []messages.ChatMessage
	epoch     string
	stored    contextSnapshot
	cache     turnCacheUsage
	compacted bool
	request   contextRequestCounts
	usage     contextSessionUsage
	revisions []sessions.Revision
	restored  bool
	// Set only before dispatching a read. Transcript statistics keep their
	// existing clock; only coordination usage is gated by scoped hints.
	skipUsage        bool
	usageSourceError error
}

// At most one bounded cache belongs to each open conversation. It deliberately
// retains no raw history. A changed transcript on reopen requires reconstruction;
// an unchanged one only rechecks its epoch/end, tools and dynamic system prefix.
type contextSnapshotCache struct {
	epoch     string
	stored    contextSnapshot
	cache     turnCacheUsage
	compacted bool
	request   contextRequestCounts
	usage     contextSessionUsage
	revisions []sessions.Revision
}

func newContextSnapshotReader(cache *contextSnapshotCache) *contextSnapshotReader {
	if cache == nil {
		return &contextSnapshotReader{}
	}
	return &contextSnapshotReader{epoch: cache.epoch, stored: cache.stored, cache: cache.cache, compacted: cache.compacted,
		request: cache.request, usage: cache.usage, revisions: cache.revisions, restored: true}
}

func (r *contextSnapshotReader) reopenCache() *contextSnapshotCache {
	if r.epoch == "" || len(r.epoch) > 1024 || !r.request.valid || len(r.request.toolsKey) > 16<<10 {
		return nil
	}
	// The request's first system message may contain arbitrary metadata/media.
	// Clone its persisted representation with a hard retained-byte bound rather
	// than pinning nested buffers from the full transcript after closing.
	first, err := json.Marshal(r.request.first)
	if err != nil || len(first) > 64<<10 {
		return nil
	}
	request := r.request
	request.first = nil
	if err := json.Unmarshal(first, &request.first); err != nil {
		return nil
	}
	cache := &contextSnapshotCache{epoch: r.epoch, stored: r.stored, cache: r.cache, compacted: r.compacted, request: request}
	cache.stored.trend = slices.Clone(cache.stored.trend)
	if len(r.usage.agents) <= 128 && len(r.revisions) <= 3 {
		bytes := 0
		for _, agent := range r.usage.agents {
			bytes += len(agent.id) + len(agent.label)
		}
		for _, revision := range r.revisions {
			bytes += len(revision.Token) + len(revision.Scope.Key) + len(revision.Scope.Index)
		}
		if bytes <= 64<<10 {
			cache.usage = r.usage
			cache.usage.agents = slices.Clone(r.usage.agents)
			cache.revisions = slices.Clone(r.revisions)
		}
	}
	return cache
}

func (r *contextSnapshotReader) read(ctx context.Context, config *Config, state *conversationState) (contextSnapshot, error) {
	if native, ok := state.session.(sessions.SessionTranscriptReader); ok {
		if r.restored {
			after := int64(r.request.through)
			page, err := native.ReadTranscriptPage(ctx, sessions.TranscriptQuery{Epoch: r.epoch, After: after, Limit: 1})
			if err != nil && !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
				return contextSnapshot{}, err
			}
			if err == nil && page.Epoch == r.epoch && page.First == after && page.Next == after && page.Through == after && len(page.Messages) == 0 && r.request.toolsKey == contextProjectionToolsKey(state.viewTools()) {
				return r.snapshot(ctx, config, state)
			}
			r.resetHistory()
		}
		if err := r.readPages(ctx, native); err != nil {
			return contextSnapshot{}, err
		}
	} else {
		history, err := state.session.GetHistory(ctx)
		if err != nil {
			return contextSnapshot{}, err
		}
		r.resetHistory()
		r.appendHistory(history)
	}
	return r.snapshot(ctx, config, state)
}

func (r *contextSnapshotReader) resetHistory() {
	r.history, r.epoch, r.restored = nil, "", false
	r.stored, r.cache, r.compacted, r.request = contextSnapshot{}, turnCacheUsage{}, false, contextRequestCounts{}
}

func (r *contextSnapshotReader) readPages(ctx context.Context, reader sessions.SessionTranscriptReader) error {
	through := int64(0)
	restarted := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		after := int64(len(r.history))
		page, err := reader.ReadTranscriptPage(ctx, sessions.TranscriptQuery{Epoch: r.epoch, After: after, Through: through, Limit: 128})
		if errors.Is(err, sessions.ErrStaleTranscriptEpoch) && !restarted {
			r.resetHistory()
			through, restarted = 0, true
			continue
		}
		if err != nil {
			return err
		}
		if page.Epoch == "" || page.First != after || page.Next != after+int64(len(page.Messages)) || page.Next > page.Through || page.Next == after && page.Next != page.Through {
			return fmt.Errorf("invalid context transcript page")
		}
		r.epoch = page.Epoch
		r.appendHistory(page.Messages)
		if page.Next == page.Through {
			return nil
		}
		through = page.Through
	}
}

func (r *contextSnapshotReader) appendHistory(history []messages.ChatMessage) {
	const trendLimit = 32
	for _, msg := range history {
		r.cache.add(msg)
		r.stored.usage.parent.addMessage(msg)
		r.stored.storedTokens += sessions.EstimateTokens(msg)
		r.stored.storedMessages++
		if c, ok := msg.Compaction(); ok {
			if c.Summary != "" {
				r.stored.summaries++
			} else {
				r.stored.clears++
			}
			r.compacted = true
		}
		if input := msg.GetInputTokens(); msg.Role == messages.MessageRoleAssistant && input > 0 {
			if len(r.stored.trend) == trendLimit {
				copy(r.stored.trend, r.stored.trend[1:])
				r.stored.trend = r.stored.trend[:trendLimit-1]
			}
			r.stored.trend = append(r.stored.trend, contextTrendPoint{tokens: input, compacted: r.compacted})
			r.compacted = false
		}
	}
	r.history = append(r.history, history...)
}

func (r *contextSnapshotReader) snapshot(ctx context.Context, config *Config, state *conversationState) (contextSnapshot, error) {
	snapshot := r.stored
	snapshot.trend = slices.Clone(snapshot.trend)
	snapshot.cache = "unknown"
	if field, ok := turnCacheField(r.cache); ok {
		snapshot.cache = field.raw
		snapshot.cacheFraction = float64(r.cache.read) / float64(r.cache.input)
		snapshot.cacheKnown = true
	}
	if state.swarm != nil {
		if !r.skipUsage {
			usage, err := state.swarm.ReadDisplayUsage(ctx, r.revisions)
			r.usage.err = err
			if err == nil && !usage.Unchanged {
				r.usage = contextSessionUsage{}
				r.usage.addDisplayUsage(usage.Members)
				r.revisions = usage.Revisions
			}
		}
		snapshot.usage.subagents, snapshot.usage.agents = r.usage.subagents, r.usage.agents
		snapshot.usage.err = r.usage.err
		if r.usageSourceError != nil {
			snapshot.usage.err = r.usageSourceError
		}
		if snapshot.usage.err != nil {
			snapshot.usage.subagents.input.missing = true
			snapshot.usage.subagents.output.missing = true
		}
	}
	var err error
	if r.restored {
		snapshot.roles, err = r.request.composedRoles(ctx, config, state)
	} else {
		snapshot.roles, err = r.request.roles(ctx, config, state, r.history)
	}
	return snapshot, err
}

// The request composers only change/prepend the first system message. Retain
// the other role counts so metadata, skills, repository guidance and sandbox
// permissions can be refreshed exactly without rescanning the transcript.
// Ordinary appends are counted incrementally. Compactions, changed projection
// tools, and the first real user after a summary use canonical RequestView again:
// that boundary moves the summary's working images back into referable history.
// Those boundary reads still scan retained history; unchanged polls and ordinary
// appends do not. KeepsTurn, image retention and recall stubs are never estimated
// by a separate approximation of the model's request projection.
type contextRequestCounts struct {
	valid          bool
	through        int
	toolsKey       string
	messages       int
	first          []messages.ChatMessage
	base           [4]contextRoleUsage
	summaryWaiting bool
}

func contextRoles() [4]contextRoleUsage {
	return [4]contextRoleUsage{{role: messages.MessageRoleUser}, {role: messages.MessageRoleAssistant}, {role: messages.MessageRoleTool}, {role: messages.MessageRoleSystem}}
}

func addContextRole(roles []contextRoleUsage, msg messages.ChatMessage) {
	for i := range roles {
		if roles[i].role == msg.Role {
			roles[i].count++
			roles[i].tokens += llm.EstimateMessageTokens(msg)
			return
		}
	}
}

func contextProjectionToolsKey(list []tools.Tool) string {
	var keys []string
	for _, tool := range list {
		stub, recall := tools.RecallStub(tool)
		keys = append(keys, fmt.Sprintf("%q:%t:%q", tool.GetName(), recall, stub))
	}
	slices.Sort(keys)
	return strings.Join(keys, "\n")
}

// This is the same real-user boundary used by llm.RequestView, not the display
// transcript's broader synthetic/admission classification.
func contextRequestRealUser(msg messages.ChatMessage) bool {
	synthetic, _ := msg.Metadata[messages.MetadataKeyAgentSynthetic].(bool)
	return msg.Role == messages.MessageRoleUser && !synthetic
}

func (p *contextRequestCounts) roles(ctx context.Context, config *Config, state *conversationState, history []messages.ChatMessage) ([]contextRoleUsage, error) {
	list := state.viewTools()
	key := contextProjectionToolsKey(list)
	rebuild := !p.valid || p.toolsKey != key || p.through > len(history)
	if p.through < len(history) {
		for _, msg := range history[p.through:] {
			_, compacted := msg.Compaction()
			if compacted || p.summaryWaiting && contextRequestRealUser(msg) {
				rebuild = true
				break
			}
		}
	}
	if rebuild {
		*p = contextRequestCounts{base: contextRoles(), toolsKey: key, valid: true}
		for _, msg := range history {
			if c, ok := msg.Compaction(); ok && c.Summary != "" {
				p.summaryWaiting = true
			} else if contextRequestRealUser(msg) {
				p.summaryWaiting = false
			}
		}
	}
	if rebuild || p.through < len(history) {
		for _, msg := range llm.RequestView(history[p.through:], list) {
			if p.messages == 0 && msg.Role == messages.MessageRoleSystem {
				p.first = []messages.ChatMessage{msg}
			} else {
				addContextRole(p.base[:], msg)
			}
			p.messages++
		}
		p.through = len(history)
	}
	return p.composedRoles(ctx, config, state)
}

func (p *contextRequestCounts) composedRoles(ctx context.Context, config *Config, state *conversationState) ([]contextRoleUsage, error) {
	prefix := slices.Clone(p.first)
	var err error
	if config == nil || config.SchemaPath == "" {
		prefix, _, err = composeSessionContracts(ctx, state, &state.settings, prefix)
		if err != nil {
			return nil, err
		}
	}
	req := llm.CompletionRequest{Messages: prefix, Skills: state.skillCatalog}
	prefix, err = llm.WithSandboxContext(req.ResolvedMessages(), state.toolRegistry)
	if err != nil {
		return nil, err
	}
	roles := slices.Clone(p.base[:])
	for _, msg := range prefix {
		addContextRole(roles, msg)
	}
	return roles, nil
}
