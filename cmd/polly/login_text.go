package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/codex"
)

// browserLogin is a browser sign-in in progress, as *codex.Login is.
type browserLogin interface {
	URL() string
	Callback() string
	Submit(pasted string) error
	Wait(context.Context) (llm.Account, error)
	Close()
}

// deviceLogin is a device sign-in in progress, as *codex.DeviceLogin is.
type deviceLogin interface {
	UserCode() string
	VerifyURL() string
	Wait(context.Context) (llm.Account, error)
}

// loginFlow is what a sign-in command does with a provider's store: start
// a sign-in either way, describe the current one, and forget it.
type loginFlow struct {
	browser func() (browserLogin, error)
	device  func(context.Context) (deviceLogin, error)
	account func() (llm.Account, bool)
	signOut func() error
}

// codexLoginFlow is the flow on the codex sign-in store.
func codexLoginFlow(store *codex.Store) loginFlow {
	return loginFlow{
		browser: func() (browserLogin, error) {
			l, err := store.StartLogin()
			if err != nil {
				return nil, err
			}
			return l, nil
		},
		device: func(ctx context.Context) (deviceLogin, error) {
			d, err := store.StartDeviceLogin(ctx)
			if err != nil {
				return nil, err
			}
			return d, nil
		},
		account: store.Account,
		signOut: store.Clear,
	}
}

// newLoginFlow opens the flow for a provider; tests substitute one.
var newLoginFlow = func(provider string) (loginFlow, error) {
	store, err := openAuthStore()
	if err != nil {
		return loginFlow{}, err
	}
	return codexLoginFlow(store), nil
}

// runTextLogin signs in from a plain terminal. A browser sign-in prints
// the page, opens it when it can, and then asks: Enter alone once the
// browser has come back on its own, or the address it landed on when it
// could not. A device sign-in prints the code and waits. say prints a
// line; readInput is nil where nothing can be asked, and the sign-in then
// waits for the callback alone.
func runTextLogin(ctx context.Context, flow loginFlow, device bool, readInput func(prompt string) (string, error), say func(string), open func(string) error) (llm.Account, error) {
	if device {
		d, err := flow.device(ctx)
		if err != nil {
			return llm.Account{}, err
		}
		say(fmt.Sprintf("Open %s and enter the code %s", d.VerifyURL(), d.UserCode()))
		say("Waiting for the code to be approved…")
		return d.Wait(ctx)
	}
	l, err := flow.browser()
	if err != nil {
		return llm.Account{}, err
	}
	defer l.Close()
	say("Sign in to ChatGPT at:")
	say("  " + l.URL())
	if open != nil && open(l.URL()) == nil {
		say("The browser is opening that page.")
	}
	if cb := l.Callback(); cb != "" {
		say(fmt.Sprintf("The browser comes back to %s when the sign-in is done.", cb))
	} else {
		say("No sign-in callback port is free, so the browser cannot come back here on its own.")
	}
	if readInput == nil {
		say("Waiting for the sign-in…")
		return l.Wait(ctx)
	}
	for {
		line, err := readInput("Press Enter once the browser has come back, or paste the address it landed on: ")
		if err != nil {
			return llm.Account{}, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if err := l.Submit(line); err != nil {
			say(err.Error())
			continue
		}
		break
	}
	say("Waiting for the sign-in…")
	return l.Wait(ctx)
}
