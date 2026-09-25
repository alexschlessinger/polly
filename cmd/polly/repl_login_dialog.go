package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/codex"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// The /login dialog runs a sign-in off the event loop and shows where it
// stands: the page to sign in on and the callback it waits for, or the
// code to type on the authority's page. A row takes the redirect pasted
// from the browser's address bar for when the browser cannot come back on
// its own. Escape ends the sign-in.

const (
	loginBrowserButton = "Open browser again"
	loginSwitchBrowser = "Use browser instead"
	loginDeviceButton  = "Use device code"
	loginCancelButton  = "Cancel"
)

// loginPasteRow is the focus value for the paste row; buttons count from 0.
const loginPasteRow = -1

type loginDialog struct {
	modal    *replModal
	provider string
	flow     loginFlow
	// device selects the device sign-in; browser and code are whichever is
	// in progress.
	device  bool
	browser browserLogin
	code    deviceLogin
	// running is set while a sign-in waits off the event loop; cancel ends
	// it. attempt counts the sign-ins started, so a superseded one's end
	// is ignored when it arrives.
	running bool
	cancel  context.CancelFunc
	attempt int
	// status is the line above the buttons: what the last action did, or
	// why the sign-in stopped.
	status, statusRole string
	// paste takes the redirect; focus is the paste row or a button.
	paste                              lineEditor
	focus                              int
	pasting                            bool
	url, callback, userCode, verifyURL string
	// Where the last frame drew the buttons, in the dialog's inner area,
	// for the mouse.
	buttonsY int
	buttonsX [][2]int
}

// openLogin opens the /login dialog and starts the sign-in.
func (r *managedREPL) openLogin(provider string, device bool) {
	flow, err := newLoginFlow(provider)
	if err != nil {
		r.model.appendNoticeLine("sign-in: " + err.Error())
		return
	}
	m := r.model
	d := &loginDialog{provider: provider, flow: flow, device: device, focus: loginPasteRow}
	d.modal = &replModal{
		title:    "Sign in to " + provider,
		width:    90,
		login:    d,
		onCancel: func() { m.appendNoticeLine("sign-in cancelled") },
	}
	r.openModal(d.modal)
	r.startLogin(d)
}

// startLogin begins the sign-in the dialog is set to and waits for it off
// the event loop, which commands and keys run on. Closing the dialog stops
// it.
func (r *managedREPL) startLogin(d *loginDialog) {
	m := r.model
	d.stop()
	parent := context.Background()
	if r.state != nil && r.state.session != nil {
		parent = r.state.session.Context()
	}
	ctx, cancel := context.WithCancel(parent)
	d.attempt++
	attempt := d.attempt
	d.running, d.cancel = true, cancel
	d.browser, d.code = nil, nil
	d.url, d.callback, d.userCode, d.verifyURL = "", "", "", ""
	d.paste.clear()
	if d.device {
		d.focus = 0
	} else {
		d.focus = loginPasteRow
	}
	d.setStatus("", "")
	var wait func(context.Context) (llm.Account, error)
	if d.device {
		wait = func(ctx context.Context) (llm.Account, error) {
			code, err := d.flow.device(ctx)
			if err != nil {
				return llm.Account{}, err
			}
			r.postUI(r.work.ctx, func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if m.modal == nil || m.modal.login != d || d.attempt != attempt {
					return
				}
				d.code, d.userCode, d.verifyURL = code, code.UserCode(), code.VerifyURL()
			})
			return code.Wait(ctx)
		}
	} else {
		l, err := d.flow.browser()
		if err != nil {
			cancel()
			r.finishLogin(m, d, llm.Account{}, err)
			return
		}
		d.browser, d.url, d.callback = l, l.URL(), l.Callback()
		if r.openURL != nil {
			if err := r.openURL(l.URL()); err != nil {
				d.setStatus("no browser opened ("+err.Error()+"); open the address yourself", "err")
			}
		}
		wait = func(ctx context.Context) (llm.Account, error) {
			acct, err := l.Wait(ctx)
			l.Close()
			return acct, err
		}
	}
	started := r.background(func() {
		stop := context.AfterFunc(r.work.ctx, cancel)
		defer stop()
		defer cancel()
		acct, err := wait(ctx)
		r.postUI(r.work.ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.modal == nil || m.modal.login != d || d.attempt != attempt {
				return
			}
			r.finishLogin(m, d, acct, err)
		})
	})
	if !started {
		cancel()
		r.finishLogin(m, d, llm.Account{}, errors.New("the workspace is closing"))
	}
}

