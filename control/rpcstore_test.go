package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/control/controltest"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

const setupKey = "0123456789abcdef0123456789abcdef"

func dial(t *testing.T, srv *controltest.Server) *control.Client {
	t.Helper()
	nc, err := net.Dial("unix", srv.Socket)
	if err != nil {
		t.Fatal(err)
	}
	c := control.NewClient(nc)
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	made, err := c.Join(ctx, control.JoinReq{Setup: setupKey})
	if err != nil || !made.NeedStart {
		t.Fatalf("join = %+v, %v", made, err)
	}
	if made, err = c.Start(ctx, control.Start{Setup: control.Setup{Key: setupKey}}); err != nil || !made.Made {
		t.Fatalf("start = %+v, %v", made, err)
	}
	if _, err := c.Hello(ctx, setupKey); err != nil {
		t.Fatal(err)
	}
	return c
}

func line(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClientHelloRefusesAnotherSetup(t *testing.T) {
	srv := controltest.New(t, nil)
	nc, err := net.Dial("unix", srv.Socket)
	if err != nil {
		t.Fatal(err)
	}
	c := control.NewClient(nc)
	defer c.Close()
	_, err = c.Hello(context.Background(), "ffffffffffffffffffffffffffffffff")
	if !control.IsRefusal(err, control.RefusedMismatch) {
		t.Fatalf("hello of another setup = %v", err)
	}
	var rpc *control.RPCError
	if !errors.As(err, &rpc) || rpc.Member != "setup" {
		t.Fatalf("refusal = %#v", err)
	}
}

func TestStoreListsReadsAndAcquires(t *testing.T) {
	srv := controltest.New(t, nil)
	root := srv.Add(&controltest.Session{Name: "brave-otter", Model: "anthropic/claude-opus-5", Lines: []json.RawMessage{
		line(t, map[string]any{"role": "user", "content": "hi"}),
		line(t, map[string]any{"role": "assistant", "content": "hello", "usage": map[string]any{"input_tokens": 7, "output_tokens": 2, "tokens_reported": true}}),
	}})
	srv.Add(&controltest.Session{Name: "brave-otter-kid", ParentID: root.ID, Parent: root.Name, Label: "audit", LastUsed: time.Now().Add(-time.Hour)})
	c := dial(t, srv)
	store := control.NewRPCStore(func(context.Context) (*control.Client, error) { return c, nil })
	ctx := context.Background()

	summaries, err := store.ListSummaries(ctx)
	if err != nil || len(summaries) != 2 {
		t.Fatalf("summaries = %+v, %v", summaries, err)
	}
	if summaries[0].ID != root.ID || summaries[0].MessageCount != 2 || summaries[0].Metadata.Model != "anthropic/claude-opus-5" {
		t.Fatalf("root summary = %+v", summaries[0])
	}
	if summaries[1].ParentID != root.ID || summaries[1].Metadata.Parent != "brave-otter" || summaries[1].Metadata.Description != "audit" {
		t.Fatalf("child summary = %+v %+v", summaries[1], summaries[1].Metadata)
	}
	if last, err := store.GetLast(ctx); err != nil || last != "brave-otter" {
		t.Fatalf("last = %q, %v", last, err)
	}
	if ok, err := store.Exists(ctx, "nobody"); err != nil || ok {
		t.Fatalf("exists(nobody) = %v, %v", ok, err)
	}
	if _, err := store.GetMetadata(ctx, "nobody"); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("metadata of nobody = %v", err)
	}

	view, err := store.ReadView(ctx, sessions.ViewTarget{Name: "brave-otter"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != root.ID || view.Sequence != 2 || len(view.History) != 2 || view.History[1].GetInputTokens() != 7 {
		t.Fatalf("view = %+v", view)
	}
	again, err := store.ReadView(ctx, sessions.ViewTarget{ID: root.ID}, view.Revision)
	if err != nil || !again.Unchanged || again.History != nil {
		t.Fatalf("unchanged view = %+v, %v", again, err)
	}

	// An existing session opens by name; one that is not there opens as
	// pending, for the backend to make on its first prompt.
	sess, err := store.Acquire(ctx, "brave-otter", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	history, err := sess.GetHistory(ctx)
	if err != nil || len(history) != 2 || history[0].Content != "hi" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	if err := sess.AddMessage(ctx, messages.ChatMessage{Role: "user", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Rename(ctx, "other"); !errors.Is(err, control.ErrUnsupported) {
		t.Fatalf("rename = %v", err)
	}
	fresh, err := store.Acquire(ctx, "quiet-lynx", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.(*control.RPCSession).Pending() || !control.IsID(fresh.(*control.RPCSession).ID()) {
		t.Fatal("a new session is not pending under a drawn id")
	}
	if h, err := fresh.GetHistory(ctx); err != nil || len(h) != 0 {
		t.Fatalf("pending history = %v, %v", h, err)
	}
	if _, err := store.Acquire(ctx, "quiet-lynx", sessions.AcquireOptions{ExistingOnly: true}); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("existing-only acquire of a new name = %v", err)
	}
	// Settings set here are kept here, over what the record says.
	md, _ := sess.GetMetadata(ctx)
	md.ThinkingEffort = "low"
	if err := sess.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.GetMetadata(ctx, "brave-otter"); again.ThinkingEffort != "low" || again.Model != "anthropic/claude-opus-5" {
		t.Fatalf("kept settings = %+v", again)
	}
}

func TestViewOfAChildBySpawnCallAndItsSavedImage(t *testing.T) {
	srv := controltest.New(t, nil)
	call := control.LineToolCall{ID: "call_7", Name: "spawn_agent", Arguments: `{"task":"look"}`}
	root := srv.Add(&controltest.Session{Name: "root-one", Lines: []json.RawMessage{
		line(t, map[string]any{"role": "user", "content": "go"}),
		line(t, controltest.Assistant("", call)),
		line(t, controltest.ToolResult(call, "found it\n\n(agent session root-one-looker · 10 in / 2 out)", true)),
	}})
	png := []byte("\x89PNG\r\n\x1a\nnot really")
	child := srv.Add(&controltest.Session{Name: "root-one-looker", ParentID: root.ID, Parent: "root-one", Lines: []json.RawMessage{
		line(t, map[string]any{"role": "tool", "tool_call_id": "v1", "tool_name": "view_image", "content": "[image]", "tool_succeeded": true,
			"parts": []map[string]any{{"kind": "image_file", "path": "/far/away/outputs/shot-1.png", "bytes": len(png), "mime_type": "image/png", "file_name": "shot.png"}}}),
	}, Outputs: map[string][]byte{"shot-1.png": png}})
	c := dial(t, srv)
	store := control.NewRPCStore(func(context.Context) (*control.Client, error) { return c, nil })
	ctx := context.Background()
	view, err := store.ReadView(ctx, sessions.ViewTarget{Parent: "root-one", SpawnCallID: "call_7"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != child.ID || view.ParentID != root.ID {
		t.Fatalf("child view = %+v", view)
	}
	part := view.History[0].Parts[0]
	if part.Type != "image_artifact" || part.Artifact == nil || part.Artifact.Bytes != int64(len(png)) || part.FileName != "shot.png" {
		t.Fatalf("image part = %+v", part)
	}
	r, err := view.Artifacts.Open(ctx, part.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != string(png) {
		t.Fatalf("image bytes = %q", got)
	}
	if _, err := store.ReadView(ctx, sessions.ViewTarget{Parent: "root-one", SpawnCallID: "call_unknown"}, ""); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("view of an unknown call = %v", err)
	}
}

func TestPromptEventsReachASubscriptionInOrder(t *testing.T) {
	srv := controltest.New(t, controltest.Say("all is well"))
	c := dial(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, _ := control.NewID()
	sub := c.SubscribeAgents(true, id)
	defer sub.Close()
	if _, err := c.Watch(ctx, control.Watch{Agent: id, Kinds: control.AllKinds, Children: true}); err != nil {
		t.Fatal(err)
	}
	content, _ := control.MessageToLine(messages.ChatMessage{Role: "user", Content: "how is it"}, nil)
	reply, err := c.Prompt(ctx, control.Prompt{Session: control.Session{New: true, NewID: id, NewName: "first-one"}, Model: "openai/x", Effort: "off", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Session != id || reply.Name != "first-one" || reply.Stop != control.StopEndTurn {
		t.Fatalf("reply = %+v", reply)
	}
	var text strings.Builder
	var methods []string
	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		methods = append(methods, ev.Method)
		if ev.Delta != nil {
			text.WriteString(ev.Delta.Text)
		}
		if ev.End != nil {
			break
		}
	}
	if text.String() != "all is well" {
		t.Fatalf("text = %q (%v)", text.String(), methods)
	}
	if got := strings.Join(methods, " "); !strings.HasPrefix(got, "checkpoint delta") || !strings.HasSuffix(got, "message checkpoint end") {
		t.Fatalf("methods = %s", got)
	}
	if p := srv.Prompts(); len(p) != 1 || !strings.Contains(string(p[0].Content), "how is it") {
		t.Fatalf("prompts = %+v", p)
	}
}

func TestBusyAndCancelledRequests(t *testing.T) {
	block := make(chan struct{})
	srv := controltest.New(t, func(ctx context.Context, tt *controltest.Turn) control.Reply {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return control.Reply{Stop: control.StopEndTurn}
	})
	c := dial(t, srv)
	sess := srv.Add(&controltest.Session{Name: "held"})
	ctx := context.Background()
	content, _ := control.MessageToLine(messages.ChatMessage{Role: "user", Content: "x"}, nil)
	done := make(chan control.Reply, 1)
	go func() {
		r, _ := c.Prompt(ctx, control.Prompt{Session: control.Session{ID: sess.ID}, Content: content, Effort: "off"})
		done <- r
	}()
	controltest.Wait(t, "the turn to run", func() bool { return srv.Running(sess.ID) })
	if _, err := c.Prompt(ctx, control.Prompt{Session: control.Session{ID: sess.ID}, Content: content, Effort: "off"}); !control.IsRefusal(err, control.RefusedBusy) {
		t.Fatalf("second prompt = %v", err)
	}
	// A caller that gives up returns at once; the turn goes on.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := c.Agents(short, control.AgentsReq{IDs: []control.ID{sess.ID}}); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("agents = %v", err)
	}
	stopped, err := c.Stop(ctx, control.StopReq{Agent: sess.ID})
	if err != nil || !stopped.Running {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
	if r := <-done; r.Stop != control.StopCancelled {
		t.Fatalf("stopped turn = %+v", r)
	}
	close(block)
}

func TestConnectionEndFailsOpenRequestsAndEndsSubscriptions(t *testing.T) {
	srv := controltest.New(t, func(ctx context.Context, tt *controltest.Turn) control.Reply {
		<-ctx.Done()
		return control.Reply{}
	})
	c := dial(t, srv)
	sess := srv.Add(&controltest.Session{Name: "dropped"})
	sub := c.SubscribeAgents(false, sess.ID)
	errs := make(chan error, 1)
	go func() {
		content, _ := control.MessageToLine(messages.ChatMessage{Role: "user", Content: "x"}, nil)
		_, err := c.Prompt(context.Background(), control.Prompt{Session: control.Session{ID: sess.ID}, Content: content, Effort: "off"})
		errs <- err
	}()
	controltest.Wait(t, "the turn to run", func() bool { return srv.Running(sess.ID) })
	srv.DropConnections(true)
	if err := <-errs; err == nil {
		t.Fatal("a prompt on a dropped connection succeeded")
	}
	if !c.Behind() {
		t.Fatal("the behind line was not taken")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, err := sub.Next(ctx)
		if errors.Is(err, control.ErrEndOfEvents) {
			break
		}
		if err != nil {
			t.Fatalf("subscription = %v", err)
		}
	}
}
