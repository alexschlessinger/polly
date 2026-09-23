package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
)

func TestAssistantMarkdownWaitsForPaint(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		m := newReplModel()
		m.hidden = hidden
		m.appendAssistant("## Result\n\n```go\nvar value = 1\n```\n")
		if m.streamShown != 0 || m.transcript[0].text != "" {
			t.Fatal("provider callback rendered Markdown")
		}
		m.finishAssistantBlock("")
		if m.transcript[0].text != "" || m.transcript[0].markdown == "" {
			t.Fatal("finalization rendered or lost the hidden result")
		}
		m.hidden = false
		m.renderPendingMarkdown()
		if !strings.Contains(plainStyledText(m.transcript[0].text), "var value = 1") {
			t.Fatal("paint lost the finalized text")
		}
		if m.markdownPending || m.transcript[0].markdown != "" {
			t.Fatal("paint retained pending Markdown")
		}
	}
}

func TestMarkdownCacheReusesCodeAndHonorsLateDefinitions(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("```go\nvar value = 1\n```\n\nSee [docs][ref].\n")
	m.renderPendingMarkdown()
	_, lines := m.streamCodeCache.Block(0)
	first := &lines[0]
	m.appendAssistant("\n[ref]: https://example.com\n")
	m.renderPendingMarkdown()
	if _, lines := m.streamCodeCache.Block(0); &lines[0] != first {
		t.Fatal("unchanged code was highlighted again")
	}
	m.finishAssistantBlock("")
	m.renderPendingMarkdown()
	if !strings.Contains(plainStyledText(m.transcript[0].text), "docs (https://example.com)") {
		t.Fatal("cached code prevented late reference resolution")
	}
}

func TestLongStreamingCodeHighlightsOffTheLoop(t *testing.T) {
	r, _ := affordanceTestREPL(t)
	m := r.model
	var src strings.Builder
	src.WriteString("```go\n")
	for i := range 200 {
		fmt.Fprintf(&src, "var v%d = %d\n", i, i)
	}
	m.appendAssistant(src.String())
	r.render()
	idx := m.currentAssistant
	exact, _, _, _ := markdown.RenderWithWidth(src.String(), m.imageBaseDir, true, nil, m.markdownWidth)
	if m.transcript[idx].text == exact || !r.codeHighlightPending() {
		t.Fatal("the paint highlighted a long block on the loop")
	}
	runUITask(t, r)
	r.render()
	if m.transcript[idx].text != exact || r.codeHighlightPending() {
		t.Fatal("the landed pass did not repaint the stream")
	}

	// The message settles while a pass is out; the pass lands on nothing.
	m.appendAssistant("var tail = 1\n")
	r.render()
	m.finishAssistantBlock("")
	r.render()
	settled := m.transcript[idx].text
	want, _, _, _ := markdown.RenderWithWidth(src.String()+"var tail = 1", m.imageBaseDir, false, nil, m.markdownWidth)
	if settled != want {
		t.Fatal("the settled message is not highlighted whole")
	}
	runUITask(t, r)
	r.render()
	if m.transcript[idx].text != settled || r.codeHighlightPending() {
		t.Fatal("a pass from before the settle changed the settled message")
	}
}

func TestHiddenModelLockDoesNotBlockVisibleNotifications(t *testing.T) {
	r := newManagedREPL(&Config{}, "visible", 0, 0)
	defer r.closeTabs()
	hidden := newReplModel()
	hidden.hidden = true
	hidden.signalHiddenLocked(signalApprovalNeeded, "read file")
	hidden.pushNotice("done")
	r.tabs = append(r.tabs, &replTab{name: "child", model: hidden})
	hidden.mu.Lock()
	done := make(chan struct{})
	go func() {
		r.relayTabSignals()
		r.takeHiddenNotices(true, false)
		close(done)
	}()
	select {
	case <-done:
		hidden.mu.Unlock()
	case <-time.After(time.Second):
		hidden.mu.Unlock()
		<-done
		t.Fatal("visible paint waited for a hidden transcript lock")
	}
}

func BenchmarkAssistantStreaming(b *testing.B) {
	raw := strings.Repeat("Review **results** for `worker.go`: the task finished normally.\n\n", 1024)
	b.ReportAllocs()
	for b.Loop() {
		m := newReplModel()
		for i := 0; i < len(raw); i += 128 {
			m.appendAssistant(raw[i:min(i+128, len(raw))])
			// Several provider chunks may arrive within one 50 ms frame.
			if (i+128)%4096 == 0 {
				m.renderPendingMarkdown()
			}
		}
		m.finishAssistantBlock("")
		m.renderPendingMarkdown()
	}
}

func BenchmarkHiddenAssistantFinalization(b *testing.B) {
	raw := "```go\n" + strings.Repeat("if value > 0 { fmt.Println(value) }\n", 4096) + "```\n"
	b.ReportAllocs()
	for b.Loop() {
		m := newReplModel()
		m.hidden = true
		m.appendAssistant(raw)
		m.finishAssistantBlock("")
	}
}
