package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiffFileChangeKindsAndCounts(t *testing.T) {
	created := DiffFileChange("x.txt", nil, []byte("a\nb\n"), false, true)
	if created.Kind != ChangeCreated || created.Additions != 2 || created.Deletions != 0 || !strings.HasPrefix(created.Diff, "--- /dev/null\n+++ b/x.txt\n@@ -0,0 +1,2 @@\n") {
		t.Fatalf("created: %+v", created)
	}
	deleted := DiffFileChange("x.txt", []byte("a\n"), nil, true, false)
	if deleted.Kind != ChangeDeleted || deleted.Deletions != 1 || !strings.Contains(deleted.Diff, "+++ /dev/null\n") {
		t.Fatalf("deleted: %+v", deleted)
	}
	modified := DiffFileChange("dir/x.txt", []byte("a\nb\n"), []byte("a\nc\n"), true, true)
	if modified.Kind != ChangeModified || modified.Additions != 1 || modified.Deletions != 1 || !strings.Contains(modified.Diff, "--- a/dir/x.txt\n+++ b/dir/x.txt\n") {
		t.Fatalf("modified: %+v", modified)
	}
	same := DiffFileChange("x", []byte("a\n"), []byte("a\n"), true, true)
	if same.Diff != "" || same.Additions != 0 || same.Deletions != 0 || same.Truncated {
		t.Fatalf("identical: %+v", same)
	}
}

func TestDiffFileChangeBinaryAndOversized(t *testing.T) {
	binary := DiffFileChange("x.bin", []byte("a\x00b"), []byte("c"), true, true)
	if !binary.Binary || binary.Diff != "" || binary.Additions != 0 {
		t.Fatalf("binary: %+v", binary)
	}
	big := make([]byte, changeMaxFileBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	large := DiffFileChange("x", []byte("old\n"), big, true, true)
	if !large.Truncated || large.Diff != "" || !large.CountsUnknown {
		t.Fatalf("oversized: %+v", large)
	}
}

func TestDiffFileChangeCutsLongDiffAtHunkBoundary(t *testing.T) {
	var old, new strings.Builder
	for i := 0; i < 20000; i++ {
		line := strings.Repeat("x", 20)
		old.WriteString(line + "\n")
		if i%10 == 0 {
			new.WriteString(line + "!\n")
		} else {
			new.WriteString(line + "\n")
		}
	}
	change := DiffFileChange("x", []byte(old.String()), []byte(new.String()), true, true)
	if !change.Truncated || len(change.Diff) > changeMaxDiffBytes || !strings.HasSuffix(change.Diff, "\n") {
		t.Fatalf("truncated=%v len=%d", change.Truncated, len(change.Diff))
	}
	if change.Additions != 2000 || change.Deletions != 2000 {
		t.Fatalf("counts survive truncation: %+v", change.Additions)
	}
	// The cut lands on a hunk boundary: the last hunk's body matches the
	// line counts its header announces.
	last := change.Diff[strings.LastIndex(change.Diff, "\n@@ ")+1:]
	var aStart, aLen, bStart, bLen int
	if _, err := fmt.Sscanf(last, "@@ -%d,%d +%d,%d @@", &aStart, &aLen, &bStart, &bLen); err != nil {
		t.Fatalf("header: %v in %q", err, last[:40])
	}
	oldLines, newLines := 0, 0
	for _, line := range strings.Split(last, "\n")[1:] {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "-") {
			oldLines++
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "+") {
			newLines++
		}
	}
	if oldLines != aLen || newLines != bLen {
		t.Fatalf("last hunk incomplete: -%d/%d +%d/%d\n%s", oldLines, aLen, newLines, bLen, last)
	}
}

func TestDiffFileChangeBoundsSingleLargeHunk(t *testing.T) {
	content := strings.Repeat(strings.Repeat("x", 100)+"\n", 1000)
	change := DiffFileChange("new.txt", nil, []byte(content), false, true)
	if !change.Truncated || len(change.Diff) > changeMaxDiffBytes || !strings.HasSuffix(change.Diff, "\n") {
		t.Fatalf("single hunk exceeded diff budget: truncated=%v bytes=%d", change.Truncated, len(change.Diff))
	}
	if change.CountsUnknown || change.Additions != 1000 || change.Deletions != 0 {
		t.Fatalf("exact counts lost when cutting the body: %+v", change)
	}
}

