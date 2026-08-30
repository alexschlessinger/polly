package main

import "testing"

func TestResolveOutputCapabilities(t *testing.T) {
	tests := []struct {
		name        string
		mode        conversationMode
		managed     bool
		stdoutTTY   bool
		env         map[string]string
		wantSurface outputSurface
		wantImage   terminalImageProtocol
	}{
		{
			name:        "managed TUI keeps existing rich behavior",
			mode:        conversationModeREPL,
			managed:     true,
			stdoutTTY:   true,
			env:         map[string]string{"KITTY_WINDOW_ID": "1", "NO_COLOR": "1"},
			wantSurface: outputSurfaceManagedTUI,
			wantImage:   terminalImageKitty,
		},
		{
			name:        "one shot TTY renders ANSI and kitty",
			mode:        conversationModeOneShot,
			managed:     true, // The terminal could run the TUI, but this invocation selected one-shot mode.
			stdoutTTY:   true,
			env:         map[string]string{"KITTY_WINDOW_ID": "1"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   terminalImageKitty,
		},
		{
			name:        "fallback TTY renders ANSI and sixel",
			mode:        conversationModeREPL,
			stdoutTTY:   true,
			env:         map[string]string{"WT_SESSION": "1"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   terminalImageSixel,
		},
		{
			name:        "redirected stdout is raw despite forced protocol",
			mode:        conversationModeOneShot,
			stdoutTTY:   false,
			env:         map[string]string{"POLLYTOOL_IMAGE_PROTOCOL": "kitty"},
			wantSurface: outputSurfaceLineRaw,
			wantImage:   terminalImageNone,
		},
		{
			name:        "dumb terminal is raw",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"TERM": "dumb", "KITTY_WINDOW_ID": "1"},
			wantSurface: outputSurfaceLineRaw,
			wantImage:   terminalImageNone,
		},
		{
			name:        "no color is raw and overrides forced graphics",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"NO_COLOR": "1", "POLLYTOOL_IMAGE_PROTOCOL": "sixel"},
			wantSurface: outputSurfaceLineRaw,
			wantImage:   terminalImageNone,
		},
		{
			name:        "multiplexer keeps ANSI but disables graphics",
			mode:        conversationModeOneShot,
			stdoutTTY:   true,
			env:         map[string]string{"TMUX": "/tmp/tmux", "KITTY_WINDOW_ID": "1"},
			wantSurface: outputSurfaceLineANSI,
			wantImage:   terminalImageNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveOutputCapabilities(tt.mode, tt.managed, tt.stdoutTTY, 132, mapGetenv(tt.env))
			if got.surface != tt.wantSurface || got.imageProtocol != tt.wantImage || got.columns != 132 {
				t.Fatalf("capabilities = %#v, want surface=%v image=%v columns=132", got, tt.wantSurface, tt.wantImage)
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
