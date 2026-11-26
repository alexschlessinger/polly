package main

import (
	"cmp"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Peers. A session is open in one polly at a time, and under a pane host
// each polly sits in a tab of its own. So before a session is opened, the
// other pollys are asked whether one of them has it: the one that does
// answers with the terminal it runs in, and that tab comes to the front
// instead of a second, read-only copy opening beside it.
//
// The question goes to the sockets the pollys already listen on for their
// panes (see repl_panes.go), which only this user can reach. A polly outside
// a pane host has no socket and is never found; its sessions open read-only
// as before.

// paneAskTimeout bounds how long one polly is given to answer.
const paneAskTimeout = time.Second

// peerHostScope identifies the multiplexer server/session a block ID belongs
// to so cross-host peer probes do not try to focus or activate an unreachable
// polly.
func peerHostScope(block string) string {
	switch {
	case block == "":
		return ""
	case strings.HasPrefix(block, tmuxBlockPrefix):
		named := strings.TrimPrefix(block, tmuxBlockPrefix)
		socket, _, _ := cutLast(named, ":")
		return tmuxBlockPrefix + socket
	case strings.HasPrefix(block, zellijBlockPrefix):
		named := strings.TrimPrefix(block, zellijBlockPrefix)
		session, _, _ := cutLast(named, ":")
		return zellijBlockPrefix + session
	default:
		return "rex"
	}
}

type peerMatch struct {
	path  string
	block string
}

// askPanes asks the pollys listening under root, other than the one at own,
// which of them has target open from the session database at store on a host
// reachable from ownBlock. The probe is strictly read-only.
func askPanes(ctx context.Context, root, own, ownBlock, store string, target sessions.ViewTarget) peerMatch {
	paths, _ := filepath.Glob(filepath.Join(root, "pane-*.sock"))
	question := []byte(store + "\x00" + target.ID + "\x00" + target.Name)
	scope := peerHostScope(ownBlock)
	answers := make(chan peerMatch, len(paths))
	asked := 0
	for _, path := range paths {
		if path == own {
			continue
		}
		asked++
		go func(p string) {
			answers <- peerMatch{path: p, block: askPane(ctx, p, question)}
		}(path)
	}
	for ; asked > 0; asked-- {
		if match := <-answers; match.block != "" {
			if scope != "" && peerHostScope(match.block) != scope {
				continue
			}
			return match
		}
	}
	return peerMatch{}
}

func askPane(ctx context.Context, path string, question []byte) string {
	ctx, cancel := context.WithTimeout(ctx, paneAskTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if writePaneFrame(conn, paneFrameWho, question) != nil {
		return ""
	}
	// A polly that does not have the session, or is too old to know the
	// question, hangs up without a word.
	answer, _ := io.ReadAll(io.LimitReader(conn, 256))
	return string(answer)
}

func activatePeer(ctx context.Context, path, store string, target sessions.ViewTarget) {
	ctx, cancel := context.WithTimeout(ctx, paneAskTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	payload := []byte(store + "\x00" + target.ID + "\x00" + target.Name)
	_ = writePaneFrame(conn, paneFrameActivate, payload)
	_, _ = io.ReadAll(io.LimitReader(conn, 16))
}

// jumpToHolder asks the other pollys who has target open and brings that
// one's terminal to the front. The answer lands on the event loop, with no
// lock held: done hears whether a holder was found and focused, or found and
// the user told which tab to look in. False means there is no host to ask,
// and done never runs.
func (r *managedREPL) jumpToHolder(target sessions.ViewTarget, done func(jumped bool)) bool {
	h := &r.panes
	root, err := scratch.EnsureRoot()
	if h.opener == nil || err != nil {
		return false
	}
	opener, own, ownBlock, store := h.opener, h.socket, h.block, h.store
	host := opener.words()
	return r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		match := askPanes(ctx, root, own, ownBlock, store, target)
		var err error
		if match.block != "" {
			err = opener.focus(ctx, match.block)
			if err == nil || paneBehind(err) {
				activatePeer(ctx, match.path, store, target)
			}
		}
		r.postUI(r.work.ctx, func() {
			if match.block != "" && paneBehind(err) {
				// It is found all the same: opening it again would only fail.
				r.model.mu.Lock()
				r.noteBehindLocked(cmp.Or(target.Name, "That session")+" is open in another "+host.name+" "+host.tab, err)
				r.model.mu.Unlock()
				err = nil
			}
			done(match.block != "" && err == nil)
		})
	})
}

// noteBehindLocked tells the user about a terminal the host has but left
// behind the one on screen, and the first time, why: the reason is the
// host's, and usually a setting. Caller holds the visible model's lock.
func (r *managedREPL) noteBehindLocked(what string, err error) {
	if !r.panes.behindTold {
		r.panes.behindTold = true
		what += " · " + err.Error()
	}
	r.model.appendNoticeLine(what)
}

// answerWho answers another polly's read-only question on the listener's
// goroutine; the tabs are the event loop's to read.
func (r *managedREPL) answerWho(conn net.Conn, question []byte) {
	defer conn.Close()
	fields := strings.Split(string(question), "\x00")
	if len(fields) != 3 {
		return
	}
	answer := make(chan string, 1)
	if !r.postUI(r.work.ctx, func() { answer <- r.holderBlock(fields[0], fields[1], fields[2]) }) {
		return
	}
	select {
	case block := <-answer:
		_ = conn.SetWriteDeadline(time.Now().Add(paneWriteTimeout))
		_, _ = conn.Write([]byte(block))
	case <-r.work.ctx.Done():
	}
}

// handleActivatePeer shows the workspace holding the requested session after
// a compatible peer has confirmed it is focusing this terminal.
func (r *managedREPL) handleActivatePeer(conn net.Conn, payload []byte) {
	defer conn.Close()
	fields := strings.Split(string(payload), "\x00")
	if len(fields) != 3 {
		return
	}
	done := make(chan struct{}, 1)
	if !r.postUI(r.work.ctx, func() {
		r.activateHolderTab(fields[0], fields[1], fields[2])
		done <- struct{}{}
	}) {
		return
	}
	select {
	case <-done:
	case <-r.work.ctx.Done():
	}
}

// holderTab finds the tab holding the requested session without mutating
// visible tab state.
func (r *managedREPL) holderTab(store, id, name string) *replTab {
	h := &r.panes
	if h.block == "" || store != h.store {
		return nil
	}
	for _, tab := range r.tabs {
		if tab.state == nil || tab.state.session == nil || tab.state.session.Context().Err() != nil {
			continue
		}
		if id != "" && tab.viewID() != id || id == "" && tab.name != name {
			continue
		}
		return tab
	}
	return nil
}

// holderBlock reports the terminal this polly runs in if it holds the
// session asked about, by identity when the question carries one and by name
// otherwise. It is strictly read-only.
func (r *managedREPL) holderBlock(store, id, name string) string {
	if r.holderTab(store, id, name) == nil {
		return ""
	}
	return r.panes.block
}

// activateHolderTab switches this polly's visible tab to the workspace holding
// the requested session.
func (r *managedREPL) activateHolderTab(store, id, name string) {
	tab := r.holderTab(store, id, name)
	if tab == nil {
		return
	}
	if root := r.rootTab(tab); root != nil {
		r.model.mu.Lock()
		r.requestShowTabLocked(r.tabIndexOfModel(root.model))
		r.model.mu.Unlock()
	}
}