// finishLogin ends the dialog on a sign-in, or shows why one stopped.
// Caller holds m.mu.
func (r *managedREPL) finishLogin(m *replModel, d *loginDialog, acct llm.Account, err error) {
	d.running, d.cancel = false, nil
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, codex.ErrLoginClosed) {
			return
		}
		d.setStatus("sign-in failed: "+err.Error(), "err")
		return
	}
	if m == r.model {
		r.closeModal()
	} else {
		d.modal.wipe()
		m.modal, m.pendingModal = m.pendingModal, nil
	}
	m.appendNoticeLine(signedInNotice(d.provider, acct))
}

// stop ends a running sign-in; closing the dialog calls it.
func (d *loginDialog) stop() {
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	if d.browser != nil {
		d.browser.Close()
	}
	d.running = false
}

func (d *loginDialog) setStatus(text, role string) {
	d.status, d.statusRole = text, role
}

// buttons are the dialog's buttons for the sign-in in progress.
func (d *loginDialog) buttons() []string {
	if d.device {
		return []string{loginSwitchBrowser, loginCancelButton}
	}
	return []string{loginBrowserButton, loginDeviceButton, loginCancelButton}
}

// handleLoginEvent is the dialog's input.
func (r *managedREPL) handleLoginEvent(d *loginDialog, e ui.Event) bool {
	switch e.ID {
	case pasteStartID:
		d.pasting = true
		return true
	case pasteEndID:
		d.pasting = false
		return true
	}
	if e.Type == ui.MouseEvent {
		r.handleLoginMouse(d, e)
		return true
	}
	if e.ID == "<Escape>" {
		r.dismissModal()
		return true
	}
	buttons := d.buttons()
	if d.focus >= len(buttons) {
		d.focus = len(buttons) - 1
	}
	first := 0
	if !d.device {
		first = loginPasteRow
	}
	switch e.ID {
	case "<Tab>":
		if d.focus == len(buttons)-1 {
			d.focus = first
		} else {
			d.focus++
		}
		return true
	case "<S-Tab>":
		if d.focus == first {
			d.focus = len(buttons) - 1
		} else {
			d.focus--
		}
		return true
	case "<Enter>":
		if d.focus == loginPasteRow {
			r.submitLoginPaste(d)
		} else {
			r.pressLoginButton(d, buttons[d.focus])
		}
		return true
	}
	if d.focus != loginPasteRow {
		switch e.ID {
		case "<Right>":
			d.focus = min(len(buttons)-1, d.focus+1)
		case "<Left>":
			d.focus = max(first, d.focus-1)
		}
		return true
	}
	if handleModalInputKey(&d.paste, e.ID) {
		return true
	}
	switch e.ID {
	case "<Backspace>", "<C-h>":
		d.paste.backspace()
	default:
		if ch, ok := printableRune(e); ok {
			d.paste.insert(ch)
		}
	}
	return true
}

func (r *managedREPL) handleLoginMouse(d *loginDialog, e ui.Event) {
	mouse, ok := e.Payload.(ui.Mouse)
	if !ok || e.ID != "<MouseLeft>" {
		return
	}
	point := image.Pt(mouse.X, mouse.Y)
	if !d.modal.bounds.Empty() && !point.In(d.modal.bounds) {
		r.dismissModal()
		return
	}
	local := point.Sub(d.modal.bounds.Min.Add(image.Pt(1, 1)))
	if local.Y != d.buttonsY {
		return
	}
	for i, span := range d.buttonsX {
		if local.X >= span[0] && local.X < span[1] {
			buttons := d.buttons()
			if i < len(buttons) {
				d.focus = i
				r.pressLoginButton(d, buttons[i])
			}
			return
		}
	}
}

