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
	active map[string]bool // canonical roots bound in this process
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
	return &Provider{opts: opts, endpoint: ep, engine: newEngine(ep), active: map[string]bool{}}, nil
}

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
	bound := p.active[canonical]
	p.mu.Unlock()
	if bound {
		return fmt.Errorf("container for %s is bound in this process", canonical)
	}
	return p.removeAll(ctx, []string{labelRoot + "=" + canonical})
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
}

func (p *Provider) claim(root string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active[root] {
		return fmt.Errorf("container for %s is already bound in this process", root)
	}
	p.active[root] = true
	return nil
}

func (p *Provider) release(root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, root)
}

func (p *Provider) open(ctx context.Context, scope tools.ToolScope, o OpenOptions) (result tools.ToolBinding, err error) {
	root, err := canonicalDir(scope.Root)
	if err != nil {
		return tools.ToolBinding{}, fmt.Errorf("workspace root: %w", err)
	}
	if err := p.claim(root); err != nil {
		return tools.ToolBinding{}, err
	}
	defer func() {
		if err != nil {
			p.release(root)
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
	mounts, err := deriveMounts(scope, p.opts.Policy, o.SkillRoots, resolvEmpty, p.opts.Helper)
	if err != nil {
		return tools.ToolBinding{}, err
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
	scratch := ""
	if scope.Grant.Scratch != "" {
		if scratch, err = canonicalDir(scope.Grant.Scratch); err != nil {
			return tools.ToolBinding{}, fmt.Errorf("scratch: %w", err)
		}
	}
	spec := containerSpec{
		session: session, root: root, scratch: scratch, readOnly: scope.Grant.ReadOnly, mode: p.opts.Mode,
		imageID: imageID, protocol: protocol.Version, network: network,
		memory: p.opts.memoryBytes, nanoCPUs: p.opts.nanoCPUs, pids: p.opts.PIDs, user: user, mounts: mounts,
	}
	spec.labels = computeLabels(p.opts.Labels, spec)
	spec.name = containerName(session, root)

	b := &binding{provider: p, root: root, keep: o.KeepOnClose}
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
		Protocol: protocol.Version, Session: session, Mode: string(spec.mode), Root: root, SourceRoot: scope.SourceRoot,
		Scratch: scratch, ReadOnly: scope.Grant.ReadOnly, DeniedReads: scope.Grant.DeniedReads, DeniedWrites: scope.Grant.DeniedWrites,
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
	specs := make([]protocol.ToolSpec, 0, len(o.Tools))
	for _, info := range o.Tools {
		specs = append(specs, protocol.ToolSpec{Name: info.Name, Type: info.Type, Source: info.Source})
	}
	loaded, err := s.load(ctx, protocol.Load{Tools: specs, SkillRoots: o.SkillRoots, ActiveSkills: o.ActiveSkills, AutoActivate: o.AutoActivate, AllowedTools: scope.AllowedTools})
	if err != nil {
		return tools.ToolBinding{}, fmt.Errorf("load tools in container: %w", err)
	}
	for _, warning := range loaded.Warnings {
		slog.Warn("docker_helper_load", "warning", warning)
	}
	b.mirror = bindSession(scope, s, loaded)
	result = b.mirror
	result.Close = b.close
	return result, nil
}

// helperExitError reports why the helper exited, when it did.
func (p *Provider) helperExitError(ctx context.Context, execID string) error {
	detail, err := p.engine.execInspect(context.WithoutCancel(ctx), execID)
	if err != nil || detail.Running {
		return nil
	}
	return fmt.Errorf("helper exited with status %d before answering hello: the image needs a polly matching this build (set Options.Helper to mount one)", detail.ExitCode)
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
		if !b.keep {
			ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			b.err = errors.Join(b.err, b.provider.engine.containerRemove(ctx, b.container))
			stop()
		}
		b.provider.release(b.root)
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
