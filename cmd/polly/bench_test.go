//go:build bench

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// pollyBin is the path to a freshly-built polly binary, populated by TestMain.
var pollyBin string

// providerModel ties a provider name to a concrete model and the env var that
// supplies its API key. Used to expand cross-provider cases (see readmeCase).
type providerModel struct {
	name   string // short tag used in subtest names: "openai", "anthropic", "gemini"
	model  string // fully-qualified model string passed to `polly -m ...`
	keyEnv string // env var name that must be set for this provider
}

// providers enumerates the three providers README examples target. The models
// mirror the ones the README advertises. Update in lockstep with README.md.
var providers = []providerModel{
	{"openai", "openai/gpt-5.4-mini", "POLLYTOOL_OPENAIKEY"},
	{"anthropic", "anthropic/claude-sonnet-4-6", "POLLYTOOL_ANTHROPICKEY"},
	{"gemini", "gemini/gemini-3.1-flash-lite-preview", "POLLYTOOL_GEMINIKEY"},
	{"huggingface", "huggingface/zai-org/GLM-5.1:together", "POLLYTOOL_HUGGINGFACEKEY"},
}

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "polly-readme-*")
	if err != nil {
		panic("failed to create temp dir for polly binary: " + err.Error())
	}
	pollyBin = filepath.Join(tmp, "polly")

	build := exec.Command("go", "build", "-o", pollyBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		_ = os.RemoveAll(tmp)
		panic("failed to build polly binary: " + err.Error())
	}

	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// readmeCase represents a single invocation drawn from README.md. The test runs
// each one against a freshly-built polly binary with an isolated HOME so that
// context-store operations do not touch the user's real ~/.pollytool directory.
type readmeCase struct {
	name string
	// args are passed to polly verbatim after the setup() hook optionally
	// splices in absolute paths for files it created in the isolated HOME.
	args []string
	// stdin is fed to polly on its stdin.
	stdin string
	// needsEnv lists environment variable names that must be set in the parent
	// shell for this case to be exercised. Missing keys trigger t.Skip.
	needsEnv []string
	// needsBin lists binaries that must be on $PATH for this case to run.
	// Missing binaries trigger t.Skip. Useful for sandbox (bwrap/sandbox-exec)
	// and shell-tool helpers (jq).
	needsBin []string
	// needsSandbox requires a working sandbox backend (bwrap on Linux,
	// sandbox-exec on macOS). Triggers t.Skip on unsupported platforms.
	needsSandbox bool
	// setup runs before args are expanded. It returns replacements applied to
	// any element of args equal to a "{key}" placeholder, so each case can
	// materialise files/dirs in its own scratch tempdir.
	setup func(t *testing.T, home string) map[string]string
	// extraEnv is merged onto the subprocess environment. Used mostly to point
	// HOME at an isolated tempdir for context-store isolation.
	extraEnv map[string]string
	// wantStdoutNonEmpty asserts the subprocess produced at least some stdout.
	// False for pure state-mutation cases (delete, reset) that may legitimately
	// produce no output beyond a confirmation line.
	wantStdoutNonEmpty bool
	// stdoutContains, if non-empty, asserts that each substring appears in the
	// subprocess stdout (case-sensitive). Used to verify that tool-call results
	// actually reached the user, complementing wantStdoutNonEmpty.
	stdoutContains []string
	// extraCheck, if non-nil, runs after the generic pass/fail checks and can
	// assert additional properties on stdout/stderr (e.g. JSON schema shape).
	// `home` is the isolated tempdir HOME the case ran under, so checks can
	// re-invoke polly against the same context store for multi-step tests.
	extraCheck func(t *testing.T, stdout, stderr, home string)
	// timeout caps subprocess wall time. Defaults to 60s if zero.
	timeout time.Duration
	// maxTokensOverride, if non-empty, replaces the default "--maxtokens 200"
	// safety cap that we append to API-calling cases. Set it to "skip" to not
	// append anything (useful when the test case already sets --maxtokens).
	maxTokensOverride string
	// crossProvider expands this case into one subtest per entry in the
	// package-level `providers` slice (openai, anthropic, gemini). The runner
	// prepends `-m <model>` to args and prepends the provider's key env to
	// needsEnv so missing keys skip cleanly. Leave false for cases that pin a
	// specific provider explicitly via args or that make no LLM call.
	crossProvider bool
}

