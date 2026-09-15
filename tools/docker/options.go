package docker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// Mode is how the workspace reaches the container.
type Mode string

const (
	// ModeBind mounts the host worktree at its own path; the daemon must be
	// local and its file sharing must cover the worktree.
	ModeBind Mode = "bind"
	// ModeCopy keeps a self-contained copy in the container, synchronised
	// with the host worktree. Not available in this version.
	ModeCopy Mode = "copy"
)

// NetworkPolicy is the container's network posture.
type NetworkPolicy struct {
	Allow   bool
	DenyDNS bool
}

// GitIdentity is written into the container's Git configuration from
// values; the host's configuration files never travel.
type GitIdentity struct {
	Name  string
	Email string
}

// DefaultPIDs bounds a container's processes when Options names no limit.
const DefaultPIDs = 4096

// ErrUnsupportedPolicy reports a sandbox policy the container cannot honor.
var ErrUnsupportedPolicy = errors.New("policy is not supported by the container backend")

// Options configures a Provider.
type Options struct {
	// Image is the image reference; it must already be present on the
	// daemon and is resolved to an image ID at open. Required.
	Image string
	// Host overrides the daemon address (DOCKER_HOST, the active context,
	// or the default socket otherwise).
	Host string
	// Mode defaults to ModeBind.
	Mode Mode
	// Memory, CPUs and PIDs are the container's resource limits: "2g",
	// "1.5", 4096. Empty means unlimited memory and CPU; PIDs default to
	// DefaultPIDs.
	Memory string
	CPUs   string
	PIDs   int
	// Policy is the host's prepared sandbox policy: its network posture,
	// environment selection, denied paths and write pins shape the
	// container. Unix-socket grants are unsupported and fail the open.
	Policy sandbox.Config
	// Network overrides the policy's network posture when set.
	Network *NetworkPolicy
	// GitIdent is the identity written inside the container.
	GitIdent GitIdentity
	// Helper is a host path to a Linux polly binary mounted read-only into
	// the container as its helper, for development and for images that
	// predate a protocol change. Empty uses the image's own polly.
	Helper string
	// Labels are added to every container; polly's own labels win.
	Labels map[string]string
	// HomeDir holds the backend's host-side runtime files (an empty
	// resolver for DNS denial). Default ~/.pollytool/docker.
	HomeDir string
	// Heartbeat is the idle heartbeat interval; zero is the default.
	Heartbeat time.Duration
	// Git supplies the host's Git for copy mode, lazily; *worktree.Manager
	// implements GitAccess. Bind mode does not use it.
	Git func(context.Context) (GitAccess, error)

	memoryBytes int64
	nanoCPUs    int64
}

// OpenOptions is what one OpenTools function loads into every container it
// opens: the session's persisted tools and skills, and whether Close keeps
// the container.
type OpenOptions struct {
	Tools        []tools.ToolLoaderInfo
	SkillRoots   []string
	ActiveSkills []string
	AutoActivate []string
	// KeepOnClose leaves the container running when the binding closes,
	// for a loop that parks and resumes; a standalone run destroys it.
	KeepOnClose bool
}

func (o *Options) validate() error {
	if o.Image == "" {
		return errors.New("docker backend requires an image")
	}
	if o.Mode == "" {
		o.Mode = ModeBind
	}
	switch o.Mode {
	case ModeBind:
	case ModeCopy:
		if o.Git == nil {
			return errors.New("docker copy mode requires Options.Git")
		}
	default:
		return fmt.Errorf("unknown docker mode %q", o.Mode)
	}
	if o.PIDs == 0 {
		o.PIDs = DefaultPIDs
	}
	if o.PIDs < 0 {
		return errors.New("docker PID limit cannot be negative")
	}
	var err error
	if o.memoryBytes, err = parseMemory(o.Memory); err != nil {
		return err
	}
	if o.nanoCPUs, err = parseCPUs(o.CPUs); err != nil {
		return err
	}
	if len(o.Policy.AllowUnixSockets) > 0 {
		return fmt.Errorf("%w: allowUnixSockets and SSH agent forwarding", ErrUnsupportedPolicy)
	}
	return nil
}

// network is the effective network posture.
func (o *Options) network() NetworkPolicy {
	if o.Network != nil {
		return *o.Network
	}
	return NetworkPolicy{Allow: o.Policy.AllowNetwork, DenyDNS: o.Policy.DenyDNS}
}

// parseMemory reads "512m", "2g", "1024k" or a byte count.
func parseMemory(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, nil
	}
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(value, "g"):
		multiplier, value = 1<<30, strings.TrimSuffix(value, "g")
	case strings.HasSuffix(value, "m"):
		multiplier, value = 1<<20, strings.TrimSuffix(value, "m")
	case strings.HasSuffix(value, "k"):
		multiplier, value = 1<<10, strings.TrimSuffix(value, "k")
	case strings.HasSuffix(value, "b"):
		value = strings.TrimSuffix(value, "b")
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("invalid docker memory limit %q", value)
	}
	return amount * multiplier, nil
}

// parseCPUs reads a CPU count such as "1.5" into nano-CPUs.
func parseCPUs(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	cpus, err := strconv.ParseFloat(value, 64)
	if err != nil || cpus <= 0 || math.IsInf(cpus, 0) {
		return 0, fmt.Errorf("invalid docker CPU limit %q", value)
	}
	return int64(math.Round(cpus * 1e9)), nil
}
