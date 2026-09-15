package main

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/alexschlessinger/pollytool/internal/log"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools/docker"
	"github.com/alexschlessinger/pollytool/tools/docker/helper"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/urfave/cli/v3"
)

// sandboxCommand groups the container backend's management commands.
func sandboxCommand() *cli.Command {
	return &cli.Command{
		Name:  "sandbox",
		Usage: "Manage the container tool backend",
		Commands: []*cli.Command{
			sandboxHelperCommand(),
			sandboxPruneCommand(),
		},
	}
}

// sandboxPruneCommand removes containers whose polly session no longer
// exists. Startup never reaps on its own.
func sandboxPruneCommand() *cli.Command {
	return &cli.Command{
		Name:  "prune",
		Usage: "Remove containers whose polly session no longer exists",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "dry-run", Usage: "List what would be removed without removing"},
			&cli.BoolFlag{Name: "all", Usage: "Remove every container polly created, live sessions included"},
			&cli.StringFlag{Name: "host", Usage: "Docker daemon address (default: DOCKER_HOST, the active context, or the default socket)", Sources: cli.EnvVars("DOCKER_HOST")},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			keep, err := knownSessions(ctx)
			if err != nil {
				return err
			}
			pruned, err := docker.Prune(ctx, cmd.String("host"), keep, cmd.Bool("all"), cmd.Bool("dry-run"))
			verb := "removed"
			if cmd.Bool("dry-run") {
				verb = "would remove"
			}
			for _, entry := range pruned {
				fmt.Fprintf(cmd.Writer, "%s %s (session %s, root %s)\n", verb, entry.Name, entry.Session, entry.Root)
			}
			if len(pruned) == 0 && err == nil {
				fmt.Fprintln(cmd.Writer, "nothing to prune")
			}
			return err
		},
	}
}

// knownSessions reports the session identities the default store holds.
func knownSessions(ctx context.Context) (func(string) bool, error) {
	path, err := defaultStorePath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		// No store yet: no session exists.
		return func(string) bool { return false }, nil
	}
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		return nil, err
	}
	defer store.Close()
	summaries, err := store.ListSummaries(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(summaries))
	for _, summary := range summaries {
		known[summary.ID] = true
	}
	return func(session string) bool { return known[session] }, nil
}

// sandboxHelperCommand is the process polly runs inside a container: it
// enters helper mode, which is the only way to construct the container
// sandbox, and serves the helper protocol on stdin and stdout. stdout is
// the protocol, so logging goes to stderr.
func sandboxHelperCommand() *cli.Command {
	return &cli.Command{
		Name:   "helper",
		Hidden: true,
		Usage:  "internal: serve tools to the host from inside a container",
		Action: func(ctx context.Context, _ *cli.Command) error {
			sandbox.EnterHelperMode()
			log.InitLogger(os.Getenv("POLLYTOOL_DEBUG") != "")
			return helper.Serve(ctx, os.Stdin, os.Stdout, helper.Options{
				Version:      buildRevision(),
				Instructions: repositoryInstructionsFor,
			})
		},
	}
}

// buildRevision names this binary's build for the helper's welcome.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return info.Main.Version
}
