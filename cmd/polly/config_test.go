package main

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestValidateModel(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		wantErr string
	}{
		{name: "empty uses default", model: ""},
		{name: "known provider", model: "openai/gpt-5.4"},
		{name: "missing provider prefix", model: "gpt-5.4", wantErr: "model must include provider prefix"},
		{name: "unknown provider", model: "custom/model", wantErr: "unknown provider 'custom'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateModel(tt.model)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateModel(%q) error = %v", tt.model, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateModel(%q) error = %v, want substring %q", tt.model, err, tt.wantErr)
			}
		})
	}
}

func TestValidateTemperature(t *testing.T) {
	tests := []struct {
		name    string
		temp    float64
		wantErr string
	}{
		{name: "lower bound", temp: 0.0},
		{name: "upper bound", temp: 2.0},
		{name: "below range", temp: -0.1, wantErr: "temperature must be between 0.0 and 2.0"},
		{name: "above range", temp: 2.1, wantErr: "temperature must be between 0.0 and 2.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTemperature(tt.temp)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTemperature(%v) error = %v", tt.temp, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateTemperature(%v) error = %v, want substring %q", tt.temp, err, tt.wantErr)
			}
		})
	}
}

func TestConfigFlagsRejectPromptAndFileOnManagementCommands(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "reset rejects prompt",
			args:    []string{"--reset", "ctx", "--prompt", "hello"},
			wantErr: "--reset does not take prompts or files",
		},
		{
			name:    "listskills rejects file",
			args:    []string{"--listskills", "--file", "README.md"},
			wantErr: "--listskills does not take prompts or files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runConfigValidationCommand(tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigFlagsRejectPurgeWithOtherFlags(t *testing.T) {
	err := runConfigValidationCommand("--purge", "--model", "openai/gpt-5.4")
	if err == nil || !strings.Contains(err.Error(), "--purge must be used alone") {
		t.Fatalf("run error = %v, want purge validation error", err)
	}
}

func TestDefineFlagsWithGroupsContextManagementMutex(t *testing.T) {
	_, groups := defineFlagsWithGroups()
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}

	got := make([]string, 0, len(groups[0].Flags))
	for _, flagSet := range groups[0].Flags {
		if len(flagSet) != 1 {
			t.Fatalf("len(flagSet) = %d, want 1", len(flagSet))
		}
		got = append(got, flagSet[0].Names()[0])
	}

	want := []string{"reset", "purge", "create", "show", "list", "delete", "add"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func runConfigValidationCommand(args ...string) error {
	flags, groups := defineFlagsWithGroups()
	cmd := &cli.Command{
		Name:                   "polly",
		Flags:                  flags,
		MutuallyExclusiveFlags: groups,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return nil
		},
	}

	return cmd.Run(context.Background(), append([]string{"polly"}, args...))
}
