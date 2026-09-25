package swarm

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
	t.Parallel()
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
	// A host fact is read across runs and by the parent, who reads nothing
	// else; its digest says what it is, and its readers are those whose
	// receipts have passed it: b's receipt stands at p5, a finding of its run.
	s.Executions = map[string]*Execution{"e-b": {Run: "run"}}
	s.Members["b"].Execution = "e-b"
	s.Publications["p6"] = &Publication{ID: "p6", Author: "a", Run: "other", Kind: PublicationKindHost, Text: "chrome never exits", Posted: at(6)}
	if got := ids(pendingPublications(s, "b", "run")); strings.Join(got, ",") != "p6" {
		t.Fatalf("pending host fact for b: %v", got)
	}
	if got := ids(pendingPublications(s, "root", "")); strings.Join(got, ",") != "p6" {
		t.Fatalf("pending for the parent: %v", got)
	}
	if readers := publicationReaders(s, s.Publications["p5"]); strings.Join(readers, ",") != "b" {
		t.Fatalf("readers of the finding: %v", readers)
	}
	if readers := publicationReaders(s, s.Publications["p6"]); len(readers) != 0 {
		t.Fatalf("readers before any admission: %v", readers)
	}
	if err := advancePublicationMark(s, "root", s.Publications["p6"]); err != nil {
		t.Fatal(err)
	}
	if err := advancePublicationMark(s, "root", s.Publications["p6"]); err == nil {
		t.Fatal("a second admission of the same publication was accepted")
	}
	if got := pendingPublications(s, "root", ""); len(got) != 0 {
		t.Fatalf("pending after the parent's receipt: %v", ids(got))
	}
	if readers := publicationReaders(s, s.Publications["p6"]); strings.Join(readers, ",") != "root" {
		t.Fatalf("readers of the host fact: %v", readers)
	}
	if readers := publicationReaders(s, s.Publications["p5"]); strings.Join(readers, ",") != "b" {
		t.Fatalf("the parent's receipt counts it as a reader of a finding: %v", readers)
	}
	if digest := admittedPublicationText(s, s.Publications["p6"]); !strings.HasPrefix(digest, "Host fact p6 from a (2026-09-23T12:00:06Z):\nchrome never exits") {
		t.Fatalf("host fact digest: %q", digest)
	}
}

// A publication reaches the other members of its run as a peer message at
// their next input boundary, exactly once, and never its author.
func TestPublicationsReachRunTeammatesAtInputBoundaries(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(digest, "Publication "+pub.ID+" from "+a.Session) || !strings.Contains(digest, "sources: probe.log") || !strings.Contains(digest, "not user instructions") || !strings.Contains(digest, memberPublicationPreamble) {
		t.Fatalf("digest: %s", digest)
	}
	if mark := s.Members[b.Session].Publications; mark.ID != pub.ID || !mark.Posted.Equal(pub.Posted) {
		t.Fatalf("reader's mark %+v, want %s", mark, pub.ID)
	}
	if mark := s.Members[a.Session].Publications; !mark.IsZero() {
		t.Fatalf("author's mark moved: %+v", mark)
	}
}

