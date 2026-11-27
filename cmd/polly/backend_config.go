package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/control"
	"github.com/urfave/cli/v3"
)

// The control backend: --backend URL runs every turn in a daemon that speaks
// the JSON-RPC control contract (control/), reached over its Unix socket,
// and reads every session through it. Without --backend polly runs its own
// engine in this process, as always.

const (
	envVarBackend   = "POLLYTOOL_BACKEND"
	envVarWorkspace = "POLLYTOOL_WORKSPACE"
)

// backendConfig is what --backend and its companions say.
type backendConfig struct {
	// URL is the backend URL; empty runs the in-process engine.
	URL string
	// Workspace is where the backend's tools work: local, ssh:HOST[:DIR]
	// or docker:CONTAINER[:DIR].
	Workspace string
	// Attach follows a running session's turn; Stop stops it. Each names
	// its session, or "" for the last one.
	Attach, Stop       bool
	AttachName, StopOf string
}

// enabled reports a run on a control backend.
func (b backendConfig) enabled() bool { return b.URL != "" }

func backendConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "backend",
			Usage:   "Run turns in a control daemon instead of this process: unix:///path/to/serve.sock, exec:///path/to/binary or exec://NAME (on PATH)",
			Sources: envDefault(envVarBackend),
		},
		&cli.StringFlag{
			Name:    "workspace",
			Usage:   "Where a --backend's tools work: local, ssh:HOST[:DIR] or docker:CONTAINER[:DIR]",
			Sources: envDefault(envVarWorkspace),
		},
		&cli.StringFlag{
			Name:  "attach",
			Usage: "With --backend: follow a session's running turn here (the last session by default); Ctrl-C detaches",
		},
		&cli.StringFlag{
			Name:  "stop",
			Usage: "With --backend: stop a session's running turn from here (the last session by default)",
		},
	}
}

func parseBackendConfig(cmd *cli.Command) backendConfig {
	return backendConfig{
		URL:        strings.TrimSpace(cmd.String("backend")),
		Workspace:  strings.TrimSpace(cmd.String("workspace")),
		Attach:     cmd.IsSet("attach"),
		AttachName: cmd.String("attach"),
		Stop:       cmd.IsSet("stop"),
		StopOf:     cmd.String("stop"),
	}
}

// normalizeOptionalValueFlags lets --attach and --stop stand alone: a flag
// followed by no value, or by another flag, names the last session.
func normalizeOptionalValueFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg != "--attach" && arg != "--stop" {
			out = append(out, arg)
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			out = append(out, arg+"="+args[i+1])
			i++
			continue
		}
		out = append(out, arg+"=")
	}
	return out
}

// backendManagementFlags are the context-management flags a control backend
// serves: they read its store. The rest write a store of polly's own.
var backendManagementFlags = []string{"list", "show", "export"}

// validateBackendConfig holds a launch to what the control backend serves,
// failing closed on everything that asks for in-process behavior: polly's
// sandbox, tool approvals, and the providers the backend does not have.
func validateBackendConfig(cmd *cli.Command, config *Config) error {
	b := config.Backend
	if !b.enabled() {
		var given []string
		if b.Workspace != "" && b.Workspace != "local" {
			given = append(given, "--workspace")
		}
		if b.Attach {
			given = append(given, "--attach")
		}
		if b.Stop {
			given = append(given, "--stop")
		}
		if len(given) > 0 {
			return fmt.Errorf("%s run through a control backend: pass --backend URL (or set %s)", strings.Join(given, ", "), envVarBackend)
		}
		return nil
	}
	if _, err := control.ParseTarget(b.URL); err != nil {
		return err
	}
	if b.Attach && b.Stop {
		return errors.New("--attach and --stop are one or the other")
	}
	if sandboxPolicyGiven(cmd, config) {
		return fmt.Errorf("--backend runs tools in the backend, which polly's sandbox does not reach: remove the sandbox policy (--sandbox, --writepath, --readpath, --denypath, --allownet, or %s and a saved sandbox setting), or run without --backend", envVarSandbox)
	}
	var refused []string
	if config.Confirm {
		refused = append(refused, "--confirm (the control contract has no tool approvals)")
	}
	if config.AgentSandbox != "" {
		refused = append(refused, "--agent-sandbox")
	}
	if len(config.AddDirs) > 0 {
		refused = append(refused, "--add-dir")
	}
	if config.Setup {
		refused = append(refused, "--setup")
	}
	if config.ShotFixture != "" {
		refused = append(refused, "--shot-fixture")
	}
	if len(config.Skills) > 0 {
		refused = append(refused, "--skill")
	}
	if config.Launch.ModelHost != "" && flagGiven(cmd, "modelhost") {
		refused = append(refused, "--modelhost")
	}
	if op := config.Management; op != nil && !slices.Contains(backendManagementFlags, op.name) {
		refused = append(refused, "--"+op.name)
	}
	if len(refused) > 0 {
		return fmt.Errorf("not supported with --backend: %s", strings.Join(refused, ", "))
	}
	if (b.Attach || b.Stop) && (config.PromptSet || config.Management != nil) {
		return errors.New("--attach and --stop take no prompt and no other context operation")
	}
	return validateBackendModel(config.Launch.Model)
}

// validateBackendModel refuses a model on a provider the backend does not
// serve.
func validateBackendModel(model string) error {
	provider, _, ok := strings.Cut(model, "/")
	if !ok || provider == "" {
		return fmt.Errorf("model must be provider/model, got %q", model)
	}
	if control.ProviderKeyVar(provider) == "" {
		return fmt.Errorf("provider %q is not served by the control backend; it serves %s", provider, strings.Join(control.SupportedProviders(), ", "))
	}
	return nil
}

// backendToolSelection splits --tool values as the backend reads them: MCP
// configurations (file.json or file.json#server) and script paths, which
// its setup loads, and patterns over tool names, which each prompt carries.
func backendToolSelection(values []string) (servers, scripts, allow []string) {
	for _, v := range values {
		file, _, _ := strings.Cut(v, "#")
		switch {
		case strings.HasSuffix(strings.ToLower(file), ".json"):
			servers = append(servers, v)
		case strings.Contains(v, "/"):
			scripts = append(scripts, v)
		default:
			allow = append(allow, v)
		}
	}
	return servers, scripts, allow
}

// remoteUnsupportedCommands are the REPL commands a session of a control
// backend does not serve, each with why: they need polly's own engine,
// sandbox or store, which the backend's session does not have.
var remoteUnsupportedCommands = map[string]string{
	"/add-dir":      "the backend's tools read its own files",
	"/clear":        "the backend's transcript is its own",
	"/reset":        "the backend's transcript is its own",
	"/compact":      "the backend compacts its conversations itself",
	"/rename":       "the backend names its sessions",
	"/title":        "the backend's record is its own",
	"/sandbox":      "polly's sandbox does not reach the backend's tools",
	"/sandbox-init": "polly's sandbox does not reach the backend's tools",
	"/spawn":        "agents are spawned by the backend's turns",
	"/plan":         "plans run on polly's own engine",
	"/todo":         "checklists run on polly's own engine",
	"/swarm":        "swarms run on polly's own engine",
	"/workflow":     "workflows run on polly's own engine",
	"/fast":         "service tiers are polly's own providers'",
	"/speed":        "service tiers are polly's own providers'",
	"/login":        "the backend holds its own provider keys",
	"/logout":       "the backend holds its own provider keys",
	"/setup":        "the backend's setup comes from this launch",
}
