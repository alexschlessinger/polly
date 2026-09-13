package main

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/urfave/cli/v3"
)

func TestAutomaticContextConfigAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		auto   bool
		budget int
	}{
		{nil, true, 256000}, {[]string{"--maxcontext", "256000"}, false, 256000}, {[]string{"--maxcontext", "0"}, false, 0},
	} {
		cmd := &cli.Command{Flags: historyConfigFlags(), Action: func(_ context.Context, c *cli.Command) error {
			s := parseConfig(c).Launch
			if s.AutoMaxContext != tc.auto || s.MaxHistoryTokens != tc.budget {
				t.Fatalf("%v: %+v", tc.args, s)
			}
			spec, _ := settingSpecFor("maxcontext")
			md := &sessions.Metadata{}
			spec.toMeta(&s, md)
			restored := Settings{}
			spec.fromMeta(&restored, md)
			if restored.AutoMaxContext != s.AutoMaxContext || restored.MaxHistoryTokens != s.MaxHistoryTokens {
				t.Fatalf("restore: %+v", restored)
			}
			return nil
		}}
		if err := cmd.Run(context.Background(), append([]string{"polly"}, tc.args...)); err != nil {
			t.Fatal(err)
		}
	}
	spec, _ := settingSpecFor("maxcontext")
	s := Settings{AutoMaxContext: true}
	spec.fromMeta(&s, &sessions.Metadata{MaxHistoryTokens: 256000})
	if s.AutoMaxContext {
		t.Fatal("legacy numeric setting became automatic")
	}
	if err := spec.parse(&s, "auto"); err != nil || !s.AutoMaxContext {
		t.Fatalf("auto: %+v %v", s, err)
	}
	if err := spec.parse(&s, "0"); err != nil || s.AutoMaxContext || s.MaxHistoryTokens != 0 {
		t.Fatalf("unlimited: %+v %v", s, err)
	}
}

func TestAutomaticContextFollowsDetectedCapacity(t *testing.T) {
	client := &metadataCompletionLLM{window: 1000000}
	agent := llm.NewAgent(client, nil, llm.AgentConfig{})
	defer agent.Close()
	state := &conversationState{agent: agent, settings: Settings{Model: "openai/test", AutoMaxContext: true, MaxHistoryTokens: 256000, MaxTokens: 4096}}
	for _, tc := range []struct{ window, want int }{{1000000, 895904}, {128000, 111104}, {0, 256000}} {
		client.window = tc.window
		if got := resolveContextBudget(context.Background(), state); got != tc.want {
			t.Fatalf("window %d: %d, want %d", tc.window, got, tc.want)
		}
	}
	client.window = 2000
	state.settings.AutoMaxContext = false
	state.settings.MaxHistoryTokens = 64000
	if got := resolveContextBudget(context.Background(), state); got != 1000 {
		t.Fatalf("explicit: %d", got)
	}
	state.settings.MaxHistoryTokens = 0
	if got := resolveContextBudget(context.Background(), state); got != 0 {
		t.Fatalf("unlimited: %d", got)
	}
}
