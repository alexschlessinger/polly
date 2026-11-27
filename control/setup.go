package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// LaunchOptions are what a launch resolves its setup from.
type LaunchOptions struct {
	Target Target
	// Provider is the provider every prompt of the setup is on.
	Provider string
	// BaseURL is the provider's base URL, when it is not the provider's own.
	BaseURL string
	// Cwd is the launch's directory, absolute.
	Cwd string
	// Environ is the launch's environment: its provider key, the setup's
	// variables, and what each prompt carries.
	Environ []string
	// SkillDirs are named skill directories, which replace discovery.
	SkillDirs []string
	// NoTools sends no tools and discovers no skills.
	NoTools bool
	// Workspace is where the tools work: ssh:HOST[:DIR], docker:CONTAINER[:DIR],
	// or empty for here.
	Workspace string
	// Servers and Scripts are MCP configurations and script tools the
	// setup loads, as named.
	Servers, Scripts []string
}

// ErrNoKey is a provider with no key in the environment.
type ErrNoKey struct{ Provider, Var string }

func (e *ErrNoKey) Error() string {
	return fmt.Sprintf("no API key configured for provider %q: export %s", e.Provider, e.Var)
}

// Prepare resolves a launch's setup and its key, and, for an exec://
// target, the binary's identity and build key: everything Reach needs.
func Prepare(ctx context.Context, opts LaunchOptions) (Bootstrap, error) {
	provider := strings.ToLower(opts.Provider)
	keyVar := ProviderKeyVar(provider)
	if keyVar == "" {
		return Bootstrap{}, fmt.Errorf("provider %q is not served by the control backend; it serves %s", opts.Provider, strings.Join(SupportedProviders(), ", "))
	}
	env := environMap(opts.Environ)
	apiKey := env[keyVar]
	if apiKey == "" && !(KeylessWithBaseURL(provider) && opts.BaseURL != "") {
		return Bootstrap{}, &ErrNoKey{Provider: provider, Var: keyVar}
	}
	if !filepath.IsAbs(opts.Cwd) {
		return Bootstrap{}, errors.New("the launch's directory must be absolute")
	}
	cwd := opts.Cwd
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	runtimeDir, err := RuntimeDir(opts.Environ)
	if err != nil {
		return Bootstrap{}, err
	}
	home := env["HOME"]
	b := Bootstrap{Target: opts.Target, Environ: opts.Environ}
	if opts.Target.Scheme == SchemeExec {
		if b.Program, err = ProgramOf(opts.Target.Binary); err != nil {
			return Bootstrap{}, err
		}
		if b.Version, err = Version(ctx, b.Program); err != nil {
			return Bootstrap{}, err
		}
		b.BuildKey = BuildKey(b.Version, b.Program)
	} else {
		// A daemon reached by its socket alone may be on another machine:
		// its paths are what the URL says, or this launch's.
		if opts.Target.Runtime != "" {
			runtimeDir = opts.Target.Runtime
		}
		if opts.Target.Dir != "" {
			cwd = opts.Target.Dir
		}
		if opts.Target.Home != "" {
			home = opts.Target.Home
		}
	}

	far := strings.TrimSpace(opts.Workspace)
	if far == "local" {
		far = ""
	}
	if far != "" {
		if far, err = normalWorkspace(far); err != nil {
			return Bootstrap{}, err
		}
	}
	var places []Place
	if !opts.NoTools && far == "" {
		if places, err = Places(cwd, home, filepath.Join(runtimeDir, "skills"), opts.SkillDirs); err != nil {
			return Bootstrap{}, fmt.Errorf("cannot discover skills: %w", err)
		}
	}
	setup := Setup{
		Provider: provider,
		BaseURL:  opts.BaseURL,
		Dir:      cwd,
		Home:     home,
		Natives:  !opts.NoTools,
		Servers:  []string{},
		Scripts:  []string{},
		Skills:   places,
		Trees:    filepath.Join(runtimeDir, "worktrees"),
	}
	if setup.Skills == nil {
		setup.Skills = []Place{}
	}
	if !opts.NoTools {
		setup.Servers = append(setup.Servers, opts.Servers...)
		setup.Scripts = append(setup.Scripts, opts.Scripts...)
	}
	if far != "" {
		_, rest, _ := strings.Cut(far, ":")
		if _, dir, ok := strings.Cut(rest, ":"); ok && dir != "" {
			setup.Dir = dir
		}
		setup.Home = ""
		setup.Trees = ""
		setup.Workspace = far
		if !opts.NoTools {
			setup.SkillDirs = opts.SkillDirs
		}
	}
	setup.Key, err = SetupKey(SetupKeyInput{
		Version:    b.Version,
		Program:    b.Program,
		RuntimeDir: runtimeDir,
		Provider:   setup.Provider,
		BaseURL:    setup.BaseURL,
		Natives:    setup.Natives,
		Servers:    setup.Servers,
		Scripts:    setup.Scripts,
		Dir:        setup.Dir,
		Home:       home,
		Skills:     places,
		Environ:    opts.Environ,
		Workspace:  setup.Workspace,
		SkillDirs:  setup.SkillDirs,
	})
	if err != nil {
		return Bootstrap{}, fmt.Errorf("cannot make the setup's key: %w", err)
	}
	b.Start = Start{
		Protocol:   Protocol,
		Keys:       []ProviderKey{{Provider: provider, Key: apiKey}},
		Setup:      setup,
		Runtime:    runtimeDir,
		Executable: b.Program,
	}
	return b, nil
}

// normalWorkspace checks a workspace target and writes it as the backend
// does: kind:name[:dir].
func normalWorkspace(value string) (string, error) {
	kind, rest, ok := strings.Cut(value, ":")
	if !ok || (kind != "ssh" && kind != "docker") {
		return "", errors.New("a workspace is ssh:HOST[:DIR], docker:CONTAINER[:DIR] or local")
	}
	name, dir, _ := strings.Cut(rest, ":")
	if name == "" {
		return "", errors.New("a workspace's host or container is not named")
	}
	if strings.ContainsAny(name, " \t'\"\\`$;&|<>()") || strings.IndexFunc(name, func(r rune) bool { return r <= ' ' }) >= 0 {
		return "", errors.New("a workspace's host or container holds a character no name has")
	}
	if strings.HasPrefix(name, "-") {
		return "", errors.New("a workspace's host or container cannot begin with '-'")
	}
	if dir != "" {
		if !(strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "~")) {
			return "", errors.New("a workspace's directory is absolute, or begins with ~")
		}
		if strings.ContainsAny(dir, "\x00\n'") {
			return "", errors.New("a workspace's directory holds a character it cannot hold")
		}
		return kind + ":" + name + ":" + dir, nil
	}
	return kind + ":" + name, nil
}
