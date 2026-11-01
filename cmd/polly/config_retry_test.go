package main

import (
	"context"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/urfave/cli/v3"
)

func TestRetryConfigurationReachesRequests(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		args                  []string
		envRetries, envBudget string
		wantRetries           int
		wantBudget            time.Duration
		invalid               bool
	}{
		{name: "defaults", wantRetries: 12, wantBudget: 5 * time.Minute},
		{name: "flags", args: []string{"--retries=3", "--retry-budget=20s"}, wantRetries: 3, wantBudget: 20 * time.Second},
		{name: "disable", args: []string{"--retries=0"}, wantBudget: 5 * time.Minute},
		{name: "environment", envRetries: "6", envBudget: "2m", wantRetries: 6, wantBudget: 2 * time.Minute},
		{name: "negative retries", args: []string{"--retries=-1"}, invalid: true},
		{name: "zero budget", args: []string{"--retry-budget=0s"}, invalid: true},
		{name: "negative budget", args: []string{"--retry-budget=-1s"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POLLYTOOL_RETRIES", tc.envRetries)
			t.Setenv("POLLYTOOL_RETRY_BUDGET", tc.envBudget)
			var config *Config
			cmd := &cli.Command{Flags: modelConfigFlags(), Action: func(_ context.Context, cmd *cli.Command) error { config = parseConfig(cmd); return nil }}
			err := cmd.Run(context.Background(), append([]string{"polly"}, tc.args...))
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid retry policy accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			registry := tools.NewToolRegistry(nil)
			defer registry.Close()
			req := createCompletionRequest(config, &config.Launch, nil, registry, nil, nil)
			if req.RetryPolicy == nil || req.RetryPolicy.MaxRetries != tc.wantRetries || req.RetryPolicy.MaxElapsed != tc.wantBudget || req.RetryPolicy.MaxDelay != llm.DefaultRetryPolicy().MaxDelay {
				t.Fatalf("request policy = %+v", req.RetryPolicy)
			}
		})
	}
}
