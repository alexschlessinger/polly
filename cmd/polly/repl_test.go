package main

import (
	"bufio"
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestSelectConversationMode(t *testing.T) {
	tests := []struct {
		name           string
		config         Config
		stdinAvailable bool
		want           conversationMode
		wantErr        string
	}{
		{
			name: "prompt flag chooses one shot even when empty",
			config: Config{
				Prompt:    "",
				PromptSet: true,
				Files:     []string{"README.md"},
			},
			want: conversationModeOneShot,
		},
		{
			name:           "stdin chooses one shot",
			config:         Config{},
			stdinAvailable: true,
			want:           conversationModeOneShot,
		},
		{
			name:   "bare command chooses repl",
			config: Config{},
			want:   conversationModeREPL,
		},
		{
			name: "file without prompt source is rejected",
			config: Config{
				Files: []string{"README.md"},
			},
			wantErr: "--file requires -p or stdin",
		},
		{
			name: "schema without prompt source is rejected",
			config: Config{
				SchemaPath: "person.schema.json",
			},
			wantErr: "--schema requires -p or stdin",
		},
		{
			name: "file and schema without prompt source are rejected",
			config: Config{
				Files:      []string{"README.md"},
				SchemaPath: "person.schema.json",
			},
			wantErr: "--file and --schema require -p or stdin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectConversationMode(&tt.config, tt.stdinAvailable)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("selectConversationMode() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectConversationMode() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("selectConversationMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunREPLLoop(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantTurns  []string
		wantPrompt string
	}{
		{
			name:       "blank lines are ignored",
			input:      "\nhello\n/exit\n",
			wantTurns:  []string{"hello"},
			wantPrompt: "> > > ",
		},
		{
			name:       "quit command exits",
			input:      "first\n/quit\n",
			wantTurns:  []string{"first"},
			wantPrompt: "> > ",
		},
		{
			name:       "eof exits cleanly",
			input:      "final turn",
			wantTurns:  []string{"final turn"},
			wantPrompt: "> > ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			var promptBuf bytes.Buffer
			var turns []string

			err := runREPLLoop(reader, &promptBuf, func(prompt string) error {
				turns = append(turns, prompt)
				return nil
			})
			if err != nil {
				t.Fatalf("runREPLLoop() error = %v", err)
			}
			if !reflect.DeepEqual(turns, tt.wantTurns) {
				t.Fatalf("runREPLLoop() turns = %#v, want %#v", turns, tt.wantTurns)
			}
			if got := promptBuf.String(); got != tt.wantPrompt {
				t.Fatalf("runREPLLoop() prompts = %q, want %q", got, tt.wantPrompt)
			}
		})
	}
}

func TestRunREPLLoopWithStatusBar(t *testing.T) {
	forceStatusBarTERM(t)
	var barOut bytes.Buffer
	bar := newStatusBar(&barOut, &fakeSizer{w: 80, h: 24})
	bar.SetModel("claude-sonnet-4-6")
	bar.SetContext("itest")
	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}

	reader := bufio.NewReader(strings.NewReader("hello\n/exit\n"))
	var turns []string

	err := runREPLLoop(reader, bar.ContentWriter(), func(prompt string) error {
		turns = append(turns, prompt)
		bar.Start()
		bar.ClearForContent()
		bar.RecordTurnTokens(1234, 380)
		bar.Stop()
		return nil
	})
	if err != nil {
		t.Fatalf("runREPLLoop: %v", err)
	}
	bar.Uninstall()

	if !reflect.DeepEqual(turns, []string{"hello"}) {
		t.Fatalf("turns = %v, want [hello]", turns)
	}
	if !strings.Contains(barOut.String(), "streaming") {
		t.Errorf("expected streaming state to have been painted")
	}
}
