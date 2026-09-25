package main

import (
	"bufio"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

// The sign-in flags run instead of a conversation, like the context
// management flags: --login signs in to a provider served on a
// subscription and --logout forgets the sign-in. --device makes --login
// hand out a code to type on a web page instead of opening a browser
// here, for a shell with no browser beside it.
func newLoginFlag() *cli.StringFlag {
	return newPromptAndFileFreeStringFlag("login", "Sign in to a provider on a subscription instead of an API key (codex: a ChatGPT plan), then exit")
}

func newLogoutFlag() *cli.StringFlag {
	return newPromptAndFileFreeStringFlag("logout", "Forget a provider's sign-in, then exit")
}

func loginConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "device", Usage: "With --login, sign in by entering a code on a web page instead of opening a browser here"},
	}
}

// handleLoginFlag is --login: the text sign-in on this terminal.
func handleLoginFlag(r *commandRunner, name string) error {
	provider, err := loginProvider(name)
	if err != nil {
		return err
	}
	flow, err := newLoginFlow(provider)
	if err != nil {
		return err
	}
	var readInput func(string) (string, error)
	if canPromptOnStdin() {
		reader := bufio.NewReader(os.Stdin)
		readInput = func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			return readLine(reader)
		}
	}
	say := func(line string) { fmt.Fprintln(os.Stderr, line) }
	acct, err := runTextLogin(r.ctx, flow, r.cmd.Bool("device"), readInput, say, browserOpener)
	if err != nil {
		return err
	}
	say(signedInNotice(provider, acct))
	return nil
}

// handleLogoutFlag is --logout.
func handleLogoutFlag(r *commandRunner, name string) error {
	provider, err := loginProvider(name)
	if err != nil {
		return err
	}
	flow, err := newLoginFlow(provider)
	if err != nil {
		return err
	}
	if _, ok := flow.account(); !ok {
		fmt.Fprintf(os.Stderr, "not signed in to %s\n", provider)
		return nil
	}
	if err := flow.signOut(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "signed out of %s\n", provider)
	return nil
}
