package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm/replay"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func writeShotFixture(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadShotFixturePointsTheLaunchAtTheReplayModel(t *testing.T) {
	path := writeShotFixture(t, "demo.json", `{"sessions": [{"name": "seeded", "launch": true}, {"name": "other"}], "turns": [{"steps": [{"content": "x", "mark": "m"}, {"gate": "g"}]}]}`)
	defer replay.Uninstall("demo")
	config := &Config{ShotScript: "-", ShotFixture: path}
	config.Launch.Model, config.Launch.ModelHost = "openrouter/x", "host"
	launch, err := loadShotFixture(config)
	if err != nil {
		t.Fatal(err)
	}
	if launch != "seeded" {
		t.Fatalf("launch = %q, want the session marked launch", launch)
	}
	if config.Launch.Model != "replay/demo" || config.Launch.ModelHost != "" {
		t.Fatalf("launch model = %q host %q", config.Launch.Model, config.Launch.ModelHost)
	}
	if config.shotFixture == nil || config.shotBus == nil {
		t.Fatal("the loaded fixture and its bus were not kept")
	}

	// The script's signals are checked against the fixture before play.
	script := filepath.Join(t.TempDir(), "scenario.txt")
	if err := os.WriteFile(script, []byte(":release g\n:at m 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := loadHeadlessRun(script, 80, 24, config.shotFixture, config.shotBus)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.steps) != 2 || run.steps[0].kind != "release" || run.steps[1].kind != "at" || run.steps[1].arg != "m" {
		t.Fatalf("steps = %+v", run.steps)
	}
	for _, tc := range []struct{ line, want string }{
		{":release nope", `no gate named "nope"`},
		{":at nope", `no mark named "nope"`},
	} {
		if err := os.WriteFile(script, []byte(tc.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHeadlessRun(script, 80, 24, config.shotFixture, config.shotBus); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.line, err, tc.want)
		}
	}
	if _, err := loadHeadlessRun(script, 80, 24, nil, nil); err == nil || !strings.Contains(err.Error(), "needs a fixture") {
		t.Errorf("without a fixture: err = %v", err)
	}
}

func TestParseShotFixtureRejectsMistakes(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
	}{
		{"empty", `{}`, "neither sessions nor turns"},
		{"unknown field", `{"turns":[{"step":[]}]}`, "unknown field"},
		{"nameless session", `{"sessions":[{"history":[]}]}`, "name is required"},
		{"duplicate session", `{"sessions":[{"name":"a"},{"name":"a"}]}`, "duplicate name"},
		{"parent after child", `{"sessions":[{"name":"b","parent":"a"},{"name":"a"}]}`, "not an earlier session"},
		{"two launches", `{"sessions":[{"name":"a","launch":true},{"name":"b","launch":true}]}`, "more than one"},
		{"bad metadata", `{"sessions":[{"name":"a","metadata":{"nope":1}}]}`, "metadata"},
		{"bad turn", `{"turns":[{"steps":[{"content":"x","gate":"g"}]}]}`, "exactly one"},
	} {
		_, err := parseShotFixture([]byte(tc.data))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	f, err := parseShotFixture([]byte(`{"sessions": [{"name": "a"}, {"name": "b", "launch": true}]}`))
	if err != nil || f.launch().Name != "b" {
		t.Fatalf("launch = %v, err = %v", f.launch(), err)
	}
}

