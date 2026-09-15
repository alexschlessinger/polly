package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// Provider opens container-backed tool bindings against one daemon and one
// image. It is the only owner of the connections it opens.
type Provider struct {
	opts     Options
	endpoint endpoint
	engine   *engine

	mu     sync.Mutex
	active map[string][]*binding // canonical roots bound in this process
}

// Info describes the daemon.
type Info struct {
	Version    string
	APIVersion string
	OS         string
	Arch       string
	Rootless   bool
}

// New validates opts and resolves the daemon address. It does not contact
// the daemon.
func New(opts Options) (*Provider, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	ep, err := resolveEndpoint(opts.Host)
	if err != nil {
		return nil, err
	}
	if opts.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		opts.HomeDir = filepath.Join(home, ".pollytool", "docker")
	}
	return &Provider{opts: opts, endpoint: ep, engine: newEngine(ep), active: map[string][]*binding{}}, nil
}

// Mode reports the workspace transport.
func (p *Provider) Mode() Mode { return p.opts.Mode }

// Endpoint names the daemon address for messages.
func (p *Provider) Endpoint() string { return p.endpoint.String() }

// Ping checks that the daemon answers and reports what it is.
func (p *Provider) Ping(ctx context.Context) (Info, error) {
	if err := p.engine.ping(ctx); err != nil {
		return Info{}, err
	}
	version, err := p.engine.version(ctx)
	if err != nil {
		return Info{}, err
	}
	info := Info{Version: version.Version, APIVersion: version.APIVersion, OS: version.OS, Arch: version.Arch}
	if detail, err := p.engine.info(ctx); err == nil {
		info.Rootless = detail.Rootless
	}
	return info, nil
}

// ResolveImage resolves the configured image reference to its ID. A missing
// image fails closed; nothing is pulled.
func (p *Provider) ResolveImage(ctx context.Context) (string, error) {
	image, err := p.engine.imageInspect(ctx, p.opts.Image)
	if err != nil {
		return "", err
	}
	return image.ID, nil
}

// OpenTools returns the OpenTools function that opens one container per
// scope and serves its tools through the helper.
func (p *Provider) OpenTools(o OpenOptions) tools.OpenTools {
	return func(ctx context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
		return p.open(ctx, scope, o)
	}
}

// Destroy removes every container of this provider's daemon that serves
// root, whichever session created it. It is idempotent and refuses while a
// binding for root is open in this process.
func (p *Provider) Destroy(ctx context.Context, root string) error {
	canonical, err := canonicalPath(root)
	if err != nil {
		canonical = filepath.Clean(root)
	}
	p.mu.Lock()
	bound := len(p.active[canonical]) > 0
	p.mu.Unlock()
	if bound {
		return fmt.Errorf("container for %s is bound in this process", canonical)
	}
	return p.removeAll(ctx, []string{labelRoot + "=" + canonical})
}

// Resync makes the copy-mode binding open over root observe the host
// checkout's current files and base commit, after a host-side write to the
// checkout (an integration, an apply, a snapshot restore). It is refused
// while a call or sync is in flight, and is a no-op for a bind-mode
// binding, whose container already sees the host files.
func (p *Provider) Resync(ctx context.Context, root string) error {
	canonical, err := canonicalPath(root)
	if err != nil {
		canonical = filepath.Clean(root)
	}
	p.mu.Lock()
	bindings := append([]*binding(nil), p.active[canonical]...)
	p.mu.Unlock()
	if len(bindings) == 0 {
		return fmt.Errorf("no container binding is open for %s", canonical)
	}
	var errs []error
	for _, b := range bindings {
		errs = append(errs, b.resync(ctx))
	}
	return errors.Join(errs...)
}