func (c readmeCase) run(t *testing.T) {
	t.Helper()

	for _, env := range c.needsEnv {
		if os.Getenv(env) == "" {
			t.Skipf("skipping: %s not set", env)
		}
	}
	for _, bin := range c.needsBin {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("skipping: %q not on PATH", bin)
		}
	}
	if c.needsSandbox && !sandboxAvailable() {
		t.Skipf("skipping: no sandbox backend (need bwrap on linux, sandbox-exec on darwin)")
	}

	home := t.TempDir()
	var replacements map[string]string
	if c.setup != nil {
		replacements = c.setup(t, home)
	}

	// Expand {placeholder} tokens in args with paths registered by setup().
	args := make([]string, 0, len(c.args))
	for _, a := range c.args {
		if v, ok := replacements[a]; ok {
			args = append(args, v)
			continue
		}
		args = append(args, a)
	}

	// Append a safety cap on token generation for any case that isn't already
	// setting --maxtokens or explicitly opting out. Keeps each live API call
	// cheap enough to run repeatedly.
	if c.maxTokensOverride != "skip" && !containsFlag(args, "--maxtokens") {
		cap := c.maxTokensOverride
		if cap == "" {
			cap = "200"
		}
		args = append(args, "--maxtokens", cap)
	}

	timeout := c.timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, pollyBin, args...)
	cmd.Stdin = strings.NewReader(c.stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Base env: pass through API keys and PATH, pin HOME to our tempdir so
	// --create/--list/etc. can't clobber the user's real context store.
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
	for _, k := range []string{
		"POLLYTOOL_ANTHROPICKEY",
		"POLLYTOOL_OPENAIKEY",
		"POLLYTOOL_GEMINIKEY",
		"POLLYTOOL_OLLAMAKEY",
		"POLLYTOOL_HUGGINGFACEKEY",
	} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range c.extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	err := cmd.Run()
	if err != nil {
		t.Fatalf("polly %s: exit error: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("polly %s: timed out after %s", strings.Join(args, " "), timeout)
	}
	if c.wantStdoutNonEmpty && strings.TrimSpace(stdout.String()) == "" {
		t.Fatalf("polly %s: expected non-empty stdout\nstderr:\n%s",
			strings.Join(args, " "), stderr.String())
	}
	for _, want := range c.stdoutContains {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("polly %s: stdout missing expected substring %q\nstdout:\n%s\nstderr:\n%s",
				strings.Join(args, " "), want, stdout.String(), stderr.String())
		}
	}
	if c.extraCheck != nil {
		c.extraCheck(t, stdout.String(), stderr.String(), home)
	}
}

func containsFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

// sandboxAvailable reports whether a sandbox backend exists on this host.
// Mirrors the check in tools/sandbox/sandbox_{linux,darwin}.go: bwrap on
// Linux, sandbox-exec on macOS. Any other OS has no backend.
func sandboxAvailable() bool {
	switch runtime.GOOS {
	case "linux":
		_, err := exec.LookPath("bwrap")
		return err == nil
	case "darwin":
		_, err := exec.LookPath("sandbox-exec")
		return err == nil
	}
	return false
}

// schemaJSON is the person schema used in the README's Structured Output section.
const schemaJSON = `{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "age": {"type": "integer"},
    "email": {"type": "string"}
  },
  "required": ["name", "age"]
}`

// uppercaseTool is a minimal shell tool matching the README's Shell Tools example.
const uppercaseTool = `#!/bin/bash
if [ "$1" = "--schema" ]; then
  cat <<'SCHEMA'
{
  "title": "uppercase",
  "description": "Convert text to uppercase",
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Text to convert"}
  },
  "required": ["text"]
}
SCHEMA
elif [ "$1" = "--execute" ]; then
  echo "$2" | jq -r .text | tr '[:lower:]' '[:upper:]'
fi
`

