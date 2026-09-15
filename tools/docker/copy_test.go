package docker

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/helper"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// scriptedHelper answers the protocol without running any tool: it records
// every sync request and serves scripted collect replies.
type scriptedHelper struct {
	mu           sync.Mutex
	syncs        []protocol.Sync
	collects     []protocol.Synced
	executes     []string
	collectDelay time.Duration
}

func (h *scriptedHelper) serve(stdin io.Reader, stdout io.Writer) {
	conn := protocol.NewConn(stdin, stdout)
	for {
		frame, err := conn.Read()
		if err != nil {
			return
		}
		switch frame.Type {
		case protocol.TypeHello:
			conn.Send(frame.ID, protocol.TypeWelcome, protocol.Welcome{Protocol: protocol.Version, UID: os.Getuid(), GID: os.Getgid()})
		case protocol.TypeLoad:
			tool := func(name, typ string, output bool) protocol.ToolInfo {
				return protocol.ToolInfo{Name: name, Type: typ, Source: "builtin", Output: output, Schema: []byte(`{"title":"` + name + `","type":"object","properties":{}}`)}
			}
			conn.Send(frame.ID, protocol.TypeLoaded, protocol.Loaded{Listed: protocol.Listed{Tools: []protocol.ToolInfo{tool("bash", "native", true), tool("write_file", "native", false), tool("edit_file", "native", false), tool("read_file", "native", false), tool("script__go", "shell", false)}}})
		case protocol.TypeExecute:
			req, _ := protocol.Decode[protocol.Execute](frame)
			h.mu.Lock()
			h.executes = append(h.executes, req.Tool)
			h.mu.Unlock()
			go func() {
				if millis, ok := req.Args["sleepMillis"].(float64); ok {
					time.Sleep(time.Duration(millis) * time.Millisecond)
				}
				result := protocol.Result{Invoked: true, Text: "ok"}
				switch req.Tool {
				case "write_file":
					result.Wrote = true
				case "edit_file":
					result.Error = &protocol.ToolError{Kind: protocol.ErrorKindPlain, Message: "no match"}
				}
				conn.Send(frame.ID, protocol.TypeResult, result)
			}()
		case protocol.TypeSync:
			req, _ := protocol.Decode[protocol.Sync](frame)
			h.mu.Lock()
			h.syncs = append(h.syncs, req)
			reply := protocol.Synced{}
			if req.Op == protocol.SyncCollect && len(h.collects) > 0 {
				reply, h.collects = h.collects[0], h.collects[1:]
			}
			delay := h.collectDelay
			h.mu.Unlock()
			if req.Op == protocol.SyncCollect && delay > 0 {
				time.Sleep(delay)
			}
			conn.Send(frame.ID, protocol.TypeSynced, reply)
		case protocol.TypeHeartbeat:
			conn.Send(frame.ID, protocol.TypePong, nil)
		case protocol.TypeCancel:
			conn.Send(frame.ID, protocol.TypeOK, nil)
		}
	}
}

func (h *scriptedHelper) syncOps() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ops []string
	for _, s := range h.syncs {
		ops = append(ops, s.Op)
	}
	return ops
}

func (h *scriptedHelper) syncAt(i int) protocol.Sync {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.syncs[i]
}

// fakeGit is a scripted GitAccess recording imports.
type fakeGit struct {
	mu        sync.Mutex
	commit    string
	tree      string
	changed   []string
	deleted   []string
	imports   []importRecord
	bundles   int
	importErr error
}

type importRecord struct {
	entries []tarEntry
	deleted []string
}

func (g *fakeGit) Base(context.Context, string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commit, g.tree, nil
}

func (g *fakeGit) WriteBundle(_ context.Context, _, commit string, w io.Writer) error {
	g.mu.Lock()
	g.bundles++
	g.mu.Unlock()
	_, err := io.WriteString(w, "BUNDLE:"+commit)
	return err
}

func (g *fakeGit) Divergence(context.Context, string, string) ([]string, []string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.changed, g.deleted, nil
}