func TestFileChangesJSONShape(t *testing.T) {
	data, err := json.Marshal(FileChanges{Root: "/r", Tracked: true, Changes: []FileChange{}})
	if err != nil || string(data) != `{"root":"/r","changes":[],"tracked":true}` {
		t.Fatalf("json: %s %v", data, err)
	}
	data, _ = json.Marshal(FileChanges{Reason: "not a git repository"})
	if string(data) != `{"root":"","changes":null,"tracked":false,"reason":"not a git repository"}` {
		t.Fatalf("untracked json: %s", data)
	}
}

func TestChangePath(t *testing.T) {
	if got := changePath("/w", "/w/a/b.go"); got != "a/b.go" {
		t.Fatalf("inside: %q", got)
	}
	if got := changePath("/w", "/other/b.go"); got != "/other/b.go" {
		t.Fatalf("outside: %q", got)
	}
	if got := changePath("", "/w/b.go"); got != "/w/b.go" {
		t.Fatalf("no root: %q", got)
	}
}

type stubChangeTracker struct {
	snapshots, changes int
	ok                 bool
	reason             string
	err                error
	result             FileChanges
	seenDir            string
	onChanges          func(ctx context.Context)
}

func (s *stubChangeTracker) Snapshot(ctx context.Context, dir string) (string, bool, string, error) {
	s.snapshots++
	s.seenDir = dir
	if s.err != nil {
		return "", false, "", s.err
	}
	return "before", s.ok, s.reason, nil
}

func (s *stubChangeTracker) Changes(ctx context.Context, dir, token string) (FileChanges, error) {
	s.changes++
	if s.onChanges != nil {
		s.onChanges(ctx)
	}
	if s.err != nil {
		return FileChanges{}, s.err
	}
	return s.result, nil
}