// sandboxedUppercaseTool mirrors the Sandboxing section's full runnable example
// (README "Full example — default sandbox"). "sandbox": true gives the tool
// temp-dir-only writes, no network, and stripped POLLYTOOL_* env vars.
const sandboxedUppercaseTool = `#!/bin/bash
if [ "$1" = "--schema" ]; then
  cat <<'SCHEMA'
{
  "title": "sandboxed_uppercase",
  "description": "Uppercase input text",
  "type": "object",
  "sandbox": true,
  "properties": {"text": {"type": "string"}},
  "required": ["text"]
}
SCHEMA
elif [ "$1" = "--execute" ]; then
  echo "$2" | jq -r .text | tr '[:lower:]' '[:upper:]'
fi
`

// denyWriteTool mirrors the README's "Fully read-only" sandbox config variation.
// denyWrite: true blocks all writes, even to temp dirs. The probe attempts a
// real write under /tmp and reports whether the sandbox blocked it — that's
// the actual signal we want the test to gate on, not just "tool ran".
const denyWriteTool = `#!/bin/bash
if [ "$1" = "--schema" ]; then
  cat <<'SCHEMA'
{
  "title": "denywrite_probe",
  "description": "Echoes the given text and reports whether writes are blocked.",
  "type": "object",
  "sandbox": { "denyWrite": true },
  "properties": {"text": {"type": "string"}},
  "required": ["text"]
}
SCHEMA
elif [ "$1" = "--execute" ]; then
  text=$(echo "$2" | jq -r .text)
  probe="/tmp/polly-denywrite-probe-$$"
  if echo x > "$probe" 2>/dev/null; then
    write_status="WRITE_ALLOWED"
    rm -f "$probe"
  else
    write_status="WRITE_BLOCKED"
  fi
  echo "text=$text status=$write_status"
fi
`

// phraseEmitterSkillMD is the SKILL.md frontmatter+body for the end-to-end
// skills test fixture. Auto-activated via --skill, so the bundled say tool
// is registered before the model's first turn.
const phraseEmitterSkillMD = `---
name: phrase-emitter
description: Use this skill whenever the user asks about the special phrase. It exposes a say tool that returns the phrase verbatim.
---
Whenever the user asks "what is the special phrase" (or any close paraphrase),
call the bundled say tool and relay its exact output back to the user. The
tool takes no arguments. Do not invent or guess the phrase yourself — only
this tool knows the real value.
`

// phraseEmitterScript is the say.sh body. The sentinel is intentionally
// unguessable so a passing assertion proves the skill's bundled script
// actually ran — a model that fabricates a phrase will never match it.
const phraseEmitterScript = `#!/bin/sh
if [ "$1" = "--schema" ]; then
  printf '%s\n' '{"title":"say","description":"Emit the special phrase","type":"object","properties":{},"required":[]}'
  exit 0
fi
if [ "$1" = "--execute" ]; then
  printf '%s\n' 'SKILLPROOF_X9Q7Z'
  exit 0
fi
exit 1
`