func (g *fakeGit) ImportChanges(_ context.Context, _ string, changes io.Reader, deleted []string) error {
	record := importRecord{deleted: deleted}
	reader := tar.NewReader(changes)
	for {
		header, err := reader.Next()
		if err != nil {
			break
		}
		content, _ := io.ReadAll(reader)
		record.entries = append(record.entries, tarEntry{name: header.Name, typeflag: header.Typeflag, content: string(content)})
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.importErr != nil {
		err := g.importErr
		g.importErr = nil
		return err
	}
	g.imports = append(g.imports, record)
	return nil
}

func copyFixture(t *testing.T) (*fakeEngine, *scriptedHelper, *fakeGit, string, string) {
	t.Helper()
	scripted := &scriptedHelper{}
	f := newFakeEngine(t, helper.Options{})
	f.helperFunc = scripted.serve
	slot := t.TempDir()
	root, scratch := filepath.Join(slot, "tree"), filepath.Join(slot, "scratch")
	for _, dir := range []string{root, scratch} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("host a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := &fakeGit{commit: "c1", tree: "t1", changed: []string{"a.txt"}, deleted: []string{"gone.txt"}}
	return f, scripted, git, root, scratch
}

func copyProvider(t *testing.T, f *fakeEngine, git *fakeGit, configure func(*Options)) *Provider {
	t.Helper()
	opts := Options{Image: "test:image", Host: f.host(), HomeDir: t.TempDir(), Mode: ModeCopy, Policy: sandbox.DefaultConfig(), Git: func(context.Context) (GitAccess, error) { return git, nil }}
	if configure != nil {
		configure(&opts)
	}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runTool(t *testing.T, binding tools.ToolBinding, name string, args map[string]any) error {
	t.Helper()
	tool, ok := binding.Registry.Get(name)
	if !ok {
		t.Fatalf("%s is not served", name)
	}
	_, err := binding.Registry.ExecuteTool(context.Background(), tool, args, time.Minute)
	return err
}

func TestCopyOpenBootstrapsFromABundleAndPushesDivergence(t *testing.T) {
	f, scripted, git, root, scratch := copyFixture(t)
	p := copyProvider(t, f, git, nil)
	binding := openScope(t, p, OpenOptions{}, tools.ToolScope{Root: root, Session: "copy-1", Grant: tools.ExecutionGrant{Scratch: scratch}})
	defer binding.Close()
	creates, execs, _, _, _ := f.snapshot()
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	canonicalScratch, _ := filepath.EvalSymlinks(scratch)
	parent := filepath.Dir(canonicalRoot)
	host := creates[0].HostConfig
	if len(host.Mounts) != 1 || host.Mounts[0].Type != "volume" || host.Mounts[0].Target != parent || host.Mounts[0].Source != "" {
		t.Fatalf("copy mode mounts %+v", host.Mounts)
	}
	if creates[0].WorkingDir != "/" || creates[0].Labels[labelMode] != "copy" || creates[0].Labels[labelScratch] != canonicalScratch {
		t.Fatalf("create %+v", creates[0])
	}
	if execs[0].WorkingDir != canonicalRoot {
		t.Fatalf("exec %+v", execs[0])
	}
	puts := f.archivePuts()
	if len(puts) != 2 || puts[0].path != parent || puts[1].path != canonicalRoot {
		t.Fatalf("archive puts %+v", puts)
	}
	var names []string
	for _, entry := range puts[0].entries {
		names = append(names, entry.name)
		if entry.uid != os.Getuid() {
			t.Fatalf("layout entry %s owned by %d", entry.name, entry.uid)
		}
		if entry.name == "tree/"+bundleName && entry.content != "BUNDLE:c1" {
			t.Fatalf("bundle content %q", entry.content)
		}
	}
	if !slices.Equal(names, []string{"tree/", "scratch/", stagingDirName + "/", "tree/" + bundleName}) {
		t.Fatalf("layout entries %v", names)
	}
	if len(puts[1].entries) != 1 || puts[1].entries[0].name != "a.txt" || puts[1].entries[0].content != "host a\n" {
		t.Fatalf("divergence put %+v", puts[1].entries)
	}
	if ops := scripted.syncOps(); !slices.Equal(ops, []string{protocol.SyncBootstrap, protocol.SyncApply}) {
		t.Fatalf("sync ops %v", ops)
	}
	bootstrap, apply := scripted.syncAt(0), scripted.syncAt(1)
	if bootstrap.Root != canonicalRoot || bootstrap.Commit != "c1" || bootstrap.Bundle != canonicalRoot+"/"+bundleName {
		t.Fatalf("bootstrap %+v", bootstrap)
	}
	if !slices.Equal(apply.Deleted, []string{"gone.txt"}) || !slices.Equal(apply.Changed, []string{"a.txt"}) {
		t.Fatalf("apply %+v", apply)
	}
	if !strings.Contains(f.received(), `"mode":"copy"`) || !strings.Contains(f.received(), `"scratch":"`+canonicalScratch+`"`) {
		t.Fatal("hello did not name copy mode and the scratch")
	}

	// Writes sync back; reads do not; a failed edit does not.
	for _, call := range []struct {
		name string
		args map[string]any
		sync bool
	}{
		{"read_file", map[string]any{"path": "a.txt"}, false},
		{"bash", map[string]any{"command": "true"}, true},
		{"write_file", map[string]any{"path": "b.txt", "content": "x"}, true},
		{"edit_file", map[string]any{"path": "b.txt"}, false},
		{"script__go", map[string]any{}, true},
	} {
		before := len(scripted.syncOps())
		err := runTool(t, binding, call.name, call.args)
		if call.name == "edit_file" && err == nil {
			t.Fatal("scripted edit failure lost")
		}
		if call.name != "edit_file" && err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		ops := scripted.syncOps()
		synced := len(ops) > before && ops[len(ops)-1] == protocol.SyncCollect
		if synced != call.sync {
			t.Fatalf("%s synced=%v, want %v (ops %v)", call.name, synced, call.sync, ops)
		}
	}
}

func TestCopySyncFetchesStagingAndImports(t *testing.T) {
	f, scripted, git, root, _ := copyFixture(t)
	scripted.collects = []protocol.Synced{{Seq: 1, Staging: "/tmp/polly-sync/1", Changed: []string{"made.txt", "dir/leaf.txt"}, Deleted: []string{"old.txt"}}}
	f.serveArchive("/tmp/polly-sync/1", tarEntry{name: "1/", typeflag: tar.TypeDir}, tarEntry{name: "1/made.txt", content: "made\n"}, tarEntry{name: "1/dir/", typeflag: tar.TypeDir}, tarEntry{name: "1/dir/leaf.txt", content: "leaf\n"})
	p := copyProvider(t, f, git, nil)
	binding := openScope(t, p, OpenOptions{}, tools.ToolScope{Root: root})
	defer binding.Close()
	if err := runTool(t, binding, "bash", map[string]any{"command": "true"}); err != nil {
		t.Fatal(err)
	}
	git.mu.Lock()
	imports := append([]importRecord(nil), git.imports...)
	git.mu.Unlock()
	if len(imports) != 1 || !slices.Equal(imports[0].deleted, []string{"old.txt"}) {
		t.Fatalf("imports %+v", imports)
	}
	var names []string
	for _, entry := range imports[0].entries {
		names = append(names, entry.name)
	}
	if !slices.Equal(names, []string{"dir/", "dir/leaf.txt", "made.txt"}) && !slices.Equal(names, []string{"made.txt", "dir/", "dir/leaf.txt"}) {
		t.Fatalf("imported names %v (prefix not stripped?)", names)
	}
	ops := scripted.syncOps()
	if ops[len(ops)-1] != protocol.SyncCollected || scripted.syncAt(len(ops)-1).Seq != 1 {
		t.Fatalf("staging not released: %v", ops)
	}
}

func TestCopySyncFailureMarksTheBindingDirty(t *testing.T) {
	f, scripted, git, root, _ := copyFixture(t)
	scripted.collects = []protocol.Synced{{Seq: 1, Staging: "/tmp/polly-sync/1", Changed: []string{"made.txt"}}}
	f.serveArchive("/tmp/polly-sync/1", tarEntry{name: "1/made.txt", content: "made\n"})
	git.importErr = errors.New("disk full")
	p := copyProvider(t, f, git, nil)
	binding := openScope(t, p, OpenOptions{}, tools.ToolScope{Root: root})
	defer binding.Close()
	err := runTool(t, binding, "bash", map[string]any{"command": "true"})
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != "sync_failed" || !strings.Contains(toolErr.Message, "container still holds the edit") {
		t.Fatalf("sync failure = %v", err)
	}
	collects := len(scripted.syncOps())
	// The next call, a read, synchronises first.
	if err := runTool(t, binding, "read_file", map[string]any{"path": "a.txt"}); err != nil {
		t.Fatalf("read after a failed sync: %v", err)
	}
	if ops := scripted.syncOps(); len(ops) != collects+1 || ops[len(ops)-1] != protocol.SyncCollect {
		t.Fatalf("dirty binding did not sync before the next call: %v", ops)
	}
	// Clean again: a read no longer syncs.
	before := len(scripted.syncOps())
	if err := runTool(t, binding, "read_file", map[string]any{"path": "a.txt"}); err != nil || len(scripted.syncOps()) != before {
		t.Fatalf("clean binding synced on a read: %v %v", err, scripted.syncOps())
	}
}

func TestCopyBarrierCoalescesParallelCallsAndSyncerOrdering(t *testing.T) {
	f, scripted, git, root, _ := copyFixture(t)
	p := copyProvider(t, f, git, nil)
	binding := openScope(t, p, OpenOptions{}, tools.ToolScope{Root: root})
	defer binding.Close()
	scripted.mu.Lock()
	scripted.collectDelay = 40 * time.Millisecond
	scripted.mu.Unlock()
	before := len(scripted.syncOps())
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runTool(t, binding, "bash", map[string]any{"command": "true", "sleepMillis": float64(30)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	collects := len(scripted.syncOps()) - before
	if collects < 1 || collects > 3 {
		t.Fatalf("six parallel calls cost %d syncs", collects)
	}

	var runs int
	s := newSyncer(func(context.Context) error { runs++; time.Sleep(20 * time.Millisecond); return nil })
	var group sync.WaitGroup
	for i := 0; i < 5; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := s.after(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if runs < 1 || runs > 2 {
		t.Fatalf("five concurrent waiters ran %d syncs", runs)
	}
	failing := newSyncer(func(context.Context) error { return errors.New("boom") })
	if err := failing.after(context.Background()); err == nil {
		t.Fatal("sync failure lost")
	}
}

func TestResyncRefusesInFlightAndShipsABundleOnlyWhenTheBaseChanged(t *testing.T) {
	f, scripted, git, root, _ := copyFixture(t)
	p := copyProvider(t, f, git, nil)
	binding := openScope(t, p, OpenOptions{}, tools.ToolScope{Root: root})
	defer binding.Close()
	done := make(chan error, 1)
	go func() {
		done <- runTool(t, binding, "bash", map[string]any{"command": "true", "sleepMillis": float64(300)})
	}()
	time.Sleep(50 * time.Millisecond)
	if err := p.Resync(context.Background(), root); !errors.Is(err, ErrBusy) {
		t.Fatalf("resync under an in-flight call = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	bundles := git.bundles
	if err := p.Resync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	ops := scripted.syncOps()
	reset := scripted.syncAt(len(ops) - 2)
	if reset.Op != protocol.SyncReset || reset.Commit != "c1" || reset.Bundle != "" || ops[len(ops)-1] != protocol.SyncApply || git.bundles != bundles {
		t.Fatalf("same-base resync: %+v ops %v bundles %d", reset, ops, git.bundles)
	}
	git.mu.Lock()
	git.commit, git.tree = "c2", "t2"
	git.mu.Unlock()
	puts := len(f.archivePuts())
	if err := p.Resync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	ops = scripted.syncOps()
	reset = scripted.syncAt(len(ops) - 2)
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	if reset.Commit != "c2" || reset.Bundle != canonicalRoot+"/"+bundleName || git.bundles != bundles+1 {
		t.Fatalf("changed-base resync: %+v bundles %d", reset, git.bundles)
	}
	if latest := f.archivePuts(); len(latest) < puts+2 || latest[puts].entries[0].name != bundleName || latest[puts].entries[0].content != "BUNDLE:c2" {
		t.Fatalf("bundle not shipped: %+v", latest[puts:])
	}
	if err := p.Resync(context.Background(), t.TempDir()); err == nil {
		t.Fatal("resync of an unbound root succeeded")
	}
}

func TestCopyReconnectResyncsInsteadOfBootstrapping(t *testing.T) {
	f, scripted, git, root, _ := copyFixture(t)
	p := copyProvider(t, f, git, nil)
	scope := tools.ToolScope{Root: root, Session: "copy-keep"}
	first := openScope(t, p, OpenOptions{KeepOnClose: true}, scope)
	first.Close()
	puts := len(f.archivePuts())
	second := openScope(t, p, OpenOptions{KeepOnClose: true}, scope)
	defer second.Close()
	ops := scripted.syncOps()
	if !slices.Equal(ops, []string{protocol.SyncBootstrap, protocol.SyncApply, protocol.SyncReset, protocol.SyncApply}) {
		t.Fatalf("reconnect sync ops %v", ops)
	}
	if reset := scripted.syncAt(2); reset.Bundle == "" {
		t.Fatalf("reconnect did not ship the base: %+v", reset)
	}
	latest := f.archivePuts()
	for _, put := range latest[puts:] {
		for _, entry := range put.entries {
			if strings.HasSuffix(entry.name, "/") && entry.name == "tree/" {
				t.Fatal("reconnect put the layout again")
			}
		}
	}
	creates, _, _, _, _ := f.snapshot()
	if len(creates) != 1 {
		t.Fatalf("reconnect created %d containers", len(creates))
	}
}

func TestCopyModeReadOnlyScopeNeverSyncs(t *testing.T) {
	f, scripted, git, root, scratch := copyFixture(t)
	git.changed, git.deleted = nil, nil
	p := copyProvider(t, f, git, nil)
	binding := openScope(t, p, OpenOptions{}, tools.ToolScope{Root: root, Grant: tools.ExecutionGrant{ReadOnly: true, Scratch: scratch}})
	defer binding.Close()
	before := len(scripted.syncOps())
	if err := runTool(t, binding, "bash", map[string]any{"command": "true"}); err != nil {
		t.Fatal(err)
	}
	if len(scripted.syncOps()) != before {
		t.Fatalf("read-only scope synced: %v", scripted.syncOps())
	}
	if _, err := New(Options{Image: "test:image", Host: f.host(), Mode: ModeCopy}); err == nil {
		t.Fatal("copy mode without Git accepted")
	}
}
