package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/worktree"
)

// Backend and mode flag values.
const (
	sandboxBackendAuto   = "auto"
	sandboxBackendNative = "native"
	sandboxBackendDocker = "docker"
	sandboxModeAuto      = "auto"
)

// imageFileName is the repository file that may suggest an image. It is a
// hint only: a checkout could name any image, so the flag or the variable
// selects one.
const imageFileName = ".polly/image"

func validateSandboxBackend(value string) error {
	switch value {
	case sandboxBackendAuto, sandboxBackendNative, sandboxBackendDocker:
		return nil
	}
	return fmt.Errorf("sandbox backend must be auto, native or docker, not %q", value)
}

func validateSandboxMode(value string) error {
	switch value {
	case sandboxModeAuto, string(docker.ModeBind), string(docker.ModeCopy):
		return nil
	}
	return fmt.Errorf("sandbox mode must be auto, bind or copy, not %q", value)
}

// sandboxBackend is the tool backend a run uses. Native has no provider;
// docker carries the provider and what selected it, for the posture line.
type sandboxBackend struct {
	name     string
	provider *docker.Provider
	image    string
	imageID  string
	mode     docker.Mode
	policy   sandbox.Config
	// fallback explains why docker was configured but native is used.
	fallback string
	// hints are startup lines for the operator, such as a repository image
	// file that was not selected.
	hints []string

	config       *Config
	skillRoots   []string
	privatePaths []string
	adminMu      sync.Mutex
	admin        *tools.ToolRegistry
}

func (b *sandboxBackend) docker() bool { return b != nil && b.provider != nil }