// writeFile is a small helper for setup() closures.
func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestReadmeExamples(t *testing.T) {
	cases := []readmeCase{
		// --- Quick Start: Basic (README:73) ---
		{
			name:               "quickstart_basic_default_model",
			args:               []string{"--quiet"},
			stdin:              "Say hello in under ten words.",
			needsEnv:           []string{"POLLYTOOL_ANTHROPICKEY"},
			wantStdoutNonEmpty: true,
		},

		// --- Quick Start: Pick a model (README:76) ---
		{
			name:               "quickstart_openai_model",
			args:               []string{"-m", "openai/gpt-5.4", "--quiet"},
			stdin:              "Say hello in under ten words.",
			needsEnv:           []string{"POLLYTOOL_OPENAIKEY"},
			wantStdoutNonEmpty: true,
		},

		// --- Model Selection: opus 4.7 (new, exercises adaptive/no-temp path) ---
		{
			name:               "model_selection_opus_4_7",
			args:               []string{"-m", "anthropic/claude-opus-4-7", "-p", "Say hello in under ten words.", "--quiet"},
			needsEnv:           []string{"POLLYTOOL_ANTHROPICKEY"},
			wantStdoutNonEmpty: true,
		},

		// --- Contexts: create (README:104) ---
		{
			name: "contexts_create",
			args: []string{
				"--create", "readmetest",
				"--model", "anthropic/claude-sonnet-4-6",
				"--maxtokens", "4096",
			},
			maxTokensOverride:  "skip", // --create takes --maxtokens as config, don't double-up
			wantStdoutNonEmpty: false,  // --create prints a confirmation but not strictly required
		},

		// --- Contexts: create + show (README:107) ---
		{
			name: "contexts_show",
			args: []string{"--show", "readmetest"},
			setup: func(t *testing.T, home string) map[string]string {
				preCreateContext(t, home, "readmetest", "anthropic/claude-sonnet-4-6")
				return nil
			},
			maxTokensOverride:  "skip",
			wantStdoutNonEmpty: true,
		},

		// --- Contexts: continue via -p (README:113) ---
		{
			name:     "contexts_use_via_prompt",
			args:     []string{"-c", "readmetest", "-p", "Say hello in under ten words.", "--quiet"},
			needsEnv: []string{"POLLYTOOL_ANTHROPICKEY"},
			setup: func(t *testing.T, home string) map[string]string {
				preCreateContext(t, home, "readmetest", "anthropic/claude-sonnet-4-6")
				return nil
			},
			wantStdoutNonEmpty: true,
		},

		// --- Contexts: list (README:119) ---
		{
			name: "contexts_list",
			args: []string{"--list"},
			setup: func(t *testing.T, home string) map[string]string {
				preCreateContext(t, home, "readmetest", "anthropic/claude-sonnet-4-6")
				return nil
			},
			maxTokensOverride:  "skip",
			wantStdoutNonEmpty: true,
		},

		// --- Contexts: list (empty) ---
		{
			name:               "contexts_list_empty",
			args:               []string{"--list"},
			maxTokensOverride:  "skip",
			wantStdoutNonEmpty: true, // prints "No contexts found"
		},

		// --- Contexts: delete (README:122) ---
		{
			name: "contexts_delete",
			args: []string{"--delete", "readmetest"},
			setup: func(t *testing.T, home string) map[string]string {
				preCreateContext(t, home, "readmetest", "anthropic/claude-sonnet-4-6")
				return nil
			},
			maxTokensOverride: "skip",
		},

		// --- Contexts: reset by name (README:116) ---
		{
			name: "contexts_reset",
			args: []string{"--reset", "readmetest"},
			setup: func(t *testing.T, home string) map[string]string {
				preCreateContext(t, home, "readmetest", "anthropic/claude-sonnet-4-6")
				return nil
			},
			maxTokensOverride: "skip",
		},

		// --- Context Settings Persistence (README:134) ---
		// First invocation seeds the "helper" context with -s; the extraCheck
		// re-runs polly under the same HOME with --show to confirm the
		// system prompt was persisted to the context store.
		{
			name: "contexts_gemini_persistence_first_use",
			args: []string{
				"-c", "helper",
				"-m", "gemini/gemini-3.1-pro-preview",
				"-s", "You are a SQL expert",
				"-p", "Say hello in under ten words.",
				"--quiet",
			},
			needsEnv:           []string{"POLLYTOOL_GEMINIKEY"},
			wantStdoutNonEmpty: true,
			extraCheck: func(t *testing.T, _, _, home string) {
				show := runPollyLocal(t, home, "--show", "helper")
				if !strings.Contains(show, "System Prompt: You are a SQL expert") {
					t.Fatalf("expected persisted system prompt in --show output:\n%s", show)
				}
				if !strings.Contains(show, "Model: gemini/gemini-3.1-pro-preview") {
					t.Fatalf("expected persisted model in --show output:\n%s", show)
				}
			},
		},

		// --- Agent Skills: list (README:218) ---
		{
			name:              "skills_listskills_empty_default",
			args:              []string{"--listskills"},
			maxTokensOverride: "skip",
		},

		// --- Agent Skills: end-to-end via --skill auto-activation (README:208) ---
		// Materializes a one-script skill on disk, points polly at it via
		// --skill (auto-activates so the bundled say tool is registered up
		// front), and asserts the unique sentinel emitted by say.sh appears
		// in stdout. Proves: skill discovery → activation → bundled-script
		// registration as a polly tool → model invocation → output relayed
		// to the user. A model that skips the tool or fabricates a phrase
		// will fail because the sentinel is unguessable.
		{
			name: "skills_phrase_emitter",
			args: []string{
				"--skill", "{SKILL}",
				// Prompt is intentionally vague about *which* tool to use —
				// the test's whole point is that the skill's description
				// guides the model to its bundled tool. Naming the tool
				// directly would just be a tool-routing test.
				"-p", "What is the special phrase? Reply with only the phrase, nothing else.",
				"--nosandbox",
				"--quiet",
			},
			setup: func(t *testing.T, home string) map[string]string {
				skillDir := filepath.Join(home, "skills", "phrase-emitter")
				if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755); err != nil {
					t.Fatalf("mkdir skill dir: %v", err)
				}
				writeFile(t, filepath.Join(skillDir, "SKILL.md"), phraseEmitterSkillMD, 0644)
				writeFile(t, filepath.Join(skillDir, "scripts", "say.sh"), phraseEmitterScript, 0755)
				return map[string]string{"{SKILL}": skillDir}
			},
			crossProvider: true,
			// Gemini 3.x burns thinking tokens before any output; the default
			// 200-token cap truncates before the tool result reaches stdout.
			maxTokensOverride:  "2000",
			wantStdoutNonEmpty: true,
			stdoutContains:     []string{"SKILLPROOF_X9Q7Z"},
			timeout:            90 * time.Second,
		},

		// --- Structured Output: schema (README:249) — run across all providers ---
		{
			name:  "structured_output_person_schema",
			args:  []string{"--schema", "{SCHEMA}", "--quiet"},
			stdin: "John Doe is 30 years old, email: john@example.com",
			setup: func(t *testing.T, home string) map[string]string {
				p := filepath.Join(home, "person.schema.json")
				writeFile(t, p, schemaJSON, 0644)
				return map[string]string{"{SCHEMA}": p}
			},
			crossProvider: true,
			// Bumped from the harness default (200) because Gemini 3.x burns
			// most of the budget on thinking tokens before emitting the JSON
			// payload; 200 truncates mid-thought, leaving the test with no
			// JSON to validate.
			maxTokensOverride:  "2000",
			wantStdoutNonEmpty: true,
			// The schema input has age as an integer and states "John Doe is
			// 30". Parsing into the struct below simultaneously checks that
			// stdout is valid JSON and that age came back as an integer, not
			// a float or string — a real schema violation would fail to
			// unmarshal into `int`.
			extraCheck: func(t *testing.T, stdout, _, _ string) {
				var got struct {
					Name  string `json:"name"`
					Age   int    `json:"age"`
					Email string `json:"email"`
				}
				trimmed := strings.TrimSpace(stdout)
				if err := json.Unmarshal([]byte(trimmed), &got); err != nil {
					t.Fatalf("stdout is not valid JSON: %v\nstdout:\n%s", err, stdout)
				}
				if !strings.Contains(strings.ToLower(got.Name), "john") {
					t.Errorf("expected name containing 'john' (case-insensitive), got %q", got.Name)
				}
				if got.Age != 30 {
					t.Errorf("expected age=30, got %d", got.Age)
				}
			},
		},

		// --- Shell Tools: custom shell tool (README:174, 298) — all providers ---
		{
			name: "shell_tool_uppercase",
			args: []string{
				"-t", "{TOOL}",
				"-p", "Use the uppercase tool to convert 'hello' to uppercase, then reply with the tool output verbatim.",
				"--nosandbox",
				"--quiet",
			},
			setup: func(t *testing.T, home string) map[string]string {
				p := filepath.Join(home, "uppercase.sh")
				writeFile(t, p, uppercaseTool, 0755)
				return map[string]string{"{TOOL}": p}
			},
			crossProvider:      true,
			needsBin:           []string{"jq"}, // the tool's --execute branch pipes through jq
			wantStdoutNonEmpty: true,
			stdoutContains:     []string{"HELLO"}, // proves the tool actually fired and its output reached the user
			timeout:            90 * time.Second,  // tool calls can add latency
		},

		// --- Sandboxing: default sandbox (README "Full example — default sandbox") — all providers ---
		{
			name: "sandbox_default",
			args: []string{
				"-t", "{TOOL}",
				"-p", "Use the sandboxed_uppercase tool to convert 'hello world' to uppercase, then reply with the tool output verbatim.",
				"--quiet",
			},
			setup: func(t *testing.T, home string) map[string]string {
				p := filepath.Join(home, "sandboxed_uppercase.sh")
				writeFile(t, p, sandboxedUppercaseTool, 0755)
				return map[string]string{"{TOOL}": p}
			},
			crossProvider:      true,
			needsBin:           []string{"jq"},
			needsSandbox:       true,
			wantStdoutNonEmpty: true,
			stdoutContains:     []string{"HELLO WORLD"},
			timeout:            90 * time.Second,
		},

		// --- Sandboxing: denyWrite variation (README "Fully read-only") — all providers ---
		// The probe tool tries a real write under /tmp and reports the result;
		// asserting WRITE_BLOCKED in stdout proves denyWrite actually took
		// effect inside the sandbox (not just that the tool ran).
		{
			name: "sandbox_deny_write",
			args: []string{
				"-t", "{TOOL}",
				"-p", "Use the denywrite_probe tool with text 'sandboxtest', then reply with the tool output verbatim.",
				"--quiet",
			},
			setup: func(t *testing.T, home string) map[string]string {
				p := filepath.Join(home, "denywrite_probe.sh")
				writeFile(t, p, denyWriteTool, 0755)
				return map[string]string{"{TOOL}": p}
			},
			crossProvider: true,
			needsBin:      []string{"jq"},
			needsSandbox:  true,
			// Gemini 3.x burns most of a 200-token budget on thinking before
			// the tool result reaches stdout — bump for headroom.
			maxTokensOverride:  "2000",
			wantStdoutNonEmpty: true,
			stdoutContains:     []string{"sandboxtest", "WRITE_BLOCKED"},
			timeout:            90 * time.Second,
		},
	}

	only := os.Getenv("BENCH_PROVIDER")
	for _, tc := range cases {
		if !tc.crossProvider {
			// When BENCH_PROVIDER pins a single provider, skip cases that
			// aren't cross-provider — they either hard-code a different
			// provider or make no LLM call at all.
			if only != "" {
				continue
			}
			t.Run(tc.name, tc.run)
			continue
		}
		// Expand the case into one subtest per provider. Each fork takes a
		// fresh copy of args/needsEnv so the loop doesn't accumulate.
		for _, p := range providers {
			if only != "" && p.name != only {
				continue
			}
			p := p
			fork := tc
			fork.args = append([]string{"-m", p.model}, tc.args...)
			fork.needsEnv = append([]string{p.keyEnv}, tc.needsEnv...)
			t.Run(tc.name+"/"+p.name, fork.run)
		}
	}
}

// runPollyLocal invokes polly with the given args under the supplied HOME,
// returning combined stdout+stderr. Used by extraCheck hooks that need to
// observe context-store state via local-only commands like --show / --list.
// No API key is forwarded since these commands don't make LLM calls.
func runPollyLocal(t *testing.T, home string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pollyBin, args...)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("polly %s: %v\noutput:\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// preCreateContext invokes `polly --create <name>` in the given HOME so that
// subsequent subtests have a context to operate on. The subprocess inherits
// only HOME + PATH; no API key is needed because --create is a local op.
func preCreateContext(t *testing.T, home, name, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pollyBin,
		"--create", name,
		"--model", model,
	)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pre-create context %q: %v\noutput:\n%s", name, err, out.String())
	}
}
