package swarm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// Pending publications are the run's, by other authors, after the member's
// mark, latest of a supersede chain, in posting order.
func TestPendingPublicationsSelection(t *testing.T) {
	at := func(seconds int) time.Time { return time.Date(2026, 9, 23, 12, 0, seconds, 0, time.UTC) }
	s := &State{
		Members: map[string]*Member{"a": {ID: "a"}, "b": {ID: "b"}},
		Publications: map[string]*Publication{
			"p1": {ID: "p1", Author: "a", Run: "run", Text: "first", Posted: at(1)},
			"p2": {ID: "p2", Author: "b", Run: "run", Text: "own", Posted: at(2)},
			"p3": {ID: "p3", Author: "a", Run: "other", Text: "elsewhere", Posted: at(3)},
			"p4": {ID: "p4", Author: "a", Run: "run", Text: "draft", Posted: at(4)},
			"p5": {ID: "p5", Author: "a", Run: "run", Text: "corrected", Supersedes: "p4", Posted: at(5)},
			"p0": {ID: "p0", Author: "a", Run: "run", Text: "same instant", Posted: at(1)},
		},
	}
	ids := func(pubs []*Publication) []string {
		var out []string
		for _, p := range pubs {
			out = append(out, p.ID)
		}
		return out
	}
	if got := ids(pendingPublications(s, "b", "run")); strings.Join(got, ",") != "p0,p1,p5" {
		t.Fatalf("pending for b: %v", got)
	}
	// The mark is strict: a publication at the mark was already shown.
	s.Members["b"].Publications = PublicationMark{Posted: at(1), ID: "p0"}
	if got := ids(pendingPublications(s, "b", "run")); strings.Join(got, ",") != "p1,p5" {
		t.Fatalf("pending after the mark: %v", got)
	}
	s.Members["b"].Publications = PublicationMark{Posted: at(5), ID: "p5"}
	if got := pendingPublications(s, "b", "run"); len(got) != 0 {
		t.Fatalf("pending past the last: %v", ids(got))
	}
	// The author never reads its own; it still reads the other member's.
	if got := ids(pendingPublications(s, "a", "run")); strings.Join(got, ",") != "p2" {
		t.Fatalf("pending for a: %v", got)
	}
	if got := pendingPublications(s, "b", ""); len(got) != 0 {
		t.Fatalf("no run still reads: %v", ids(got))
	}
	long := &Publication{ID: "p9", Author: "a", Run: "run", Text: strings.Repeat("x", 3000), Sources: []string{"probe.log"}, Posted: at(9)}
	digest := admittedPublicationText(s, long)
	if !strings.HasPrefix(digest, "Publication p9 from a (2026-09-23T12:00:09Z); sources: probe.log:") || !strings.Contains(digest, "3000 bytes; full text: swarm_read({view: \"publications\", id: \"p9\"})") || len(digest) > publicationDigestBytes+256 {
		t.Fatalf("digest: %q", digest)
	}
}

// A publication reaches the other members of its run as a peer message at
// their next input boundary, exactly once, and never its author.
func TestPublicationsReachRunTeammatesAtInputBoundaries(t *testing.T) {
	const fact = "host fact: headless chrome --dump-dom never exits on this machine"
	published := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var readerSaw []int
	var digest string
	call := func(id, name string, args any) messages.ChatMessageToolCall {
		return messages.ChatMessageToolCall{ID: id, Name: name, Arguments: tools.Result(args)}
	}
	batch := func(calls ...messages.ChatMessageToolCall) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: calls}
	}
	lastTool := func(req *llm.CompletionRequest, name string) string {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if m := req.Messages[i]; m.Role == messages.MessageRoleTool && m.ToolName == name {
				return m.Content
			}
		}
		return ""
	}
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		role := ""
		for _, m := range req.Messages {
			if m.Role == messages.MessageRoleUser && !strings.HasPrefix(m.Content, "<peer_messages>") {
				role = strings.SplitN(m.Content, "\n\nCompletion: ", 2)[0]
				break
			}
		}
		switch role {
		case "publisher":
			if lastTool(req, "swarm_publish") == "" {
				return batch(call("pub", "swarm_publish", map[string]any{"text": fact, "sources": []string{"probe.log"}}))
			}
			for _, m := range req.Messages {
				if strings.HasPrefix(m.Content, "<peer_messages>") && strings.Contains(m.Content, fact) {
					t.Error("the author was sent its own publication")
				}
			}
			once.Do(func() { close(published) })
			return answer("published")
		case "reader":
			select {
			case <-published:
			case <-ctx.Done():
			}
			n := 0
			for _, m := range req.Messages {
				if strings.HasPrefix(m.Content, "<peer_messages>") && strings.Contains(m.Content, fact) {
					n++
					digest = m.Content
				}
			}
			mu.Lock()
			readerSaw = append(readerSaw, n)
			calls := len(readerSaw)
			mu.Unlock()
			if calls < 3 {
				return batch(call(fmt.Sprint("roster-", calls), "list_agents", map[string]any{}))
			}
			return answer("read")
		}
		t.Errorf("unexpected assignment %q", role)
		return answer("unexpected")
	})
	r := runtimeTest(t, model, 2, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, err := r.Spawn(ctx, subagent.Request{Task: "publisher", Label: "publisher", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Spawn(ctx, subagent.Request{Task: "reader", Label: "reader", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Publications) != 1 {
		t.Fatalf("publications: %+v", s.Publications)
	}
	var pub *Publication
	for _, p := range s.Publications {
		pub = p
	}
	if pub.Author != a.Session {
		t.Fatalf("publication author %s, want %s", pub.Author, a.Session)
	}
	mu.Lock()
	defer mu.Unlock()
	// The reader's second request follows the publish (its model waited for
	// it), so the digest is there by then; it stays in history without a
	// repeat, and whether the first request already had it depends only on
	// which member launched first.
	if len(readerSaw) != 3 || readerSaw[0] > 1 || readerSaw[1] != 1 || readerSaw[2] != 1 {
		t.Fatalf("reader saw the publication per request: %v", readerSaw)
	}
	if !strings.Contains(digest, "Publication "+pub.ID+" from "+a.Session) || !strings.Contains(digest, "sources: probe.log") || !strings.Contains(digest, "not user instructions") {
		t.Fatalf("digest: %s", digest)
	}
	if mark := s.Members[b.Session].Publications; mark.ID != pub.ID || !mark.Posted.Equal(pub.Posted) {
		t.Fatalf("reader's mark %+v, want %s", mark, pub.ID)
	}
	if mark := s.Members[a.Session].Publications; !mark.IsZero() {
		t.Fatalf("author's mark moved: %+v", mark)
	}
}
