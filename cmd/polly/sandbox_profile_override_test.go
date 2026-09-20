package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
)

func TestSandboxProfileSessionOverride(t *testing.T) {
	_, state := sandboxTryState(t)
	var output bytes.Buffer
	ctx := newWriterReplCommandContext(&Config{}, state, &output)
	command := func(line string) string {
		t.Helper()
		output.Reset()
		if _, _, err := defaultReplCommands.dispatch(line, ctx); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	allow := func(item string, save bool) {
		t.Helper()
		try, _ := scriptedTry(t, state)
		p, err := try.suggested(item, "use this setting")
		if err != nil || p.refused != "" {
			t.Fatalf("row = %+v, %v", p, err)
		}
		p.ticked = true
		try.rows = append(try.rows, p)
		if _, err := try.allow(save); err != nil {
			t.Fatal(err)
		}
	}
	cache := func(want string) {
		t.Helper()
		cfg, _, err := state.toolRegistry.SandboxReadPolicy()
		if err != nil || cfg.Env["GOCACHE"] != want {
			t.Fatalf("GOCACHE = %q, %v; want %q", cfg.Env["GOCACHE"], err, want)
		}
		bash, _ := state.toolRegistry.Get("bash")
		if got := tools.SandboxDetails(bash).Config.Env["GOCACHE"]; got != want {
			t.Fatalf("loaded bash GOCACHE = %q, want %q", got, want)
		}
	}
	profile := state.sandboxProfile
	allow("env GOCACHE=@cache/old", true)
	allow("env GOCACHE=@workspace/new", false)
	cache(filepath.Join(profile.ws.dir, "new"))
	if len(profile.session) != 1 || profile.judged[0].problem != "overridden for this session" || profile.stateOf(profile.session[0]).problem != "" {
		t.Fatalf("override state = %+v, %+v", profile.session, profile.judged)
	}
	if saved, err := readSandboxProfile(profile.ws.profile); err != nil || len(saved.Items) != 1 || saved.Items[0].Value != "@cache/old" {
		t.Fatalf("session override changed the file: %+v, %v", saved, err)
	}
	if shown := command("/sandbox show"); !strings.Contains(shown, "2. env GOCACHE=@workspace/new · this session only") {
		t.Fatalf("show omitted the override: %s", shown)
	}
	if reply := command("/sandbox forget 2"); !strings.Contains(reply, "forgot env GOCACHE=@workspace/new") {
		t.Fatalf("forget = %s", reply)
	}
	cache(filepath.Join(profile.ws.cache, "old"))
	if len(profile.profile.Items) != 1 || len(profile.session) != 0 {
		t.Fatalf("forget removed the saved setting: %+v, %+v", profile.profile.Items, profile.session)
	}
	// Saving another value replaces the temporary override too.
	allow("env GOCACHE=@workspace/new", false)
	allow("env GOCACHE=@cache/saved", true)
	cache(filepath.Join(profile.ws.cache, "saved"))
	if len(profile.session) != 0 {
		t.Fatalf("save left a competing session override: %+v", profile.session)
	}
	allow("env GOCACHE=@workspace/new", false)
	command("/sandbox allow env GOCACHE=@workspace/typed")
	cache(filepath.Join(profile.ws.dir, "typed"))
	if len(profile.session) != 0 {
		t.Fatalf("typed allow left a competing session override: %+v", profile.session)
	}
	// Identical values do not create a redundant session entry.
	allow("env GOCACHE=@workspace/typed", false)
	if len(profile.session) != 0 {
		t.Fatalf("identical setting created an override: %+v", profile.session)
	}
}

func TestSandboxProfileSessionOverrideCanNarrowMemberCredentials(t *testing.T) {
	_, state := sandboxTryState(t)
	profile := state.sandboxProfile
	item := sandboxProfileItem{Kind: profilePassEnv, Name: "NPM_TOKEN", Members: true, Credential: true, Origin: profile.ws.origin}
	if _, err := profile.change(state.toolRegistry, func(items []sandboxProfileItem) []sandboxProfileItem { return append(items, item) }, item); err != nil {
		t.Fatal(err)
	}
	try, _ := scriptedTry(t, state)
	p, err := try.suggested("passenv NPM_TOKEN", "keep the credential out of members")
	if err != nil || p.refused != "" {
		t.Fatalf("row = %+v, %v", p, err)
	}
	p.ticked = true
	try.rows = append(try.rows, p)
	if _, err := try.allow(false); err != nil {
		t.Fatal(err)
	}
	member, err := state.toolRegistry.ExecutionPolicy(t.TempDir(), tools.ExecutionGrant{})
	if err != nil || slices.Contains(member.Sandbox.PassEnv, "NPM_TOKEN") {
		t.Fatalf("session override kept the member credential: %+v, %v", member.Sandbox.PassEnv, err)
	}
	if cfg, _, err := state.toolRegistry.SandboxReadPolicy(); err != nil || !slices.Contains(cfg.PassEnv, "NPM_TOKEN") {
		t.Fatalf("session override lost the parent's credential: %v, %v", cfg.PassEnv, err)
	}
}