func TestChangeTrackerPropagatesToDerivedAndBoundRegistries(t *testing.T) {
	tracker := &stubChangeTracker{ok: true}
	r := NewToolRegistry(nil, WithUnsafeNoSandbox(), WithChangeTracker(tracker))
	defer r.Close()
	if r.ChangeTracker() != tracker {
		t.Fatal("option did not install the tracker")
	}
	derived := r.Derive()
	defer derived.Close()
	if derived.ChangeTracker() != tracker {
		t.Fatal("derived registry lost the tracker")
	}
	bound, _, err := r.BindExecutionContext(ExecutionContext{Root: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if bound.ChangeTracker() != tracker {
		t.Fatal("bound registry lost the tracker")
	}
	var nilRegistry *ToolRegistry
	if nilRegistry.ChangeTracker() != nil {
		t.Fatal("nil registry")
	}
}

func TestChangeTrackerSetAfterBashLoaded(t *testing.T) {
	skipIfWindows(t)
	r := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer r.Close()
	if _, err := r.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	tracker := &stubChangeTracker{ok: true, result: FileChanges{Tracked: true, Changes: []FileChange{}}}
	r.SetChangeTracker(tracker)
	bash, _, _ := r.GetIfAllowed("bash")
	out, err := bash.(OutputTool).ExecuteOutput(context.Background(), map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if tracker.snapshots != 1 || tracker.changes != 1 {
		t.Fatalf("tracker calls: %d %d", tracker.snapshots, tracker.changes)
	}
	if result, ok := out.Data.(CommandResult); !ok || result.Changes == nil || !result.Changes.Tracked {
		t.Fatalf("data: %#v", out.Data)
	}
}

func trackedBash(t *testing.T, tracker *stubChangeTracker) *BashTool {
	t.Helper()
	skipIfWindows(t)
	bash := NewUnsafeBashTool(t.TempDir())
	bash.tracker = func() ChangeTracker { return tracker }
	return bash
}

func TestBashReportsTrackedChanges(t *testing.T) {
	want := FileChanges{Root: "/w", Tracked: true, Changes: []FileChange{{Path: "a.txt", Kind: ChangeModified, Additions: 1}}}
	tracker := &stubChangeTracker{ok: true, result: want}
	bash := trackedBash(t, tracker)
	out, err := bash.ExecuteOutput(context.Background(), map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	result := out.Data.(CommandResult)
	if result.Changes == nil || result.Changes.Root != "/w" || len(result.Changes.Changes) != 1 || tracker.seenDir != bash.workDir {
		t.Fatalf("changes: %+v dir=%q", result.Changes, tracker.seenDir)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), `"changes":{"root":"/w"`) {
		t.Fatalf("json: %s", data)
	}
}

func TestBashChangesOnNonZeroExit(t *testing.T) {
	tracker := &stubChangeTracker{ok: true, result: FileChanges{Tracked: true, Changes: []FileChange{}}}
	bash := trackedBash(t, tracker)
	out, err := bash.ExecuteOutput(context.Background(), map[string]any{"command": "exit 3"})
	if err == nil {
		t.Fatal("expected exit error")
	}
	if result := out.Data.(CommandResult); result.ExitCode != 3 || result.Changes == nil || !result.Changes.Tracked {
		t.Fatalf("result: %+v", result)
	}
}

func TestBashChangesUntrackedWorkspace(t *testing.T) {
	tracker := &stubChangeTracker{ok: false, reason: "not a git repository"}
	bash := trackedBash(t, tracker)
	out, err := bash.ExecuteOutput(context.Background(), map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	result := out.Data.(CommandResult)
	if result.Changes == nil || result.Changes.Tracked || result.Changes.Reason != "not a git repository" || tracker.changes != 0 {
		t.Fatalf("result: %+v calls=%d", result.Changes, tracker.changes)
	}
}

func TestBashChangesTrackerErrorDoesNotFailCommand(t *testing.T) {
	tracker := &stubChangeTracker{err: context.DeadlineExceeded}
	bash := trackedBash(t, tracker)
	out, err := bash.ExecuteOutput(context.Background(), map[string]any{"command": "echo hi"})
	if err != nil || out.Text != "hi" {
		t.Fatalf("command failed: %q %v", out.Text, err)
	}
	if result := out.Data.(CommandResult); result.Changes == nil || result.Changes.Tracked || result.Changes.Reason == "" {
		t.Fatalf("result: %+v", result.Changes)
	}
}

func TestBashChangesObservedAfterCommandTimeout(t *testing.T) {
	var liveAtChanges bool
	tracker := &stubChangeTracker{ok: true, result: FileChanges{Tracked: true, Changes: []FileChange{}}}
	tracker.onChanges = func(ctx context.Context) { liveAtChanges = ctx.Err() == nil }
	bash := trackedBash(t, tracker)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out, err := bash.ExecuteOutput(ctx, map[string]any{"command": "sleep 5"})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if tracker.changes != 1 || !liveAtChanges || out.Data == nil {
		t.Fatalf("after-snapshot: calls=%d live=%v %+v", tracker.changes, liveAtChanges, out.Data)
	}
}

func TestBashWithoutTrackerOmitsChanges(t *testing.T) {
	skipIfWindows(t)
	bash := NewUnsafeBashTool(t.TempDir())
	out, err := bash.ExecuteOutput(context.Background(), map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(out.Data)
	if string(data) != `{"exitCode":0}` {
		t.Fatalf("json: %s", data)
	}
}

func TestEditFileOutputCarriesDiff(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "f.txt", "alpha\nbeta\ngamma\n")
	tool := NewEditFileTool(NewToolRegistry(nil)).(OutputTool)
	out, err := tool.ExecuteOutput(context.Background(), map[string]any{"path": path, "old_string": "beta", "new_string": "delta"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := NewEditFileTool(NewToolRegistry(nil)).Execute(context.Background(), map[string]any{"path": path, "old_string": "delta", "new_string": "beta"})
	if err != nil || !strings.HasPrefix(text, "Edited "+path+": 1 replacement(s).\n") || strings.Contains(text, "@@") {
		t.Fatalf("model text changed: %q %v", text, err)
	}
	changes, ok := out.Data.(FileChanges)
	if !ok || !changes.Tracked || len(changes.Changes) != 1 {
		t.Fatalf("data: %#v", out.Data)
	}
	change := changes.Changes[0]
	if change.Kind != ChangeModified || change.Additions != 1 || change.Deletions != 1 || !strings.Contains(change.Diff, "-beta\n+delta\n") {
		t.Fatalf("change: %+v", change)
	}
	// Outside the workspace root the path stays absolute; inside it is relative.
	if change.Path != filepath.ToSlash(path) {
		t.Fatalf("path outside root: %q", change.Path)
	}
	bound, _, err := NewToolRegistry(nil, WithUnsafeNoSandbox()).BindExecutionContext(ExecutionContext{Root: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	out, err = NewEditFileTool(bound).(OutputTool).ExecuteOutput(context.Background(), map[string]any{"path": "f.txt", "old_string": "alpha", "new_string": "omega", "replace_all": true})
	if err != nil {
		t.Fatal(err)
	}
	if changes := out.Data.(FileChanges); changes.Root != dir || changes.Changes[0].Path != "f.txt" {
		t.Fatalf("bound registry: %+v", changes)
	}
}

func TestEditFileReplaceAllDiffCountsEveryOccurrence(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "x=1\nx=2\nx=3\n")
	out, err := NewEditFileTool(NewToolRegistry(nil)).(OutputTool).ExecuteOutput(context.Background(), map[string]any{"path": path, "old_string": "x=", "new_string": "y=", "replace_all": true})
	if err != nil {
		t.Fatal(err)
	}
	if change := out.Data.(FileChanges).Changes[0]; change.Additions != 3 || change.Deletions != 3 {
		t.Fatalf("replace_all counts: %+v", change)
	}
}

func TestWriteFileOutputCarriesDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	tool := NewWriteFileTool(NewToolRegistry(nil)).(OutputTool)
	out, err := tool.ExecuteOutput(context.Background(), map[string]any{"path": path, "content": "one\ntwo\n"})
	if err != nil || !strings.HasPrefix(out.Text, "Created ") {
		t.Fatalf("create: %q %v", out.Text, err)
	}
	created := out.Data.(FileChanges).Changes[0]
	if created.Kind != ChangeCreated || created.Additions != 2 || created.Deletions != 0 || !strings.Contains(created.Diff, "@@ -0,0 +1,2 @@\n+one\n+two\n") {
		t.Fatalf("created: %+v", created)
	}
	out, err = tool.ExecuteOutput(context.Background(), map[string]any{"path": path, "content": "one\nthree\n"})
	if err != nil || !strings.HasPrefix(out.Text, "Overwrote ") || strings.Contains(out.Text, "@@") {
		t.Fatalf("overwrite text: %q %v", out.Text, err)
	}
	modified := out.Data.(FileChanges).Changes[0]
	if modified.Kind != ChangeModified || modified.Additions != 1 || modified.Deletions != 1 || !strings.Contains(modified.Diff, "-two\n+three\n") {
		t.Fatalf("modified: %+v", modified)
	}
	if data, _ := os.ReadFile(path); string(data) != "one\nthree\n" {
		t.Fatalf("content: %q", data)
	}
	// Shorter content must not leave the old tail behind.
	out, err = tool.ExecuteOutput(context.Background(), map[string]any{"path": path, "content": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "x" {
		t.Fatalf("truncate on rewrite: %q", data)
	}
	if same, _ := tool.ExecuteOutput(context.Background(), map[string]any{"path": path, "content": "x"}); same.Data.(FileChanges).Changes[0].Diff != "" {
		t.Fatalf("identical rewrite should have no diff: %+v", same.Data)
	}
}

func TestWriteFileBinaryAndLargeOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	tool := NewWriteFileTool(NewToolRegistry(nil)).(OutputTool)
	out, err := tool.ExecuteOutput(context.Background(), map[string]any{"path": path, "content": "a\x00b"})
	if err != nil || !out.Data.(FileChanges).Changes[0].Binary {
		t.Fatalf("binary: %+v %v", out.Data, err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(changeMaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err = tool.ExecuteOutput(context.Background(), map[string]any{"path": path, "content": "small\n"})
	if err != nil {
		t.Fatal(err)
	}
	if change := out.Data.(FileChanges).Changes[0]; !change.Truncated || change.Diff != "" || change.Kind != ChangeModified || !change.CountsUnknown {
		t.Fatalf("large old: %+v", change)
	}
	if data, _ := os.ReadFile(path); string(data) != "small\n" {
		t.Fatalf("content: %q", data)
	}
}