// submitLoginPaste hands the pasted redirect to the sign-in.
func (r *managedREPL) submitLoginPaste(d *loginDialog) {
	text := strings.TrimSpace(d.paste.text())
	if text == "" {
		return
	}
	if d.browser == nil || !d.running {
		d.setStatus("no browser sign-in is waiting", "err")
		return
	}
	if err := d.browser.Submit(text); err != nil {
		d.setStatus(err.Error(), "err")
		return
	}
	d.paste.clear()
	d.setStatus("redirect received; finishing the sign-in…", "muted")
}

// pressLoginButton runs one of the dialog's buttons.
func (r *managedREPL) pressLoginButton(d *loginDialog, label string) {
	switch label {
	case loginBrowserButton:
		if d.url == "" || r.openURL == nil {
			return
		}
		if err := r.openURL(d.url); err != nil {
			d.setStatus("no browser opened ("+err.Error()+"); open the address yourself", "err")
		} else {
			d.setStatus("the browser is opening the page again", "muted")
		}
	case loginSwitchBrowser:
		d.device = false
		r.startLogin(d)
	case loginDeviceButton:
		d.device = true
		r.startLogin(d)
	case loginCancelButton:
		r.dismissModal()
	}
}

// text renders the dialog for a frame of maxRows rows at modalWidth.
func (d *loginDialog) text(maxRows, modalWidth int) string {
	inner := max(20, modalWidth-2)
	var lines []string
	add := func(text, role, mod string) {
		for _, row := range wrapModalText(text, inner) {
			lines = append(lines, style.Styled(row, role, mod))
		}
	}
	switch {
	case d.device && d.userCode == "":
		add("Asking ChatGPT for a device code…", "muted", "")
	case d.device:
		add("On any device, open", "", "")
		add("  "+d.verifyURL, "accent", "")
		add("and enter the code", "", "")
		add("  "+d.userCode, "accent", "bold")
		lines = append(lines, "")
		add("Waiting for the code to be approved…", "muted", "")
	default:
		add("Sign in to ChatGPT in your browser:", "", "")
		add("  "+d.url, "accent", "")
		lines = append(lines, "")
		if d.callback != "" {
			add("Waiting for the browser to come back to "+d.callback+"…", "muted", "")
		} else {
			add("No sign-in callback port is free, so the browser cannot come back here on its own.", "muted", "")
		}
		add("If it lands on a page that cannot load, paste that page's address here:", "muted", "")
		prefix := "  "
		if d.focus == loginPasteRow {
			prefix = "› "
		}
		value := rw.Truncate(d.paste.text(), max(1, inner-10), "…")
		if d.focus == loginPasteRow {
			value = formEditorText(&d.paste, false, max(1, inner-10))
		}
		row := prefix + fmt.Sprintf("%-7s", "Paste") + value
		if d.focus == loginPasteRow {
			lines = append(lines, style.Styled(row, "accent", "bold"))
		} else {
			lines = append(lines, style.Escape(row))
		}
	}
	lines = append(lines, "")
	if d.status != "" {
		add(d.status, d.statusRole, "")
		lines = append(lines, "")
	}
	d.buttonsY = len(lines)
	lines = append(lines, d.buttonRow())
	help := "Tab next · Enter press · Esc cancel"
	if !d.device {
		help = "Enter submit · Tab next · Esc cancel"
	}
	lines = append(lines, "", centeredModalHelper(help, modalWidth))
	return strings.Join(lines, "\n")
}

// buttonRow draws the buttons and records where for the mouse.
func (d *loginDialog) buttonRow() string {
	var buttons strings.Builder
	d.buttonsX = d.buttonsX[:0]
	x := 0
	for i, label := range d.buttons() {
		marker, role, mod := "  ", "", ""
		if i == d.focus {
			marker, role, mod = "› ", "accent", "bold"
		}
		button := "[ " + label + " ]"
		buttons.WriteString(style.Styled(marker, "accent", "bold") + style.Styled(button, role, mod))
		x += 2
		d.buttonsX = append(d.buttonsX, [2]int{x, x + rw.StringWidth(button)})
		x += rw.StringWidth(button)
	}
	return buttons.String()
}
