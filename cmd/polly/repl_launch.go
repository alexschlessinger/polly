package main

import (
	"cmp"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Launches. Where a pane host exists, things that are processes of their own
// run in terminals of the host's instead of inside this polly's screen:
// another session is another polly in a tab, and the user's editor gets a
// pane if it wants one. Each starts with this process's environment and
// directory (see paneExec), so it behaves as if started here. Without a host,
// or when the host fails, the caller's own way of doing it takes over.

// launchSession opens a session as a polly of its own in a tab of the host's,
// in front: target's, or a new one when target names none. A session another polly
// under the host already has open is not opened again: that polly's tab
// comes to the front (see repl_pane_peers.go). here opens the session inside
// this process instead, under the visible model's lock: the caller runs it
// when launchSession reports false, and a host that fails runs it once and is
// not asked again.
func (r *managedREPL) launchSession(target sessions.ViewTarget, here func()) bool {
	h := &r.panes
	exe, err := os.Executable()
	if err != nil || h.opener == nil || h.tabless {
		return false
	}
	dir, _ := os.Getwd()
	x := paneExec{Argv: append([]string{exe}, relaunchArgs(os.Args[1:], target.Name)...), Dir: dir}
	for _, entry := range os.Environ() {
		// The session is chosen on the command line; the variable would
		// choose this process's own again.
		if !strings.HasPrefix(entry, "POLLYTOOL_CONTEXT=") {
			x.Env = append(x.Env, entry)
		}
	}
	// The tab carries the session's name until the polly in it has a title
	// to name it with (see titlePane).
	host := h.opener.words()
	open := func() bool {
		return r.launch(x, paneTab, cmp.Or(target.Name, paneTabLabel), func(err error) {
			if err == nil {
				return
			}
			r.model.mu.Lock()
			defer r.model.mu.Unlock()
			if paneBehind(err) {
				r.noteBehindLocked(cmp.Or(target.Name, "The new session")+" opened in a "+host.name+" "+host.tab+" behind this one", err)
				return
			}
			r.panes.tabless = true
			r.model.appendNoticeLine(host.name + " " + host.tab + " unavailable, opening the session here · " + err.Error())
			here()
		})
	}
	if target.Name == "" {
		return open()
	}
	return r.jumpToHolder(target, func(jumped bool) {
		if !jumped && !open() {
			r.model.mu.Lock()
			defer r.model.mu.Unlock()
			here()
		}
	}) || open()
}

// relaunchArgs is this process's command line for another session: the same
// options, with the ones that chose this session or ran once at startup
// dropped, and name selected when given.
func relaunchArgs(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if flag, ok := strings.CutPrefix(arg, "-"); ok && flag != "" {
			flag, _, attached := strings.Cut(strings.TrimPrefix(flag, "-"), "=")
			switch flag {
			case "c", "context":
				if !attached {
					i++ // The session's name is the next word.
				}
				continue
			case "L", "last", "setup":
				continue
			}
		}
		out = append(out, arg)
	}
	if name != "" {
		out = append(out, "--context", name)
	}
	return out
}

// openEditor opens path in the user's editor: $VISUAL, else $EDITOR, run in a
// pane beside polly if it turns out to be a terminal program and left to its
// own window otherwise (see rexPanes.openIfTerminal). Without an editor named,
// or without a host, the desktop's handler for the file opens it, as it
// always has.
func (r *managedREPL) openEditor(path string) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if editor == "" {
		return openExternal(path)
	}
	dir, _ := os.Getwd()
	// The shell splits the editor's own arguments from its name, as git's does.
	x := paneExec{Argv: []string{"/bin/sh", "-c", editor + ` "$@"`, "sh", path}, Env: os.Environ(), Dir: dir}
	if r.launch(x, paneIfTerminal, "editor", func(err error) {
		if err == nil {
			return
		}
		r.model.mu.Lock()
		r.model.appendNoticeLine("Could not run " + editor + " · " + err.Error())
		r.model.mu.Unlock()
		_ = openExternal(path)
	}) {
		return nil
	}
	return openExternal(path)
}
