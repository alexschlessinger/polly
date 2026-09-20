package main

import (
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
)

func TestResolveOutputCapabilities(t *testing.T) {
	tests := []struct {
		name          string
		mode          conversationMode
		managed       bool
		stdoutTTY     bool
		env           map[string]string
		wantSurface   outputSurface
		wantImage     termimg.Protocol
		wantTruecolor bool
	}{
		{
			name:        "managed TUI keeps existing rich behavior",
			mode:        conversationModeREPL,
			managed:     true,
			stdoutTTY:   true,
			env:         map[string]string{"KITTY_WINDOW_ID": "1", "NO_COLOR": "1"},
			wantSurface: outputSurfaceManagedTUI,
			wantImage:   termimg.ProtocolKitty,
		},
		{
			name:        "one shot TTY renders ANSI and kitty",
			mode:        conversationModeOneShot,
			managed:     true, // The terminal could run the TUI, but this invocation selected one-shot mode.
			stdoutTTY:   true,
			env:         map[string]string{"KITTY_WINDOW_ID": "1"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   termimg.ProtocolKitty,
		},
		{
			name:        "fallback TTY renders ANSI and sixel",
			mode:        conversationModeREPL,
			stdoutTTY:   true,
			env:         map[string]string{"WT_SESSION": "1"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   termimg.ProtocolSixel,
		},
		{
			name:        "redirected stdout is raw despite forced protocol",
			mode:        conversationModeOneShot,
			stdoutTTY:   false,
			env:         map[string]string{"POLLYTOOL_IMAGE_PROTOCOL": "kitty"},
			wantSurface: outputSurfaceLineRaw,
			wantImage:   termimg.ProtocolNone,
		},
		{
			name:        "dumb terminal is raw",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"TERM": "dumb", "KITTY_WINDOW_ID": "1"},
			wantSurface: outputSurfaceLineRaw,
			wantImage:   termimg.ProtocolNone,
		},
		{
			name:        "no color retains terminal rendering and overrides forced graphics",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"NO_COLOR": "1", "POLLYTOOL_IMAGE_PROTOCOL": "sixel"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   termimg.ProtocolNone,
		},
		{
			name:        "multiplexer keeps ANSI but disables graphics",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"TMUX": "/tmp/tmux", "KITTY_WINDOW_ID": "1"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   termimg.ProtocolNone,
		},
		{
			name:          "COLORTERM truecolor advertises 24-bit and the 256 palette",
			mode:          conversationModeOneShot,
			stdoutTTY:     true,
			env:           map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"},
			wantSurface:   outputSurfaceLineANSI,
			wantImage:     termimg.ProtocolNone,
			wantTruecolor: true,
		},
		{
			name:          "COLORTERM direct advertises 24-bit",
			mode:          conversationModeOneShot,
			stdoutTTY:     true,
			env:           map[string]string{"TERM": "xterm-256color", "COLORTERM": "direct"},
			wantSurface:   outputSurfaceLineANSI,
			wantImage:     termimg.ProtocolNone,
			wantTruecolor: true,
		},
		{
			name:          "COLORTERM 24bit advertises 24-bit",
			mode:          conversationModeOneShot,
			stdoutTTY:     true,
			env:           map[string]string{"TERM": "xterm-256color", "COLORTERM": "24bit"},
			wantSurface:   outputSurfaceLineANSI,
			wantImage:     termimg.ProtocolNone,
			wantTruecolor: true,
		},
		{
			name:          "TERM direct suffix advertises 24-bit",
			mode:          conversationModeOneShot,
			stdoutTTY:     true,
			env:           map[string]string{"TERM": "xterm-direct"},
			wantSurface:   outputSurfaceLineANSI,
			wantImage:     termimg.ProtocolNone,
			wantTruecolor: true,
		},
		{
			// Emission branches only on truecolor: a 256-color terminal
			// must not get 38;2 sequences, RGB degrades to 38;5;N there.
			name:        "TERM 256color is not truecolor",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"TERM": "xterm-256color"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   termimg.ProtocolNone,
		},
		{
			name:        "COLORTERM naming 256 is not truecolor",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"TERM": "xterm", "COLORTERM": "256"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   termimg.ProtocolNone,
		},
		{
			name:        "plain xterm keeps the base palette",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"TERM": "xterm"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   termimg.ProtocolNone,
		},
		{
			// The raw and managed-TUI surfaces never emit renderer-owned SGR,
			// so they record the depth only for introspection.
			name:          "managed TUI records the depth it will not emit",
			mode:          conversationModeREPL,
			managed:       true,
			stdoutTTY:     true,
			env:           map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"},
			wantSurface:   outputSurfaceManagedTUI,
			wantImage:     termimg.ProtocolNone,
			wantTruecolor: true,
		},
		{
			name:          "redirected stdout records the depth it will not emit",
			mode:          conversationModeOneShot,
			stdoutTTY:     false,
			env:           map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"},
			wantSurface:   outputSurfaceLineRaw,
			wantImage:     termimg.ProtocolNone,
			wantTruecolor: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveOutputCapabilities(tt.mode, tt.managed, tt.stdoutTTY, 132, mapGetenv(tt.env))
			if got.surface != tt.wantSurface || got.imageProtocol != tt.wantImage || got.columns != 132 {
				t.Fatalf("capabilities = %#v, want surface=%v image=%v columns=132", got, tt.wantSurface, tt.wantImage)
			}
			if got.truecolor != tt.wantTruecolor {
				t.Fatalf("color depth = truecolor:%v, want truecolor:%v", got.truecolor, tt.wantTruecolor)
			}
		})
	}
}

