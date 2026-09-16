package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/alexschlessinger/pollytool/internal/log"
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
		},
	}
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
