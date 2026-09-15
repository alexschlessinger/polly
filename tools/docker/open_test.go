package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/helper"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func testProvider(t *testing.T, f *fakeEngine, configure func(*Options)) *Provider {
	t.Helper()
	opts := Options{Image: "test:image", Host: f.host(), HomeDir: t.TempDir(), Policy: sandbox.DefaultConfig()}
	if configure != nil {
		configure(&opts)
	}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func nativeLoad(names ...string) OpenOptions {
	var o OpenOptions
	for _, name := range names {
		o.Tools = append(o.Tools, tools.ToolLoaderInfo{Name: name, Type: "native", Source: "builtin"})
	}
	return o
}

func openScope(t *testing.T, p *Provider, o OpenOptions, scope tools.ToolScope) tools.ToolBinding {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binding, err := p.OpenTools(o)(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestOpenCreatesAContainerAndServesItsTools(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root := t.TempDir()
	p := testProvider(t, f, func(o *Options) {
		o.Memory = "512m"
		o.CPUs = "1.5"
		o.Labels = map[string]string{"team": "qa", "polly.root": "spoofed"}
	})
	binding := openScope(t, p, nativeLoad("bash", "read_file"), tools.ToolScope{Root: root, Session: "session-1"})
	creates, execs, starts, removed, containers := f.snapshot()
	if len(creates) != 1 || len(starts) != 1 || len(execs) != 1 || len(removed) != 0 || len(containers) != 1 {
		t.Fatalf("creates %d starts %d execs %d removed %d containers %d", len(creates), len(starts), len(execs), len(removed), len(containers))
	}
	create := creates[0]
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	host := create.HostConfig
	if create.Image != "sha256:image-one" || !slices.Equal(create.Cmd, []string{"sleep", "infinity"}) || !slices.Equal(create.Env, []string{"HOME=/run/polly/home"}) || create.WorkingDir != canonicalRoot {
		t.Fatalf("create body %+v", create)
	}
	if !host.ReadonlyRootfs || !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Equal(host.SecurityOpt, []string{"no-new-privileges"}) || host.NetworkMode != "none" || !host.Init || host.PidsLimit != DefaultPIDs || host.Memory != 512<<20 || host.NanoCPUs != 1500000000 {
		t.Fatalf("host config %+v", host)
	}
	if host.Tmpfs["/tmp"] == "" || host.Tmpfs["/run/polly"] == "" {
		t.Fatalf("tmpfs %+v", host.Tmpfs)
	}
	if len(host.Mounts) != 1 || host.Mounts[0].Source != canonicalRoot || host.Mounts[0].Target != canonicalRoot || host.Mounts[0].ReadOnly {
		t.Fatalf("mounts %+v", host.Mounts)
	}
	if create.Labels["team"] != "qa" || create.Labels[labelRoot] != canonicalRoot || create.Labels[labelSession] != "session-1" || create.Labels[labelImage] != "sha256:image-one" || create.Labels[labelProtocol] != "1" || create.Labels[labelNetwork] != "none" {
		t.Fatalf("labels %+v", create.Labels)
	}
	if !slices.Equal(execs[0].Cmd, []string{"polly", "sandbox", "helper"}) || execs[0].WorkingDir != canonicalRoot || !execs[0].AttachStdin || execs[0].Tty {
		t.Fatalf("exec %+v", execs[0])
	}
	var names []string
	for _, tool := range binding.Registry.All() {
		names = append(names, tool.GetName())
	}
	if !slices.Equal(names, []string{"bash", "read_file", "view_image"}) {
		t.Fatalf("served %v", names)
	}
	bashTool, _ := binding.Registry.Get("bash")
	execution, err := binding.Registry.ExecuteTool(context.Background(), bashTool, map[string]any{"command": "echo through-the-container; pwd"}, time.Minute)
	if err != nil || !strings.Contains(execution.Output.Text, "through-the-container") || !strings.Contains(execution.Output.Text, canonicalRoot) {
		t.Fatalf("bash through the binding = %+v, %v", execution.Output, err)
	}
	if err := binding.Close(); err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	_, _, _, removed, containers = f.snapshot()
	if len(removed) != 1 || len(containers) != 0 {
		t.Fatalf("standalone close did not destroy: removed %v containers %d", removed, len(containers))
	}
}

func TestOpenReconnectsAMatchingContainerAndStartsAStoppedOne(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root := t.TempDir()
	p := testProvider(t, f, nil)
	scope := tools.ToolScope{Root: root, Session: "session-keep"}
	keep := OpenOptions{KeepOnClose: true}
	first := openScope(t, p, keep, scope)
	first.Close()
	creates, execs, starts, removed, _ := f.snapshot()
	if len(creates) != 1 || len(removed) != 0 {
		t.Fatalf("keep-on-close: creates %d removed %v", len(creates), removed)
	}
	second := openScope(t, p, keep, scope)
	second.Close()
	creates, execs, starts, _, _ = f.snapshot()
	if len(creates) != 1 || len(execs) != 2 || len(starts) != 1 {
		t.Fatalf("reconnect created again: creates %d execs %d starts %d", len(creates), len(execs), len(starts))
	}
	f.stopAll()
	third := openScope(t, p, keep, scope)
	third.Close()
	creates, _, starts, _, _ = f.snapshot()
	if len(creates) != 1 || len(starts) != 2 {
		t.Fatalf("stopped container was not started: creates %d starts %d", len(creates), len(starts))
	}
}

func TestOpenDestroysAMismatchedContainer(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root := t.TempDir()
	p := testProvider(t, f, nil)
	scope := tools.ToolScope{Root: root, Session: "session-image"}
	openScope(t, p, OpenOptions{KeepOnClose: true}, scope).Close()
	f.setImage("test:image", "sha256:image-two")
	openScope(t, p, OpenOptions{KeepOnClose: true}, scope).Close()
	creates, _, _, removed, containers := f.snapshot()
	if len(creates) != 2 || len(removed) != 1 || removed[0] != "container-1" || len(containers) != 1 || creates[1].Image != "sha256:image-two" {
		t.Fatalf("image change: creates %d removed %v containers %d", len(creates), removed, len(containers))
	}
	// A different resource limit is part of the identity as well.
	limited := testProvider(t, f, func(o *Options) { o.Memory = "1g" })
	openScope(t, limited, OpenOptions{KeepOnClose: true}, scope).Close()
	creates, _, _, removed, _ = f.snapshot()
	if len(creates) != 3 || len(removed) != 2 {
		t.Fatalf("limit change: creates %d removed %v", len(creates), removed)
	}
}

func TestDestroyIsIdempotentAndRefusedWhileBound(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root := t.TempDir()
	p := testProvider(t, f, nil)
	scope := tools.ToolScope{Root: root, Session: "session-destroy"}
	binding := openScope(t, p, OpenOptions{KeepOnClose: true}, scope)
	ctx := context.Background()
	if err := p.Destroy(ctx, root); err == nil {
		t.Fatal("destroy succeeded under an open binding")
	}
	if _, err := p.OpenTools(OpenOptions{})(ctx, scope); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("second binding = %v", err)
	}
	binding.Close()
	if err := p.Destroy(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(ctx, root); err != nil {
		t.Fatalf("second destroy = %v", err)
	}
	_, _, _, removed, containers := f.snapshot()
	if len(removed) != 1 || len(containers) != 0 {
		t.Fatalf("removed %v containers %d", removed, len(containers))
	}
	if _, err := p.OpenTools(OpenOptions{})(ctx, scope); err != nil {
		t.Fatalf("rebinding after close = %v", err)
	}
}

func TestSealedEnvironmentNeverAppearsInDaemonRequests(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root := t.TempDir()
	t.Setenv("SEALED_TOKEN", "sealed-value-9f8e7d")
	policy := sandbox.DefaultConfig()
	policy.PassEnv = []string{"SEALED_TOKEN"}
	p := testProvider(t, f, func(o *Options) { o.Policy = policy })
	binding := openScope(t, p, nativeLoad("bash"), tools.ToolScope{Root: root})
	bashTool, _ := binding.Registry.Get("bash")
	execution, err := binding.Registry.ExecuteTool(context.Background(), bashTool, map[string]any{"command": "echo $SEALED_TOKEN"}, time.Minute)
	if err != nil || !strings.Contains(execution.Output.Text, "sealed-value-9f8e7d") {
		t.Fatalf("sealed value did not reach the tool: %+v %v", execution.Output, err)
	}
	binding.Close()
	creates, execs, _, _, _ := f.snapshot()
	for _, body := range []any{creates, execs} {
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "sealed-value-9f8e7d") {
			t.Fatalf("sealed value in a daemon request: %s", encoded)
		}
	}
	if !strings.Contains(f.received(), "sealed-value-9f8e7d") {
		t.Fatal("sealed value did not travel over the exec stream")
	}
}

func TestOpenFailsClosed(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root := t.TempDir()
	if _, err := New(Options{Image: "test:image", Host: f.host(), Policy: sandbox.Config{AllowUnixSockets: []string{"/tmp/agent.sock"}}}); !errors.Is(err, ErrUnsupportedPolicy) {
		t.Fatalf("unix sockets = %v", err)
	}
	if _, err := New(Options{Image: "test:image", Host: f.host(), Mode: ModeCopy}); !errors.Is(err, ErrUnsupportedPolicy) {
		t.Fatalf("copy mode = %v", err)
	}
	if _, err := New(Options{Host: f.host()}); err == nil {
		t.Fatal("missing image accepted")
	}
	if _, err := New(Options{Image: "x", Host: "ssh://user@host"}); !errors.Is(err, ErrUnsupportedHost) {
		t.Fatalf("ssh host = %v", err)
	}
	missing := testProvider(t, f, func(o *Options) { o.Image = "absent:image" })
	if _, err := missing.OpenTools(OpenOptions{})(context.Background(), tools.ToolScope{Root: root}); !errors.Is(err, ErrImageMissing) {
		t.Fatalf("missing image = %v", err)
	}
	if creates, _, _, _, _ := f.snapshot(); len(creates) != 0 {
		t.Fatal("a container was created for a missing image")
	}
	// Denied reads inside the root cannot be honored.
	p := testProvider(t, f, nil)
	if _, err := p.OpenTools(OpenOptions{})(context.Background(), tools.ToolScope{Root: root, Grant: tools.ExecutionGrant{DeniedReads: []string{filepath.Join(root, "secret")}}}); err == nil || !strings.Contains(err.Error(), "cannot be hidden") {
		t.Fatalf("denied read inside the root = %v", err)
	}
	if creates, _, _, _, _ := f.snapshot(); len(creates) != 0 {
		t.Fatal("a container was created for an unsatisfiable scope")
	}
	f.server.Close()
	if _, err := p.OpenTools(OpenOptions{})(context.Background(), tools.ToolScope{Root: root}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("daemon down = %v", err)
	}
}

func TestOpenMountsHelperScratchAndDeniedDNS(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root, scratch := t.TempDir(), t.TempDir()
	helperBinary := filepath.Join(t.TempDir(), "polly-linux")
	if err := os.WriteFile(helperBinary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.DefaultConfig()
	policy.AllowNetwork, policy.DenyDNS = true, true
	p := testProvider(t, f, func(o *Options) { o.Policy = policy; o.Helper = helperBinary })
	binding := openScope(t, p, OpenOptions{}, tools.ToolScope{Root: root, Grant: tools.ExecutionGrant{ReadOnly: true, Scratch: scratch}})
	defer binding.Close()
	creates, execs, _, _, _ := f.snapshot()
	host := creates[0].HostConfig
	if host.NetworkMode != "bridge" || creates[0].Labels[labelNetwork] != "bridge-nodns" {
		t.Fatalf("network %q labels %v", host.NetworkMode, creates[0].Labels)
	}
	targets := map[string]engineMount{}
	for _, m := range host.Mounts {
		targets[m.Target] = m
	}
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	canonicalScratch, _ := filepath.EvalSymlinks(scratch)
	if m := targets[canonicalRoot]; !m.ReadOnly {
		t.Fatalf("read-only scope mounted its root writable: %+v", host.Mounts)
	}
	if m := targets[canonicalScratch]; m.Source == "" || m.ReadOnly {
		t.Fatalf("scratch mount %+v", host.Mounts)
	}
	if m := targets["/etc/resolv.conf"]; m.Source == "" || !m.ReadOnly {
		t.Fatalf("resolver mount %+v", host.Mounts)
	}
	canonicalHelper, _ := filepath.EvalSymlinks(helperBinary)
	if m := targets[helperMountPath]; m.Source != canonicalHelper || !m.ReadOnly {
		t.Fatalf("helper mount %+v", host.Mounts)
	}
	if !slices.Equal(execs[0].Cmd, []string{helperMountPath, "sandbox", "helper"}) {
		t.Fatalf("exec %v", execs[0].Cmd)
	}
}
