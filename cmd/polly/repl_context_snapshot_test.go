package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestContextSnapshotTrendOrderingAndLimit(t *testing.T) {
	for _, count := range []int{1, 32, 40} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var history []messages.ChatMessage
			var want []contextTrendPoint
			for i := 0; i < count; i++ {
				compacted := i == count-32
				if compacted {
					history = append(history, messages.Compaction{Summary: "earlier work"}.Message())
				}
				// Deliberately not sorted. A new model can report a much larger
				// input count; neither the ordering nor the scale may change.
				input := 10_000 - i*73
				model := "first/model"
				if i >= 20 {
					input += 128_000
					model = "second/model"
				}
				reply := cacheMessage(input, 0, true)
				reply.SetTokenUsage(input, 900_000)
				reply.Metadata[messages.MetadataKeyRequestEstimateRoute] = model
				history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "continue"}, reply)
				want = append(want, contextTrendPoint{tokens: input, compacted: compacted})
			}
			want = want[max(0, len(want)-32):]
			state := &conversationState{settings: Settings{Model: "second/model", MaxHistoryTokens: 256_000}}
			snapshot, err := collectContextSnapshot(context.Background(), &Config{SchemaPath: "schema.json"}, state, history)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(snapshot.trend, want) {
				t.Fatalf("trend = %+v, want %+v", snapshot.trend, want)
			}
			state.settings.Model, state.settings.MaxHistoryTokens = "third/model", 4_000
			switched, err := collectContextSnapshot(context.Background(), &Config{SchemaPath: "schema.json"}, state, history)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(switched.trend, want) {
				t.Fatalf("model switch changed absolute counts: %+v", switched.trend)
			}
		})
	}
}

func TestContextSnapshotTrendMarksNextReportedRequest(t *testing.T) {
	summary := messages.Compaction{Summary: "earlier work"}.Message()
	clear := messages.Compaction{ClearThrough: "call"}.Message()
	for _, tc := range []struct {
		name              string
		markers           []messages.ChatMessage
		summaries, clears int
	}{
		{"summary", []messages.ChatMessage{summary}, 1, 0},
		{"clear", []messages.ChatMessage{clear}, 0, 1},
		{"consecutive", []messages.ChatMessage{summary, clear, summary}, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := []messages.ChatMessage{cacheMessage(1_000, 0, true)}
			history = append(history, tc.markers...)
			unreported := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "no provider count"}
			unreported.SetRequestEstimate(42_000)
			user := cacheMessage(2_000, 0, true)
			user.Role = messages.MessageRoleUser
			history = append(history,
				unreported, cacheMessage(0, 0, true), cacheMessage(-1, 0, true),
				cacheMessage(8_000, 0, true).UsageRecord(), user,
				messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "call", Content: "result"},
				cacheMessage(600, 0, true), cacheMessage(750, 0, true),
				clear, unreported,
			)
			snapshot, err := collectContextSnapshot(context.Background(), &Config{SchemaPath: "schema.json"}, &conversationState{}, history)
			if err != nil {
				t.Fatal(err)
			}
			want := []contextTrendPoint{{tokens: 1_000}, {tokens: 600, compacted: true}, {tokens: 750}}
			if !reflect.DeepEqual(snapshot.trend, want) {
				t.Fatalf("trend = %+v, want %+v", snapshot.trend, want)
			}
			if snapshot.summaries != tc.summaries || snapshot.clears != tc.clears+1 {
				t.Fatalf("compactions = %d summaries, %d clears", snapshot.summaries, snapshot.clears)
			}
		})
	}
}

func TestContextSnapshotTrendDoesNotInventCounts(t *testing.T) {
	estimated := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: strings.Repeat("answer ", 100)}
	estimated.SetRequestEstimate(4_000)
	tool := cacheMessage(500, 0, true)
	tool.Role = messages.MessageRoleTool
	for _, history := range [][]messages.ChatMessage{
		nil,
		{estimated, cacheMessage(0, 0, false), cacheMessage(-1, 0, false), tool},
		{messages.Compaction{Summary: "earlier work"}.Message(), estimated},
		{cacheMessage(500, 0, true).UsageRecord()},
	} {
		snapshot, err := collectContextSnapshot(context.Background(), &Config{SchemaPath: "schema.json"}, &conversationState{}, history)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.trend) != 0 {
			t.Fatalf("invented trend points for history without reported assistant input: %+v", snapshot.trend)
		}
	}
}

func TestContextSnapshotCacheAccounting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		history  []messages.ChatMessage
		want     string
		fraction float64
	}{
		{"weighted", []messages.ChatMessage{cacheMessage(100, 0, true), cacheMessage(300, 300, true)}, "75% cache hit", 0.75},
		{"internal usage", []messages.ChatMessage{cacheMessage(100, 0, true), cacheMessage(300, 300, true).UsageRecord()}, "75% cache hit", 0.75},
		{"zero", []messages.ChatMessage{cacheMessage(100, 0, true)}, "0% cache hit", 0},
		{"full", []messages.ChatMessage{cacheMessage(100, 100, true)}, "100% cache hit", 1},
		{"missing", []messages.ChatMessage{cacheMessage(100, 100, true), cacheMessage(100, 0, false)}, "unknown", 0},
		{"empty", nil, "unknown", 0},
		{"invalid", []messages.ChatMessage{cacheMessage(100, 101, true)}, "unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := collectContextSnapshot(context.Background(), &Config{SchemaPath: "schema.json"}, &conversationState{}, tc.history)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.cache != tc.want {
				t.Fatalf("cache = %q, want %q", snapshot.cache, tc.want)
			}
			if snapshot.cacheKnown != (tc.want != "unknown") || snapshot.cacheFraction != tc.fraction {
				t.Fatalf("cache meter = %v (known %t), want %v (%s)", snapshot.cacheFraction, snapshot.cacheKnown, tc.fraction, tc.want)
			}
		})
	}
}

func TestContextSnapshotRequestViewAndReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config *Config
	}{
		{"nil config", nil},
		{"free text", &Config{}},
		{"structured output", &Config{SchemaPath: "schema.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "context-snapshot")
			registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, sandbox.Config{}))
			defer registry.Close()
			state := &conversationState{
				session: session, toolRegistry: registry, skillCatalog: referenceTestCatalog(t),
				settings: Settings{SystemPrompt: "custom persona"}, displayContract: richTerminalDisplayContract,
				instructionWarnings: []string{"not yet delivered"},
			}
			registerSessionTitleTool(state)
			testAddMessages(t, session, []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: "custom persona", Parts: []messages.ContentPart{{Type: "text", Text: "extra system text"}}},
				{Role: messages.MessageRoleUser, Content: "old question"},
				cacheMessage(10_000, 5_000, true),
				messages.Compaction{Summary: "earlier work"}.Message(),
				{Role: messages.MessageRoleUser, Content: "continue"},
				{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "lookup", Arguments: `{}`}}},
				{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "lookup", Content: strings.Repeat("old result ", 1_000)},
				messages.Compaction{ClearThrough: "call"}.Message(),
				cacheMessage(500, 250, true),
			})
			history := testSessionHistory(t, session)
			before, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := session.GetMetadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := collectContextSnapshot(ctx, tc.config, state, history)
			if err != nil {
				t.Fatal(err)
			}
			// Independently compose the custom persona, frontend/title guidance,
			// skills and sandbox in request order. Structured output omits only
			// the send-time contracts, not skills or the sandbox context.
			view := llm.RequestView(history, state.viewTools())
			if tc.config == nil || tc.config.SchemaPath == "" {
				title, err := sessionTitleGuidance(ctx, state)
				if err != nil || title == "" {
					t.Fatalf("title guidance = %q, err = %v", title, err)
				}
				view = applyDisplayContract(view, sendTimeContracts(state.displayContract)+"\n\n"+title)
			}
			view[0].Content = state.skillCatalog.RuntimeSystemPrompt(view[0].Content)
			view, err = llm.WithSandboxContext(view, registry)
			if err != nil {
				t.Fatal(err)
			}
			wantRoles := []contextRoleUsage{{role: "user"}, {role: "assistant"}, {role: "tool"}, {role: "system"}}
			for i := range wantRoles {
				for _, msg := range view {
					if msg.Role == wantRoles[i].role {
						wantRoles[i].count++
						wantRoles[i].tokens += llm.EstimateMessageTokens(msg)
					}
				}
			}
			if !reflect.DeepEqual(snapshot.roles, wantRoles) {
				t.Fatalf("roles = %+v, want %+v", snapshot.roles, wantRoles)
			}
			if snapshot.roles[3].tokens <= llm.EstimateMessageTokens(history[0]) || snapshot.roles[2].tokens >= llm.EstimateMessageTokens(history[6]) {
				t.Fatalf("generated guidance or tool clearing not reflected: %+v", snapshot.roles)
			}
			storedTokens := 0
			for _, msg := range history {
				storedTokens += sessions.EstimateTokens(msg)
			}
			if snapshot.storedMessages != len(history) || snapshot.storedTokens != storedTokens || snapshot.summaries != 1 || snapshot.clears != 1 {
				t.Fatalf("stored accounting = %+v, want %d messages and %d tokens", snapshot, len(history), storedTokens)
			}
			details, err := contextStats(ctx, tc.config, state, history)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(details, snapshot.details()) {
				t.Fatalf("legacy details = %v, snapshot details = %v", details, snapshot.details())
			}
			after, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			durable, err := json.Marshal(testSessionHistory(t, session))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) || string(durable) != string(before) {
				t.Fatal("inspector changed supplied or durable history")
			}
			afterMetadata, err := session.GetMetadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterMetadata, metadata) || !reflect.DeepEqual(state.instructionWarnings, []string{"not yet delivered"}) {
				t.Fatal("inspector changed session metadata or consumed instruction warnings")
			}
		})
	}
}

func TestContextSnapshotDetailsPreservesText(t *testing.T) {
	snapshot := contextSnapshot{
		roles:     []contextRoleUsage{{role: "user", count: 1, tokens: 10}, {role: "assistant", count: 2, tokens: 20}, {role: "tool"}, {role: "system", count: 1, tokens: 30}},
		summaries: 1, clears: 2, storedMessages: 7, storedTokens: 100, cache: "75% cache hit",
		trend: []contextTrendPoint{{tokens: 123, compacted: true}},
	}
	want := []string{
		"requests carry:",
		"user       1 · ~10 tokens",
		"assistant  2 · ~20 tokens",
		"tool       0 · ~0 tokens",
		"system     1 · ~30 tokens",
		"compactions: 1 summary · 2 clears",
		"transcript: 7 msgs · ~100 tokens stored",
		"",
		"session cache: 75% cache hit",
	}
	if got := snapshot.details(); !reflect.DeepEqual(got, want) {
		t.Fatalf("details = %q, want %q", got, want)
	}
}