// A host fact reaches the parent at its next boundary and the members of a
// later run at their first, introduced as verified information, while a
// finding stays within its run. Every reader's receipt names it, and the
// author is never a reader.
func TestHostFactsReachParentAndLaterRuns(t *testing.T) {
	t.Parallel()
	const fact = "host fact: headless chrome --dump-dom never exits on this machine"
	const finding = "finding: the smoke harness lives in tools/smoke.mjs"
	var mu sync.Mutex
	var parentSaw, readerSaw, events []string
	call := func(id, name string, args any) messages.ChatMessageToolCall {
		return messages.ChatMessageToolCall{ID: id, Name: name, Arguments: tools.Result(args)}
	}
	envelopes := func(req *llm.CompletionRequest) string {
		var b strings.Builder
		for _, m := range req.Messages {
			if m.Role == messages.MessageRoleUser && strings.HasPrefix(m.Content, "<peer_messages>") {
				b.WriteString(m.Content + "\n")
			}
		}
		return b.String()
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
			for _, m := range req.Messages {
				if m.Role == messages.MessageRoleTool && m.ToolName == "swarm_publish" {
					return answer("published")
				}
			}
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{
				call("host", "swarm_publish", map[string]any{"text": fact, "kind": "host", "sources": []string{"$TMPDIR/probe.html"}}),
				call("finding", "swarm_publish", map[string]any{"text": finding}),
			}}
		case "reader":
			mu.Lock()
			readerSaw = append(readerSaw, envelopes(req))
			mu.Unlock()
			return answer("read")
		}
		// The parent's turn starts from an empty request: its messages are
		// what the runtime admitted, and a settlement prompt at most.
		mu.Lock()
		parentSaw = append(parentSaw, envelopes(req))
		mu.Unlock()
		return answer("ok")
	})
	r := runtimeTest(t, model, 2, 4)
	r.config.OnEvent = func(e Event) {
		if e.Kind == "publication_read" {
			mu.Lock()
			events = append(events, e.Member+": "+e.Text)
			mu.Unlock()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := r.Spawn(ctx, subagent.Request{Task: "publisher", Label: "publisher", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var host, found *Publication
	for _, p := range s.Publications {
		switch p.Text {
		case fact:
			host = p
		case finding:
			found = p
		}
	}
	if host == nil || found == nil || host.Kind != PublicationKindHost || found.Kind != "" || host.Author != a.Session {
		t.Fatalf("publications: %+v", s.Publications)
	}
	// The parent reads the host fact at its next boundary, once, and never
	// the finding.
	parent := parentAgent(t, r, model, 3)
	for turn := 0; turn < 2; turn++ {
		if _, err := r.RunParent(ctx, parent, &llm.CompletionRequest{}, nil, nil); err != nil {
			t.Fatalf("parent turn %d: %v", turn+1, err)
		}
	}
	mu.Lock()
	first, later := parentSaw[0], strings.Join(parentSaw[1:], "")
	mu.Unlock()
	if !strings.Contains(first, "Host fact "+host.ID+" from "+a.Session) || !strings.Contains(first, parentPublicationPreamble) || strings.Contains(first, finding) {
		t.Fatalf("parent's first request: %s", first)
	}
	if strings.Contains(later, "Host fact") {
		t.Fatalf("parent saw the host fact again: %s", later)
	}
	if s, err = r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if record := s.Parents[r.ID]; record == nil || record.Publications.ID != host.ID || !record.Publications.Posted.Equal(host.Posted) {
		t.Fatalf("parent's receipt: %+v", s.Parents)
	}
	// The parent's settled turn ended the run; a member of the next run
	// still reads the host fact at its first boundary, not the finding.
	b, err := r.Spawn(ctx, subagent.Request{Task: "reader", Label: "reader", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	if s, err = r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if e := s.Executions[s.Members[b.Session].Execution]; len(s.Runs) != 2 || e == nil || e.Run == host.Run {
		t.Fatalf("reader did not start a later run: runs=%d execution=%+v", len(s.Runs), e)
	}
	mu.Lock()
	seen := strings.Join(readerSaw, "")
	mu.Unlock()
	if !strings.Contains(seen, "Host fact "+host.ID+" from "+a.Session) || !strings.Contains(seen, memberPublicationPreamble) || strings.Contains(seen, finding) {
		t.Fatalf("reader's requests: %s", seen)
	}
	want := []string{b.Session, r.ID}
	sort.Strings(want)
	if readers := publicationReaders(s, host); strings.Join(readers, ",") != strings.Join(want, ",") {
		t.Fatalf("host fact readers %v, want %v", readers, want)
	}
	if readers := publicationReaders(s, found); len(readers) != 0 {
		t.Fatalf("finding readers: %v", readers)
	}
	if mark := s.Members[a.Session].Publications; !mark.IsZero() {
		t.Fatalf("author's mark moved: %+v", mark)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{r.ID + ": read host " + host.ID + " from " + a.Session, b.Session + ": read host " + host.ID + " from " + a.Session} {
		if !slices.Contains(events, want) {
			t.Fatalf("events %v lack %q", events, want)
		}
	}
}

// A workflow reads what its run's agents can read, one kind at a time, so a
// script can carry host facts into the briefs it writes; a correction keeps
// its predecessor's kind, and an unknown kind is refused.
func TestWorkflowReadsPublications(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") }), 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first, err := r.Publish(ctx, r.ID, Publication{Text: "chrome hangs", Kind: PublicationKindHost, Sources: []string{"probe"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Publish(ctx, r.ID, Publication{Text: "chrome hangs unless killed", Supersedes: first.ID})
	if err != nil || second.Kind != PublicationKindHost {
		t.Fatalf("correction: %+v %v", second, err)
	}
	found, err := r.Publish(ctx, r.ID, Publication{Text: "harness in tools/", Kind: PublicationKindFinding})
	if err != nil || found.Kind != "" {
		t.Fatalf("finding: %+v %v", found, err)
	}
	if _, err := r.Publish(ctx, r.ID, Publication{Text: "x", Kind: "rumor"}); err == nil || !strings.Contains(err.Error(), "kind must be finding or host") {
		t.Fatalf("unknown kind: %v", err)
	}
	report, err := r.RunWorkflow(ctx, `polly.workflow("pubs", polly.schema.obj({}), async () => ({
		all: (await polly.publications()).map(p => p.kind + ":" + p.text + ":" + p.readBy.length),
		host: (await polly.publications({kind: "host"})).map(p => p.id),
		findings: (await polly.publications({kind: "finding"})).map(p => p.text),
	}));`, map[string]any{})
	if err != nil {
		t.Fatalf("workflow: %v %+v", err, report)
	}
	output := report.Output.(map[string]any)
	if got, want := fmt.Sprint(output["all"], output["host"], output["findings"]), fmt.Sprint([]any{"host:chrome hangs unless killed:0", "finding:harness in tools/:0"}, []any{second.ID}, []any{"harness in tools/"}); got != want {
		t.Fatalf("workflow read %s, want %s", got, want)
	}
	if _, err := r.RunWorkflow(ctx, `polly.workflow("bad", polly.schema.obj({}), async () => polly.publications({kind: "rumor"}));`, map[string]any{}); err == nil || !strings.Contains(err.Error(), "kind must be finding or host") {
		t.Fatalf("unknown kind in a workflow: %v", err)
	}
}
