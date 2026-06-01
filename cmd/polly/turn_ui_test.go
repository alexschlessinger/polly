package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestAppendWarningGoesToStderrNotStdout(t *testing.T) {
	var out, errBuf bytes.Buffer
	ui := &lineTurnUI{config: &Config{}, writer: &out, errWriter: &errBuf}
	ui.AppendWarning("response truncated")
	if strings.Contains(out.String(), "truncated") {
		t.Fatalf("warning must not pollute stdout, got: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "truncated") {
		t.Fatalf("warning should go to stderr, got: %q", errBuf.String())
	}
}