func (p *Provider) removeAll(ctx context.Context, labels []string) error {
	containers, err := p.engine.containerList(ctx, labels)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range containers {
		if err := p.engine.containerRemove(ctx, c.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// binding is one open container binding.
type binding struct {
	provider  *Provider
	root      string
	container string
	created   bool
	keep      bool
	mirror    tools.ToolBinding
	once      sync.Once
	err       error

	// Copy mode: the transport, the base the copy holds, and the sync
	// barrier; nil in bind mode.
	copy    *copyTransport
	base    string
	tree    string
	syncer  *syncer
	proxies *mirror
}

// claim registers a binding for root. Bind mode admits several bindings
// over one root, each its own helper in the shared container: the
// coordinator serialises their use through its context lock, and a
// workflow keeps its binding across steps while a member runs its own. Copy
// mode has one synchronisation state per root and admits one binding.
func (p *Provider) claim(root string, b *binding) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.opts.Mode == ModeCopy && len(p.active[root]) > 0 {
		return fmt.Errorf("copy for %s is already bound in this process", root)
	}
	p.active[root] = append(p.active[root], b)
	return nil
}

func (p *Provider) release(root string, b *binding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	bindings := p.active[root]
	for i, candidate := range bindings {
		if candidate == b {
			bindings = append(bindings[:i], bindings[i+1:]...)
			break
		}
	}
	if len(bindings) == 0 {
		delete(p.active, root)
	} else {
		p.active[root] = bindings
	}
}

func (p *Provider) open(ctx context.Context, scope tools.ToolScope, o OpenOptions) (result tools.ToolBinding, err error) {
	root, err := canonicalDir(scope.Root)
	if err != nil {
		return tools.ToolBinding{}, fmt.Errorf("workspace root: %w", err)
	}
	b := &binding{provider: p, root: root, keep: o.KeepOnClose}
	if err := p.claim(root, b); err != nil {
		return tools.ToolBinding{}, err
	}
	defer func() {
		if err != nil {
			p.release(root, b)
		}
	}()
	if err := p.engine.ping(ctx); err != nil {
		return tools.ToolBinding{}, err
	}
	imageID, err := p.ResolveImage(ctx)
	if err != nil {
		return tools.ToolBinding{}, err
	}
	resolvEmpty := ""
	network := p.opts.network()
	if network.Allow && network.DenyDNS {
		if resolvEmpty, err = p.emptyResolver(); err != nil {
			return tools.ToolBinding{}, err
		}
	}
	scratch := ""
	if scope.Grant.Scratch != "" {
		if scratch, err = canonicalDir(scope.Grant.Scratch); err != nil {
			return tools.ToolBinding{}, fmt.Errorf("scratch: %w", err)
		}
	}
	var mounts []mount
	var git GitAccess
	containerScratch := scratch
	sourceRoot := scope.SourceRoot
	if p.opts.Mode == ModeCopy {
		if git, err = p.opts.Git(ctx); err != nil {
			return tools.ToolBinding{}, fmt.Errorf("copy mode git: %w", err)
		}
		if b.base, b.tree, err = git.Base(ctx, root); err != nil {
			return tools.ToolBinding{}, fmt.Errorf("copy base: %w", err)
		}
		mounts, containerScratch = copyMounts(root, scratch, resolvEmpty, p.opts.Helper)
		// Operator-selected paths of the source checkout do not exist in
		// the copy; nothing is rebound from it.
		sourceRoot = ""
	} else {
		mounts, err = deriveMounts(scope, p.opts.Policy, o.SkillRoots, resolvEmpty, p.opts.Helper)
		if err != nil {
			return tools.ToolBinding{}, err
		}
	}
	session := scope.Session
	if session == "" {
		session = anonymousSession()
	}
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if info, err := p.engine.info(ctx); err == nil && info.Rootless {
		// A rootless daemon maps the host user to the container's root.
		user = "0:0"
	}
	spec := containerSpec{
		session: session, root: root, scratch: containerScratch, readOnly: scope.Grant.ReadOnly, mode: p.opts.Mode,
		imageID: imageID, protocol: protocol.Version, network: network,
		memory: p.opts.memoryBytes, nanoCPUs: p.opts.nanoCPUs, pids: p.opts.PIDs, user: user, mounts: mounts,
	}
	spec.labels = computeLabels(p.opts.Labels, spec)
	spec.name = containerName(session, root)

	b.container, b.created, err = p.reconnectOrCreate(ctx, spec)
	if err != nil {
		return tools.ToolBinding{}, err
	}
	defer func() {
		if err != nil && b.created {
			removeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = p.engine.containerRemove(removeCtx, b.container)
			stop()
		}
	}()
	if p.opts.Mode == ModeCopy {
		b.copy = &copyTransport{engine: p.engine, container: b.container, git: git, root: root}
		if b.created {
			scratchName := ""
			if containerScratch != "" && filepath.Dir(containerScratch) == filepath.Dir(root) {
				scratchName = filepath.Base(containerScratch)
			}
			if err = b.copy.putLayout(ctx, filepath.Dir(root), filepath.Base(root), scratchName, b.base); err != nil {
				return tools.ToolBinding{}, err
			}
		}
	}
	helperPath := ""
	if p.opts.Helper != "" {
		helperPath = helperMountPath
	}
	execID, err := p.engine.execCreate(ctx, b.container, helperExec(spec, helperPath))
	if err != nil {
		return tools.ToolBinding{}, fmt.Errorf("start helper: %w", err)
	}
	stream, err := p.engine.execStart(ctx, execID)
	if err != nil {
		return tools.ToolBinding{}, fmt.Errorf("start helper: %w", err)
	}
	heartbeat := p.opts.Heartbeat
	if heartbeat == 0 {
		heartbeat = defaultHeartbeat
	}
	conn := protocol.NewConn(newStdcopyReader(stream.reader, helperStderr{}), stream.conn)
	s := newSession(conn, func() error { return errors.Join(stream.closeWrite(), stream.conn.Close()) }, heartbeat)
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	hello := protocol.Hello{
		Protocol: protocol.Version, Session: session, Mode: string(spec.mode), Root: root, SourceRoot: sourceRoot,
		Scratch: containerScratch, ReadOnly: scope.Grant.ReadOnly, DeniedReads: scope.Grant.DeniedReads, DeniedWrites: scope.Grant.DeniedWrites,
		Network:  protocol.NetworkPolicy{Allow: network.Allow, DenyDNS: network.DenyDNS},
		AllowEnv: p.opts.Policy.AllowEnv, PassEnv: p.opts.Policy.PassEnv, Env: sandbox.SelectedEnv(p.opts.Policy),
		Home: containerHome, GitIdent: protocol.GitIdentity{Name: p.opts.GitIdent.Name, Email: p.opts.GitIdent.Email},
	}
	welcome, err := s.hello(ctx, hello)
	if err != nil {
		var reply *ProtocolError
		if !errors.As(err, &reply) {
			if execErr := p.helperExitError(ctx, execID); execErr != nil {
				return tools.ToolBinding{}, execErr
			}
		}
		return tools.ToolBinding{}, fmt.Errorf("helper hello: %w (set Options.Helper to mount a matching polly binary)", err)
	}
	if welcome.UID != os.Getuid() && user != "0:0" {
		message := fmt.Sprintf("helper runs as uid %d, host user is %d: files it writes may be owned differently", welcome.UID, os.Getuid())
		if runtime.GOOS == "linux" && p.endpoint.network == "unix" {
			return tools.ToolBinding{}, errors.New(message)
		}
		slog.Warn("docker_helper_uid", "message", message)
	}
	if b.copy != nil {
		b.copy.session = s
		if b.created {
			if err = b.copy.bootstrap(ctx, b.base); err != nil {
				return tools.ToolBinding{}, fmt.Errorf("bootstrap copy: %w", err)
			}
		} else if err = b.copy.reset(ctx, b.base, true); err != nil {
			return tools.ToolBinding{}, fmt.Errorf("resync copy: %w", err)
		}
		if err = b.copy.push(ctx, b.tree); err != nil {
			return tools.ToolBinding{}, fmt.Errorf("push host changes: %w", err)
		}
	}
	specs := make([]protocol.ToolSpec, 0, len(o.Tools))
	for _, info := range o.Tools {
		specs = append(specs, protocol.ToolSpec{Name: info.Name, Type: info.Type, Source: info.Source})
	}
	loaded, err := s.load(ctx, protocol.Load{Tools: specs, Sources: o.Sources, SkillRoots: o.SkillRoots, ActiveSkills: o.ActiveSkills, AutoActivate: o.AutoActivate, AllowedTools: scope.AllowedTools})
	if err != nil {
		return tools.ToolBinding{}, fmt.Errorf("load tools in container: %w", err)
	}
	for _, warning := range loaded.Warnings {
		slog.Warn("docker_helper_load", "warning", warning)
	}
	b.proxies, b.mirror = bindSession(scope, s, loaded)
	if b.copy != nil && !scope.Grant.ReadOnly {
		b.syncer = newSyncer(b.copy.collect)
		b.proxies.before = func(ctx context.Context) error {
			if !b.syncer.dirty {
				return nil
			}
			if err := b.syncer.after(ctx); err != nil {
				return syncFailed(err)
			}
			b.syncer.dirty = false
			return nil
		}
		b.proxies.after = func(ctx context.Context, info protocol.ToolInfo, result protocol.Result, err error) error {
			if !writeCapable(info, result, err) {
				return nil
			}
			if syncErr := b.syncer.after(ctx); syncErr != nil {
				b.syncer.dirty = true
				return syncFailed(syncErr)
			}
			b.syncer.dirty = false
			return nil
		}
	}
	result = b.mirror
	result.Close = b.close
	return result, nil
}

// copyMounts is the copy-mode mount set: an anonymous volume at the root's
// parent holding the tree and the scratch, the helper, and the resolver
// override. Nothing from the host is mounted. A scratch outside the parent
// lives in the container's private temp.
func copyMounts(root, scratch, resolvEmpty, helper string) ([]mount, string) {
	mounts := []mount{{Type: "volume", Target: filepath.Dir(root)}}
	containerScratch := scratch
	if scratch != "" && filepath.Dir(scratch) != filepath.Dir(root) {
		containerScratch = "/tmp/polly-scratch"
	}
	if helper != "" {
		if canonical, err := canonicalPath(helper); err == nil {
			mounts = append(mounts, mount{Type: "bind", Source: canonical, Target: helperMountPath, ReadOnly: true})
		}
	}
	if resolvEmpty != "" {
		mounts = append(mounts, mount{Type: "bind", Source: resolvEmpty, Target: "/etc/resolv.conf", ReadOnly: true})
	}
	return mounts, containerScratch
}

// resync brings a copy to the host checkout's current base and files. It
// refuses while a call or a sync is in flight.
func (b *binding) resync(ctx context.Context) error {
	if b.copy == nil {
		return nil
	}
	if b.proxies.inflight.Load() > 0 {
		return ErrBusy
	}
	run := func() error {
		commit, tree, err := b.copy.git.Base(ctx, b.root)
		if err != nil {
			return err
		}
		if err := b.copy.reset(ctx, commit, commit != b.base); err != nil {
			return err
		}
		b.base, b.tree = commit, tree
		return b.copy.push(ctx, tree)
	}
	if b.syncer == nil {
		return run()
	}
	if err := b.syncer.exclusive(run); err != nil {
		return err
	}
	b.syncer.dirty = false
	return nil
}

// helperExitError reports why the helper exited, when it did.
func (p *Provider) helperExitError(ctx context.Context, execID string) error {
	detail, err := p.engine.execInspect(context.WithoutCancel(ctx), execID)
	if err != nil || detail.Running {
		return nil
	}
	return fmt.Errorf("helper exited with status %d before answering hello: the image needs a polly matching this build (set Options.Helper to mount one), or its working directory is missing", detail.ExitCode)
}

// reconnectOrCreate finds this scope's container by session and root. One
// whose labels match is kept and started if stopped; any other is removed;
// none means create.
func (p *Provider) reconnectOrCreate(ctx context.Context, spec containerSpec) (id string, created bool, err error) {
	existing, err := p.engine.containerList(ctx, []string{labelSession + "=" + spec.session, labelRoot + "=" + spec.root})
	if err != nil {
		return "", false, err
	}
	var keep *containerSummary
	for i := range existing {
		c := existing[i]
		if keep == nil && labelsMatch(c.Labels, spec.labels) {
			keep = &existing[i]
			continue
		}
		slog.Info("docker_container_replaced", "container", c.ID, "reason", specMismatch(c.Labels, spec.labels))
		if err := p.engine.containerRemove(ctx, c.ID); err != nil {
			return "", false, err
		}
	}
	if keep != nil {
		if keep.State != "running" {
			if err := p.engine.containerStart(ctx, keep.ID); err != nil {
				return "", false, fmt.Errorf("start container: %w", err)
			}
		}
		return keep.ID, false, nil
	}
	id, err = p.engine.containerCreate(ctx, spec.name, createRequest(spec))
	if err != nil {
		return "", false, fmt.Errorf("create container: %w", err)
	}
	if err := p.engine.containerStart(ctx, id); err != nil {
		removeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = p.engine.containerRemove(removeCtx, id)
		stop()
		return "", false, fmt.Errorf("start container: %w", err)
	}
	if detail, err := p.engine.containerInspect(ctx, id); err == nil && !detail.State.Running {
		removeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = p.engine.containerRemove(removeCtx, id)
		stop()
		return "", false, fmt.Errorf("container exited right after start (%s): the image needs sleep from coreutils as its init", detail.State.Status)
	}
	return id, true, nil
}

// close detaches from the container and, for a standalone binding, removes
// it. It is idempotent.
func (b *binding) close() error {
	b.once.Do(func() {
		b.err = b.mirror.Close()
		b.provider.release(b.root, b)
		if !b.keep && !b.provider.bound(b.root) {
			ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			b.err = errors.Join(b.err, b.provider.engine.containerRemove(ctx, b.container))
			stop()
		}
	})
	return b.err
}

// emptyResolver is the host file bound over /etc/resolv.conf when DNS is
// denied: an empty resolver configuration.
func (p *Provider) emptyResolver() (string, error) {
	path := filepath.Join(p.opts.HomeDir, "resolv.conf.empty")
	if err := os.MkdirAll(p.opts.HomeDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return "", err
	}
	return canonicalPath(path)
}

func anonymousSession() string {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	return "anonymous-" + hex.EncodeToString(nonce[:])
}

// helperStderr forwards the helper's stderr to the log.
type helperStderr struct{}

func (helperStderr) Write(p []byte) (int, error) {
	slog.Debug("docker_helper_stderr", "text", string(p))
	return len(p), nil
}

// bound reports whether any binding over root is open in this process.
func (p *Provider) bound(root string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active[root]) > 0
}
