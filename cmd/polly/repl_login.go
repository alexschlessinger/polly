package main

import (
	"errors"
	"strings"
)

// loginUsage is how /login is spelled.
const loginUsage = "/login [provider] [--device]"

func registerLoginCommands(r *replCommandRegistry) {
	r.register(replCommand{
		name:    "/login",
		usage:   loginUsage,
		summary: "sign in with a subscription (codex: a ChatGPT plan)",
		run:     replLoginCommand,
	})
	r.register(replCommand{
		name:    "/logout",
		usage:   "/logout [provider]",
		summary: "forget a provider's sign-in",
		run:     replLogoutCommand,
	})
}

// parseLoginArgs reads the provider and the device switch off a /login
// line, after the command itself.
func parseLoginArgs(args []string) (provider string, device bool, err error) {
	name := ""
	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "--device", "device":
			device = true
		default:
			if name != "" {
				return "", false, errors.New("usage: " + loginUsage)
			}
			name = arg
		}
	}
	provider, err = loginProvider(name)
	return provider, device, err
}

// replLoginCommand runs /login: the dialog in the managed TUI, the text
// sign-in elsewhere.
func replLoginCommand(ctx *replCommandContext, args []string) replCommandResult {
	provider, device, err := parseLoginArgs(args[1:])
	if err != nil {
		return replCommandResult{err: ctx.replyLine(err.Error())}
	}
	if ctx.openLogin != nil {
		ctx.openLogin(provider, device)
		return replCommandResult{}
	}
	flow, err := newLoginFlow(provider)
	if err != nil {
		return replCommandResult{err: ctx.replyLine("sign-in: " + err.Error())}
	}
	acct, err := runTextLogin(ctx.operationContext(), flow, device, ctx.readInput, func(line string) { _ = ctx.replyLine(line) }, browserOpener)
	if err != nil {
		return replCommandResult{err: ctx.replyLine("sign-in failed: " + err.Error())}
	}
	return replCommandResult{err: ctx.replyLine(signedInNotice(provider, acct))}
}

// replLogoutCommand runs /logout.
func replLogoutCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) > 2 {
		return replCommandResult{err: ctx.replyLine("usage: /logout [provider]")}
	}
	name := ""
	if len(args) == 2 {
		name = args[1]
	}
	provider, err := loginProvider(name)
	if err != nil {
		return replCommandResult{err: ctx.replyLine(err.Error())}
	}
	flow, err := newLoginFlow(provider)
	if err != nil {
		return replCommandResult{err: ctx.replyLine("sign-out: " + err.Error())}
	}
	if _, ok := flow.account(); !ok {
		return replCommandResult{err: ctx.replyLine("not signed in to " + provider)}
	}
	if err := flow.signOut(); err != nil {
		return replCommandResult{err: ctx.replyLine("sign-out failed: " + err.Error())}
	}
	return replCommandResult{err: ctx.replyLine("signed out of " + provider)}
}