func TestOutputCapabilitiesLineColors(t *testing.T) {
	tests := []struct {
		name   string
		caps   outputCapabilities
		colors lineColorCapabilities
	}{
		{
			name:   "rich truecolor surface",
			caps:   outputCapabilities{surface: outputSurfaceLineANSI, truecolor: true},
			colors: lineColorCapabilities{enabled: true, truecolor: true},
		},
		{
			// A 256-color surface without truecolor degrades RGB to 38;5;N,
			// which is what lineColorCapabilities records: emission branches
			// only on truecolor.
			name:   "rich 256 color surface",
			caps:   outputCapabilities{surface: outputSurfaceLineANSI},
			colors: lineColorCapabilities{enabled: true},
		},
		{
			name:   "NO_COLOR disables SGR but not the recorded depth",
			caps:   outputCapabilities{surface: outputSurfaceLineANSI, noColor: true, truecolor: true},
			colors: lineColorCapabilities{truecolor: true},
		},
		{
			name: "raw surface disables SGR",
			caps: outputCapabilities{surface: outputSurfaceLineRaw},
		},
		{
			name:   "managed TUI surface disables SGR",
			caps:   outputCapabilities{surface: outputSurfaceManagedTUI, truecolor: true},
			colors: lineColorCapabilities{truecolor: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.caps.lineColors(); got != tt.colors {
				t.Fatalf("lineColors() = %+v, want %+v", got, tt.colors)
			}
		})
	}
}

func TestResolveLineStatusCapabilitiesColorDepth(t *testing.T) {
	tests := []struct {
		name          string
		tty           bool
		env           map[string]string
		wantColor     bool
		wantTruecolor bool
	}{
		{"truecolor terminal", true, map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"}, true, true},
		{"256 color terminal", true, map[string]string{"TERM": "xterm-256color"}, true, false},
		{"base palette terminal", true, map[string]string{"TERM": "xterm"}, true, false},
		{"no color keeps the recorded depth", true, map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "NO_COLOR": "1"}, false, true},
		{"redirected stderr keeps the recorded depth", false, map[string]string{"TERM": "xterm-256color"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveLineStatusCapabilities(tt.tty, 80, mapGetenv(tt.env))
			if got.color != tt.wantColor || got.truecolor != tt.wantTruecolor {
				t.Fatalf("status capabilities = %+v", got)
			}
		})
	}
}

func TestResolveOutputCapabilitiesUsesDefaultWidth(t *testing.T) {
	got := resolveOutputCapabilities(conversationModeOneShot, false, true, 0, mapGetenv(nil))
	if got.columns != 80 {
		t.Fatalf("columns = %d, want 80", got.columns)
	}
}

func mapGetenv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}
