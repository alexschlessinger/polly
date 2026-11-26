package rex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// pickRunner plays a Rex whose configuration file defines the actions named,
// and answers the action performed with stdout.
func pickRunner(stdout string, actions ...string) *fakeRunner {
	return &fakeRunner{run: func(_ context.Context, spec CommandSpec) (CommandResult, error) {
		if slices.Contains(spec.Args, "actions") {
			list := make([]ActionInfo, len(actions))
			for n, name := range actions {
				list[n] = ActionInfo{Name: name}
			}
			encoded, _ := json.Marshal(list)
			return CommandResult{Stdout: encoded}, nil
		}
		return CommandResult{Stdout: []byte(stdout)}, nil
	}}
}

func TestPickReturnsTheChosenRow(t *testing.T) {
	fr := pickRunner(`{"id":"b"}`, "other", "polly_ask")
	c := NewClient(WithCommandRunner(fr), WithEnvLookup(func(string) string { return "" }))
	got, err := c.Pick(context.Background(), "s1", "polly_ask", PickRequest{
		Title:       "Sessions",
		Placeholder: "Filter",
		Timeout:     90 * time.Second,
		Items:       []PickItem{{ID: "a", Title: `He said "hi"`}, {ID: "b", Title: "naïve\nline", Detail: "3m", Icon: "terminal"}},
	})
	if err != nil || got != (PickAnswer{ID: "b"}) {
		t.Fatalf("Pick = %+v, %v", got, err)
	}
	// The action is performed with the list on stdin, and no time limit.
	call := fr.lastCall()
	if want := []string{"--color", "never", "do", "-s", "s1", "polly_ask", "--args", "-"}; !reflect.DeepEqual(call.Args, want) {
		t.Fatalf("args = %v, want %v", call.Args, want)
	}
	var sent struct {
		Ask struct {
			Title       string     `json:"title"`
			Placeholder string     `json:"placeholder"`
			Timeout     int        `json:"timeout"`
			AllowOther  bool       `json:"allow_other"`
			Items       []PickItem `json:"items"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(call.Stdin, &sent); err != nil {
		t.Fatalf("stdin is not JSON: %v\n%s", err, call.Stdin)
	}
	prompt := sent.Ask
	if prompt.Title != "Sessions" || prompt.Placeholder != "Filter" || prompt.Timeout != 90 || prompt.AllowOther {
		t.Fatalf("prompt = %+v", prompt)
	}
	if len(prompt.Items) != 2 || prompt.Items[0].Title != `He said "hi"` || prompt.Items[1].Title != "naïve\nline" || prompt.Items[1].Icon != "terminal" {
		t.Fatalf("items = %+v", prompt.Items)
	}
}

func TestPickTakesTypedTextOnlyWhenAsked(t *testing.T) {
	typed := pickRunner(`{"text":"something else"}`, "polly_ask")
	c := NewClient(WithCommandRunner(typed))
	got, err := c.Pick(context.Background(), "s1", "polly_ask", PickRequest{Title: "Question", AllowOther: true, Items: []PickItem{{ID: "a"}}})
	if err != nil || got != (PickAnswer{Text: "something else"}) {
		t.Fatalf("Pick = %+v, %v", got, err)
	}
	if !strings.Contains(string(typed.lastCall().Stdin), `"allow_other":true`) {
		t.Fatalf("typed answers were not asked for: %s", typed.lastCall().Stdin)
	}
	// Text that was not asked for is no answer to a list.
	if got, err := c.Pick(context.Background(), "s1", "polly_ask", PickRequest{Title: "Question"}); err == nil {
		t.Fatalf("a list was answered with %+v", got)
	}
}

func TestInputReturnsTheTextTyped(t *testing.T) {
	fr := pickRunner(`{"text":""}`, "polly_ask")
	c := NewClient(WithCommandRunner(fr), WithEnvLookup(func(string) string { return "" }))
	// An empty line is an answer: the person submitted the field empty.
	text, err := c.Input(context.Background(), "s1", "polly_ask", InputRequest{Title: "Title", Value: "old", Confirm: "Save"})
	if err != nil || text != "" {
		t.Fatalf("Input = %q, %v", text, err)
	}
	var sent struct {
		Ask map[string]any `json:"ask"`
	}
	if err := json.Unmarshal(fr.lastCall().Stdin, &sent); err != nil {
		t.Fatal(err)
	}
	// Without items, which is how the action tells a line of text from a list.
	if _, list := sent.Ask["items"]; list || sent.Ask["title"] != "Title" || sent.Ask["value"] != "old" || sent.Ask["confirm"] != "Save" {
		t.Fatalf("asked %v", sent.Ask)
	}
	fr = pickRunner(`{"text":"new title"}`, "polly_ask")
	if text, err := NewClient(WithCommandRunner(fr)).Input(context.Background(), "s1", "polly_ask", InputRequest{Title: "Title"}); err != nil || text != "new title" {
		t.Fatalf("Input = %q, %v", text, err)
	}
	var prompt *PromptError
	fr = pickRunner(`{"why":"cancelled"}`, "polly_ask")
	if _, err := NewClient(WithCommandRunner(fr)).Input(context.Background(), "s1", "polly_ask", InputRequest{Title: "Title"}); !errors.As(err, &prompt) || !prompt.Dismissed() {
		t.Fatalf("a closed prompt = %v", err)
	}
}

func TestPickNeedsTheConfigurationFilesAction(t *testing.T) {
	fr := pickRunner(`{"id":"b"}`, "other")
	_, err := NewClient(WithCommandRunner(fr)).Pick(context.Background(), "s1", "polly_ask", PickRequest{Title: "Sessions"})
	if !errors.Is(err, ErrNoAskAction) {
		t.Fatalf("Pick without the action = %v", err)
	}
	if call := fr.lastCall(); slices.Contains(call.Args, "do") {
		t.Fatalf("an action nobody defined was performed: %v", call.Args)
	}
}

func TestPickWithoutAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		stdout    string
		reason    string
		dismissed bool
	}{
		{`{"why":"cancelled"}`, PromptCancelled, true},
		{`{"why":"timeout"}`, PromptTimeout, true},
		{`{"why":"no_client"}`, PromptNoClient, false},
		{`{"why":"failed","detail":"boom"}`, PromptFailed, false},
		{``, PromptFailed, false},
	} {
		c := NewClient(WithCommandRunner(pickRunner(tc.stdout, "polly_ask")))
		_, err := c.Pick(context.Background(), "s1", "polly_ask", PickRequest{Title: "Sessions"})
		var prompt *PromptError
		if !errors.As(err, &prompt) || prompt.Reason != tc.reason || prompt.Dismissed() != tc.dismissed {
			t.Fatalf("stdout %q: err = %v", tc.stdout, err)
		}
	}
}

func TestPickRejectsWhatRexWould(t *testing.T) {
	c := NewClient(WithCommandRunner(pickRunner(`{"id":"a"}`, "polly_ask")))
	if _, err := c.Pick(context.Background(), "s1", "polly_ask", PickRequest{}); err == nil {
		t.Fatal("a prompt without a title was sent")
	}
	if _, err := c.Pick(context.Background(), "s1", "polly_ask", PickRequest{Title: "x", Items: make([]PickItem, MaxPickItems+1)}); err == nil {
		t.Fatal("a list longer than Rex shows was sent")
	}
}

func TestSelectWindow(t *testing.T) {
	fr := &fakeRunner{}
	c := NewClient(WithCommandRunner(fr), WithEnvLookup(func(string) string { return "" }))
	if err := c.SelectWindow(context.Background(), "client:1", "session:1", "window:1"); err != nil {
		t.Fatal(err)
	}
	call := fr.lastCall()
	if want := []string{"--color", "never", "api", "call", "client.perform", "-"}; !reflect.DeepEqual(call.Args, want) {
		t.Fatalf("args = %v, want %v", call.Args, want)
	}
	var sent struct {
		ClientID string            `json:"client_id"`
		Action   string            `json:"action"`
		Args     map[string]string `json:"args"`
	}
	if err := json.Unmarshal(call.Stdin, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ClientID != "client:1" || sent.Action != "session.select" || sent.Args["session_id"] != "session:1" || sent.Args["window_id"] != "window:1" {
		t.Fatalf("sent %s", call.Stdin)
	}
}

// TestLivePickWithNobodyToAsk performs the real action on a Rex server that
// has no app connected and whose configuration file defines "polly_ask":
// POLLYTOOL_REX_PROMPT_SERVER names the server and
// POLLYTOOL_REX_PROMPT_SESSION a session on it. It shows nothing to anyone:
// the server finds no app to ask, which is as far as a test can follow it.
func TestLivePickWithNobodyToAsk(t *testing.T) {
	server, session := os.Getenv("POLLYTOOL_REX_PROMPT_SERVER"), os.Getenv("POLLYTOOL_REX_PROMPT_SESSION")
	if server == "" || session == "" {
		t.Skip("set POLLYTOOL_REX_PROMPT_SERVER and POLLYTOOL_REX_PROMPT_SESSION to a Rex server with no app connected")
	}
	c := NewClient(WithServer(server), WithAutoStart(false), WithEnvLookup(func(string) string { return "" }))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// With POLLYTOOL_REX_PROMPT_CONFIG naming that server's configuration
	// file, the action tested is the one AskActionLua writes today.
	if config := os.Getenv("POLLYTOOL_REX_PROMPT_CONFIG"); config != "" {
		if err := os.WriteFile(config, []byte(AskActionLua("polly_ask", "Polly: Ask")), 0600); err != nil {
			t.Fatal(err)
		}
		if err := c.Call(ctx, "server.config_reload", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	items := []PickItem{{ID: "a", Title: `quote " backslash \ naïve ✓`, Subtitle: "line\nbreak", Icon: "terminal", Detail: "3m"}, {ID: "b"}}
	_, err := c.Pick(ctx, session, "polly_ask", PickRequest{Title: "Sessions", Timeout: 2 * time.Second, Items: items})
	var prompt *PromptError
	if !errors.As(err, &prompt) || prompt.Reason != PromptNoClient {
		t.Fatalf("Pick with no app = %v, want the server to find no app to ask", err)
	}
	if _, err := c.Input(ctx, session, "polly_ask", InputRequest{Title: "Title", Value: `quote " naïve`, Timeout: 2 * time.Second}); !errors.As(err, &prompt) || prompt.Reason != PromptNoClient {
		t.Fatalf("Input with no app = %v, want the server to find no app to ask", err)
	}
	if _, err := c.Pick(ctx, session, "polly_ask", PickRequest{Title: "Question", AllowOther: true, Timeout: 2 * time.Second, Items: items}); !errors.As(err, &prompt) || prompt.Reason != PromptNoClient {
		t.Fatalf("Pick with typed answers and no app = %v", err)
	}
	if _, err := c.Pick(ctx, session, "polly_no_such_action", PickRequest{Title: "Sessions"}); !errors.Is(err, ErrNoAskAction) {
		t.Fatalf("Pick through an action nobody defined = %v", err)
	}
}
