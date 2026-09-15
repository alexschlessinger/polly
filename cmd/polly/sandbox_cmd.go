package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	dockerfiles "github.com/alexschlessinger/pollytool/docker"
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
			sandboxBuildCommand(),
		},
	}
}

// sandboxBuildCommand writes a reference Dockerfile out and builds it with
// the docker CLI. It is the one place polly runs docker itself: an explicit
// management command, never a conversation's tool path, because BuildKit is
// not reachable through the daemon's legacy build endpoint.
func sandboxBuildCommand() *cli.Command {
	return &cli.Command{
		Name:      "build",
		Usage:     "Build a reference image variant (base, go, node, python) with the docker CLI",
		ArgsUsage: "<variant>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "ref", Usage: "Branch, tag or commit of the polly repository to build (POLLY_REF)", Value: "main"},
			&cli.StringFlag{Name: "repo", Usage: "Repository the image builds polly from (POLLY_REPO)", Value: "https://github.com/alexschlessinger/polly.git"},
			&cli.StringFlag{Name: "dir", Usage: "Directory the Dockerfiles are written to", Value: defaultBuildDir()},
			&cli.BoolFlag{Name: "print", Usage: "Write the Dockerfiles and print the docker commands without running them"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			variant := cmd.Args().First()
			if variant == "" {
				return fmt.Errorf("name a variant: %s", strings.Join(dockerfiles.Variants(), ", "))
			}
			if _, err := dockerfiles.Dockerfile(variant); err != nil {
				return err
			}
			order := []string{variant}
			if parent := dockerfiles.Parent(variant); parent != "" {
				order = append([]string{parent}, order...)
			}
			for _, name := range order {
				dir := filepath.Join(cmd.String("dir"), name)
				content, _ := dockerfiles.Dockerfile(name)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), content, 0o644); err != nil {
					return err
				}
				args := []string{"build", "--tag", dockerfiles.Tag(name), "--build-arg", "POLLY_REF=" + cmd.String("ref"), "--build-arg", "POLLY_REPO=" + cmd.String("repo"), dir}
				if cmd.Bool("print") {
					fmt.Fprintf(cmd.Writer, "docker %s\n", strings.Join(args, " "))
					continue
				}
				build := exec.CommandContext(ctx, "docker", args...)
				build.Stdout, build.Stderr = cmd.Writer, cmd.ErrWriter
				if err := build.Run(); err != nil {
					return fmt.Errorf("docker build %s: %w", name, err)
				}
			}
			return nil
		},
	}
}

func defaultBuildDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "polly-docker-build")
	}
	return filepath.Join(home, ".pollytool", "docker", "build")
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