func TestLoadShotFixtureRefusesConflictingFlags(t *testing.T) {
	path := writeShotFixture(t, "demo.json", `{"sessions": [{"name": "seeded"}]}`)
	defer replay.Uninstall("demo")
	for _, tc := range []struct {
		name   string
		config Config
		want   string
	}{
		{"no script", Config{ShotFixture: path}, "--shot-script"},
		{"context", Config{ShotScript: "-", ShotFixture: path, ContextID: "x"}, "drop --context"},
		{"last", Config{ShotScript: "-", ShotFixture: path, UseLastContext: true}, "drop --context"},
		{"missing file", Config{ShotScript: "-", ShotFixture: filepath.Join(t.TempDir(), "none.json")}, "read shot fixture"},
	} {
		config := tc.config
		if _, err := loadShotFixture(&config); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestSeedShotFixtureWritesSessionsInOrder(t *testing.T) {
	fixture, err := parseShotFixture([]byte(`{"sessions": [
		{"name": "root", "metadata": {"title": "Root title", "thinkingEffort": "high"},
		 "history": [{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello", "metadata": {"thinking_ms": 5}}]},
		{"name": "child", "parent": "root", "metadata": {"model": "replay/other"}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{shotFixture: fixture}
	config.Launch.Model = "replay/demo"
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()

	// A session already stored under a fixture name is replaced.
	stale := testAcquireSession(t, store, "root")
	testAddMessage(t, stale, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "stale"})
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	if err := seedShotFixture(ctx, store, config); err != nil {
		t.Fatal(err)
	}
	root, err := store.GetMetadata(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if root.Title != "Root title" || root.TitleSource != sessions.TitleSourceUser || root.ThinkingEffort != "high" || root.Model != "replay/demo" {
		t.Fatalf("root metadata = %+v", root)
	}
	if len(root.ActiveTools) == 0 {
		t.Fatal("the launch defaults were not kept under the fixture's metadata")
	}
	history := testSessionHistory(t, testAcquireSession(t, store, "root"))
	var user, assistant int
	for _, msg := range history {
		switch msg.Role {
		case messages.MessageRoleUser:
			user++
			if msg.Content == "stale" {
				t.Fatal("the stale history survived")
			}
		case messages.MessageRoleAssistant:
			assistant++
			if msg.ThinkingDuration() == 0 {
				t.Fatalf("assistant metadata was not stored verbatim: %+v", msg)
			}
		}
	}
	if user != 1 || assistant != 1 {
		t.Fatalf("history = %+v", history)
	}

	child, err := store.GetMetadata(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != "root" || child.Model != "replay/other" {
		t.Fatalf("child metadata = %+v", child)
	}
}

func TestSeedShotFixtureReplacesTheModelOnlyWhenTurnsPlay(t *testing.T) {
	for _, tc := range []struct {
		name, turns, want string
	}{
		{"sessions only keep the stored model", "", "anthropic/stored"},
		{"turns are answered by the replay model", `, "turns": [{"steps": [{"content": "x"}]}]`, "replay/demo"},
	} {
		fixture, err := parseShotFixture([]byte(`{"sessions": [{"name": "s", "metadata": {"model": "anthropic/stored", "modelHost": "h"}}]` + tc.turns + `}`))
		if err != nil {
			t.Fatal(err)
		}
		config := &Config{shotFixture: fixture}
		config.Launch.Model = "replay/demo"
		store := testOpenMemoryStore(t, nil)
		if err := seedShotFixture(context.Background(), store, config); err != nil {
			t.Fatal(err)
		}
		md, err := store.GetMetadata(context.Background(), "s")
		if err != nil {
			t.Fatal(err)
		}
		if md.Model != tc.want {
			t.Errorf("%s: model = %q, want %q", tc.name, md.Model, tc.want)
		}
		if md.Model == "replay/demo" && md.ModelHost != "" {
			t.Errorf("%s: host survived the replay override", tc.name)
		}
	}
}

func TestExportShotFixtureRoundTripsSeededSessions(t *testing.T) {
	fixture, err := parseShotFixture([]byte(`{"sessions": [
		{"name": "root", "metadata": {"title": "Root title", "thinkingEffort": "high", "systemPrompt": "be brief"},
		 "history": [{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"}]},
		{"name": "scout", "parent": "root", "metadata": {"spawnCallID": "call_0"}, "history": [{"role": "user", "content": "look"}]},
		{"name": "deeper", "parent": "scout"},
		{"name": "unrelated"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{shotFixture: fixture}
	config.Launch.Model = "anthropic/real"
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	if err := seedShotFixture(ctx, store, config); err != nil {
		t.Fatal(err)
	}

	exported, err := exportShotFixture(ctx, store, "root", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Sessions) != 3 {
		t.Fatalf("exported %d sessions, want root and its two descendants: %+v", len(exported.Sessions), exported.Sessions)
	}
	root, scout, deeper := exported.Sessions[0], exported.Sessions[1], exported.Sessions[2]
	if root.Name != "root" || !root.Launch || root.Parent != "" {
		t.Fatalf("root = %+v", root)
	}
	if scout.Name != "scout" || scout.Parent != "root" || scout.Launch || deeper.Parent != "scout" {
		t.Fatalf("children = %+v / %+v", scout, deeper)
	}
	for _, msg := range root.History {
		if msg.Role == messages.MessageRoleSystem {
			t.Fatal("the stored system message was exported; the seeder writes its own")
		}
	}
	if len(root.History) != 2 || len(scout.History) != 1 {
		t.Fatalf("histories = %d / %d", len(root.History), len(scout.History))
	}
	var fields map[string]any
	if err := json.Unmarshal(root.Metadata, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "created", "lastUsed", "parent"} {
		if _, ok := fields[key]; ok {
			t.Errorf("exported metadata carries %q", key)
		}
	}
	if fields["model"] != "anthropic/real" {
		t.Fatalf("the stored model was not exported: %v", fields["model"])
	}
	if fields["title"] != "Root title" || fields["thinkingEffort"] != "high" || fields["systemPrompt"] != "be brief" {
		t.Fatalf("exported metadata = %v", fields)
	}
	if _, err := exportShotFixture(ctx, store, "nope", false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing context err = %v", err)
	}

	// The export seeds again as it is: a fixture written by polly parses.
	data, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	again, err := parseShotFixture(data)
	if err != nil {
		t.Fatal(err)
	}
	if again.launch().Name != "root" {
		t.Fatalf("re-parsed launch = %q", again.launch().Name)
	}
}

func TestExportShotFixtureCarriesArtifactsOnRequest(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "pictures")
	png := []byte("\x89PNG not really a picture")
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "shot.png", Data: png})
	if err != nil {
		t.Fatal(err)
	}
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "look", Parts: []messages.ContentPart{
		{Type: "image_artifact", MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken, Artifact: &ref},
		{Type: "image_artifact", Artifact: &ref}, // referenced twice, exported once
	}})
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	plain, err := exportShotFixture(ctx, store, "pictures", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Sessions[0].Artifacts) != 0 {
		t.Fatal("artifacts were exported without --artifacts")
	}
	exported, err := exportShotFixture(ctx, store, "pictures", true)
	if err != nil {
		t.Fatal(err)
	}
	got := exported.Sessions[0].Artifacts
	if len(got) != 1 || got[0].ID != ref.ID || got[0].Kind != artifacts.KindImage || got[0].Name != "shot.png" || string(got[0].Data) != string(png) {
		t.Fatalf("artifacts = %+v", got)
	}

	// The export seeds into another store and the part's reference resolves.
	data, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := parseShotFixture(data)
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{shotFixture: fixture}
	config.Launch.Model = "replay/demo"
	other := testOpenMemoryStore(t, nil)
	if err := seedShotFixture(ctx, other, config); err != nil {
		t.Fatal(err)
	}
	seeded := testAcquireSession(t, other, "pictures")
	history := testSessionHistory(t, seeded)
	var part *messages.ContentPart
	for i := range history {
		if len(history[i].Parts) > 0 {
			part = &history[i].Parts[0]
		}
	}
	if part == nil || part.Artifact == nil || part.Artifact.ID != ref.ID {
		t.Fatalf("seeded history lost its reference: %+v", history)
	}
	reader, err := seeded.ArtifactStore().Open(ctx, part.Artifact.ID)
	if err != nil {
		t.Fatalf("the seeded artifact does not resolve: %v", err)
	}
	defer reader.Close()
	back, err := io.ReadAll(reader)
	if err != nil || string(back) != string(png) {
		t.Fatalf("seeded bytes = %q, err = %v", back, err)
	}

	// A payload that does not hash to its claimed id is refused at parse.
	data = bytes.Replace(data, []byte(`"data":"`), []byte(`"data":"AAAA`), 1)
	if _, err := parseShotFixture(data); err == nil || !strings.Contains(err.Error(), "does not hash") {
		t.Fatalf("tampered data err = %v", err)
	}
}
