package main

import (
	"bytes"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func canonicalBash(t *testing.T, command string) string {
	t.Helper()
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := syntax.NewPrinter(syntax.SingleLine(true)).Print(&out, file); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestFormatBashPreservesProgramAndSeparatesStructure(t *testing.T) {
	for _, command := range []string{
		`git status --porcelain | grep -iE '\.(png|jpe?g)$'; echo 'a; b && c | d'; for f in a.png b.png; do echo "$f"; ls -l "$f" | awk '{print $5 " bytes"}'; done`,
		`cd /tmp && FOO=bar go test ./... || echo failed`,
		`for f in *.go; do cat "$f"; done`,
		`if test -f a; then echo yes; elif test -f b; then echo b; else echo no; fi`,
		`while test -f a; do echo waiting; done`,
		`f() { echo one; }; (echo two); case "$x" in a) echo yes;; *) echo no;; esac`,
		"cat <<'EOF' | head -1\na ; b && c\nEOF\necho end",
		"echo one # a comment\necho two | cat",
		`printf '%s\n' "$(echo 'one; two')"`,
		"cd /tmp && printf '%s\\n' 'first\n    literal indentation' | grep first | head -1",
		`cd /tmp && { printf '%s\n' data | grep d | head -1; }`,
		`cat a | grep b | head -1 && echo done || echo failed`,
	} {
		t.Run(command, func(t *testing.T) {
			formatted := formatBashCommand(command)
			if canonicalBash(t, command) != canonicalBash(t, formatted) {
				t.Fatalf("formatting changed the program:\n%s\n=>\n%s", command, formatted)
			}
			if twice := formatBashCommand(formatted); twice != formatted {
				t.Fatalf("formatting is unstable:\n%s\n=>\n%s", formatted, twice)
			}
		})
	}
	if got := formatBashCommand(`for f in *.go; do cat "$f" | head -3; done`); got != "for f in *.go; do\n  cat \"$f\" |\n    head -3\ndone" {
		t.Fatalf("loop/pipeline formatting = %q", got)
	}
	broken := "echo 'unterminated\nkeep this exact text"
	if got := formatBashCommand(broken); got != broken {
		t.Fatalf("invalid shell should retain its original text: %q", got)
	}
}

func TestFormatBashAlignsPipelineContinuations(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{`cd /tmp && grep -rn "swarm_snapshot" --include=*.go . | grep -v gopath | head -20`, "cd /tmp &&\n  grep -rn \"swarm_snapshot\" --include=*.go . |\n  grep -v gopath |\n  head -20"},
		{`for f in *.go; do cat "$f" | grep test | head -20; done`, "for f in *.go; do\n  cat \"$f\" |\n    grep test |\n    head -20\ndone"},
	} {
		if got := formatBashCommand(tc.command); got != tc.want {
			t.Fatalf("pipeline indentation:\n%s\nwant:\n%s", got, tc.want)
		}
	}
}