// resolveSandboxBackend decides between native tools and a container:
// nothing configured is native, silently; an image configured under auto
// selects docker when the daemon answers and falls back to native with a
// notice otherwise; explicit docker fails closed; a missing image fails
// closed in both, because polly never pulls.
func resolveSandboxBackend(ctx context.Context, config *Config, warnings *broadWritablePathWarner, skillRoots []string, privatePaths []string) (*sandboxBackend, error) {
	native := &sandboxBackend{name: sandboxBackendNative, config: config, skillRoots: skillRoots, privatePaths: privatePaths}
	backend := config.SandboxBackend
	if backend == "" {
		backend = sandboxBackendAuto
	}
	if config.NoSandbox || backend == sandboxBackendNative {
		return native, nil
	}
	image := strings.TrimSpace(config.SandboxImage)
	if image == "" {
		if backend == sandboxBackendDocker {
			return nil, errors.New("sandbox backend docker requested but unavailable: no image configured; pass --sandbox-image or set POLLYTOOL_SANDBOX_IMAGE (polly sandbox build makes one)")
		}
		if hint := imageFileHint(); hint != "" {
			native.hints = append(native.hints, hint)
		}
		return native, nil
	}
	fallback := func(reason string) (*sandboxBackend, error) {
		if backend == sandboxBackendDocker {
			return nil, fmt.Errorf("sandbox backend docker requested but unavailable: %s", reason)
		}
		native.fallback = fmt.Sprintf("docker image %s is configured but %s; using the native sandbox", image, reason)
		return native, nil
	}
	policy, err := baseSandboxPolicy(config, warnings, skillRoots, privatePaths...)
	if err != nil {
		return nil, err
	}
	candidate := &sandboxBackend{name: sandboxBackendDocker, image: image, policy: policy, config: config, skillRoots: skillRoots, privatePaths: privatePaths}
	opts, err := candidate.dockerOptions(image, policy)
	if err != nil {
		return fallback(err.Error())
	}
	provider, err := docker.New(opts)
	if err != nil {
		return fallback(err.Error())
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := provider.Ping(probeCtx); err != nil {
		return fallback(fmt.Sprintf("the daemon at %s did not answer (%v)", provider.Endpoint(), err))
	}
	imageID, err := provider.ResolveImage(probeCtx)
	if errors.Is(err, docker.ErrImageMissing) {
		return nil, fmt.Errorf("sandbox backend docker: image %s is not present on the daemon at %s; polly never pulls, so build it with polly sandbox build or pull it yourself", image, provider.Endpoint())
	}
	if err != nil {
		return fallback(err.Error())
	}
	candidate.provider, candidate.imageID, candidate.mode = provider, imageID, opts.Mode
	return candidate, nil
}

// dockerOptions assembles the provider options from the run's configuration
// and environment. Mode auto is bind for a local daemon and copy otherwise.
func (b *sandboxBackend) dockerOptions(image string, policy sandbox.Config) (docker.Options, error) {
	mode := docker.Mode(b.config.SandboxMode)
	if b.config.SandboxMode == "" || b.config.SandboxMode == sandboxModeAuto {
		_, local, err := docker.ResolveHost("")
		if err != nil {
			return docker.Options{}, err
		}
		mode = docker.ModeBind
		if !local {
			mode = docker.ModeCopy
		}
	}
	name, email := sandbox.GitUserIdentity()
	opts := docker.Options{
		Image:    image,
		Mode:     mode,
		Policy:   policy,
		GitIdent: docker.GitIdentity{Name: name, Email: email},
		Helper:   os.Getenv("POLLYTOOL_SANDBOX_HELPER"),
		Memory:   os.Getenv("POLLYTOOL_SANDBOX_MEMORY"),
		CPUs:     os.Getenv("POLLYTOOL_SANDBOX_CPUS"),
		Git:      b.gitAccess,
	}
	if pids := os.Getenv("POLLYTOOL_SANDBOX_PIDS"); pids != "" {
		value, err := strconv.Atoi(pids)
		if err != nil {
			return docker.Options{}, fmt.Errorf("POLLYTOOL_SANDBOX_PIDS: %w", err)
		}
		opts.PIDs = value
	}
	return opts, nil
}

// adminRegistry is the native registry the host's own Git plumbing runs
// with under the docker backend: the worktree manager needs a process
// sandbox that a registry of proxies does not have. It loads no tools.
func (b *sandboxBackend) adminRegistry() (*tools.ToolRegistry, error) {
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if b.admin != nil {
		return b.admin, nil
	}
	opts, probe, err := sandboxRegistryOptionsWithWarnings(b.config, nil, b.skillRoots, b.privatePaths...)
	if err != nil {
		return nil, err
	}
	if err := probe.wait(context.Background()); err != nil {
		return nil, err
	}
	b.admin = tools.NewToolRegistry(nil, opts...)
	return b.admin, nil
}

// gitAccess is copy mode's host Git: a worktree manager over the working
// directory, with a runtime directory of its own under the backend's home.
func (b *sandboxBackend) gitAccess(ctx context.Context) (docker.GitAccess, error) {
	registry, err := b.adminRegistry()
	if err != nil {
		return nil, err
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(root))
	directory := filepath.Join(home, ".pollytool", "docker", "git", hex.EncodeToString(sum[:])[:16])
	return worktree.New(ctx, worktree.Config{Root: root, Directory: directory, Registry: registry, PrivatePaths: b.privatePaths})
}

// openWorktrees is the swarm's Git manager constructor under docker: the
// coordinator's configuration with the administrative registry.
func (b *sandboxBackend) openWorktrees(ctx context.Context, cfg worktree.Config) (*worktree.Manager, error) {
	registry, err := b.adminRegistry()
	if err != nil {
		return nil, err
	}
	cfg.Registry = registry
	return worktree.New(ctx, cfg)
}

// openOptions is what every container of this run loads: the session's
// persisted tools or the command line's sources, and its skills.
func (b *sandboxBackend) openOptions(config *Config, metadata *sessions.Metadata, skillResult *skillCatalogResult, keep bool) docker.OpenOptions {
	o := docker.OpenOptions{SkillRoots: b.skillRoots, KeepOnClose: keep}
	if len(config.Tools) > 0 {
		o.Sources = append([]string(nil), config.Tools...)
	} else if metadata != nil {
		o.Tools = append([]tools.ToolLoaderInfo(nil), metadata.ActiveTools...)
	}
	if metadata != nil {
		o.ActiveSkills = append([]string(nil), metadata.ActiveSkills...)
	}
	if skillResult != nil {
		o.AutoActivate = append([]string(nil), skillResult.autoActivate...)
	}
	return o
}

// standaloneScope is the parent's or a one-shot run's scope: the working
// directory, the session's identity, and the private paths denied.
func (b *sandboxBackend) standaloneScope(session sessions.Session) (tools.ToolScope, error) {
	root, err := os.Getwd()
	if err != nil {
		return tools.ToolScope{}, err
	}
	scope := tools.ToolScope{Root: root, Grant: tools.ExecutionGrant{DeniedReads: b.privatePaths}}
	if session != nil {
		if identity, ok := session.(sessions.ViewIdentity); ok {
			scope.Session = identity.ViewID()
		}
	}
	return scope, nil
}

// openStandalone opens the run's own container binding.
func (b *sandboxBackend) openStandalone(ctx context.Context, config *Config, session sessions.Session, metadata *sessions.Metadata, skillResult *skillCatalogResult) (tools.ToolBinding, error) {
	scope, err := b.standaloneScope(session)
	if err != nil {
		return tools.ToolBinding{}, err
	}
	binding, err := b.provider.OpenTools(b.openOptions(config, metadata, skillResult, false))(ctx, scope)
	if err != nil {
		return tools.ToolBinding{}, fmt.Errorf("sandbox backend docker: %w", err)
	}
	return binding, nil
}

// imageFileHint reports a repository image file, which never selects the
// image by itself.
func imageFileHint() string {
	data, err := os.ReadFile(imageFileName)
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(data))
	if ref == "" {
		return ""
	}
	return fmt.Sprintf("%s names the image %s; pass --sandbox-image %s or set POLLYTOOL_SANDBOX_IMAGE to run tools in it", imageFileName, ref, ref)
}
