package main

import (
	"github.com/alexschlessinger/pollytool/messages"
	"strings"
	"testing"
)

func cacheMessage(in, read int, known bool) messages.ChatMessage {
	m := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer"}
	m.SetTokenUsage(in, 10)
	if known {
		m.SetPromptCacheUsage(read, 0)
	}
	return m
}

func TestTurnCacheRateWeightedAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []messages.ChatMessage
		want string
	}{
		{"weighted", []messages.ChatMessage{cacheMessage(100, 0, true), cacheMessage(300, 300, true)}, "75% cache hit"},
		{"zero", []messages.ChatMessage{cacheMessage(100, 0, true)}, "0% cache hit"},
		{"unknown", []messages.ChatMessage{cacheMessage(100, 0, false)}, ""},
		{"mixed", []messages.ChatMessage{cacheMessage(100, 100, true), cacheMessage(100, 0, false)}, ""},
		{"empty", nil, ""},
		{"invalid", []messages.ChatMessage{cacheMessage(100, 101, true)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c turnCacheUsage
			for _, m := range tc.msgs {
				c.add(m)
			}
			f, ok := turnCacheField(c)
			if f.raw != tc.want || ok != (tc.want != "") {
				t.Fatalf("got %q, %v", f.raw, ok)
			}
		})
	}
}

func TestSettledAndResumedCacheRate(t *testing.T) {
	withDisplayTTY(t)
	msgs := []messages.ChatMessage{cacheMessage(100, 0, true), cacheMessage(300, 300, true)}
	var cache turnCacheUsage
	for _, msg := range msgs {
		cache.add(msg)
	}
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.RecordTurnTokens(400, 20)
	tui.CompleteTurn(turnCompletion{Cache: cache})
	r.endTurn(nil)
	got := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(got, "75% cache hit") {
		t.Fatalf("settled: %s", got)
	}
	row, _ := m.turnDockRowFor(m.turnTrailers.latest().dock, 30)
	if strings.Contains(plainStyledText(row), "cache hit") {
		t.Fatalf("narrow row: %s", row)
	}
	history := append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}}, msgs...)
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "next"}, cacheMessage(100, 0, false))
	resumed := newReplModel()
	resumed.hydrateHistory(history, "ctx")
	got = plainStyledText(strings.Join(transcriptTexts(resumed), "\n"))
	if strings.Count(got, "75% cache hit") != 1 {
		t.Fatalf("resumed: %s", got)
	}
	if strings.Contains(plainStyledText(resumed.transcript[resumed.turnTrailers.latest().transcriptIndex].text), "cache hit") {
		t.Fatal("rate leaked to next turn")
	}
}
