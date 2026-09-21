package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// A headless shot run paints the managed TUI on an off-screen terminal emulator
// while a script supplies what a keyboard would, so a frame can be captured
// with no terminal, no pty host and no window. The script is played by its own
// goroutine: its keys reach the event loop as the events a terminal would
// deliver, and every step that reads or writes the screen runs on the loop as
// a UI task, since the loop is the only goroutine allowed to touch the screen.
// The loop paints after each of those, so a :shot step captures the frame the
// step before it produced — the same "next frame" rule the interactive
// /screenshot uses.
//
// The script text lives in one place: docs/CLI.md documents it as the
// `polly --shot-script` language.
const (
	// headlessWaitDefault bounds a :wait or :settle no seconds were given for.
	headlessWaitDefault = 10 * time.Second
	// headlessReadyDefault bounds the wait for a TUI that can take input, which
	// is the startup workspace baseline rather than anything the script does.
	headlessReadyDefault = 30 * time.Second
	// headlessPollInterval is how often a waiting step looks at the screen.
	headlessPollInterval = 100 * time.Millisecond
)

// errHeadlessStop ends the script early without failing it: the script asked to
// quit, or the run itself ended under the player.
var errHeadlessStop = errors.New("shot run ended")

// headlessStep is one script line, parsed: its directive (a bare line is a
// submit) and the argument that directive takes.
type headlessStep struct {
	line int
	kind string
	// arg is the text a submit or type sends, the name of a key, the path of a
	// shot, or the pattern of a wait.
	arg string
	// width and height are the terminal a size step switches to.
	width, height int
	// duration bounds a wait, settle or ready step, and is a sleep's length.
	duration time.Duration
}

// headlessRun is a parsed script plus what playing it produced.
type headlessRun struct {
	steps  []headlessStep
	width  int
	height int
	// screen is the off-screen screen the run paints on, once installed.
	screen *headlessscreen.Screen
	// keys carries scripted key events to the event loop, in place of the
	// terminal's event queue.
	keys chan ui.Event
	// typed reports that input has reached the TUI once already, so the startup
	// readiness wait happens before the first input step only.
	typed bool

	mu  sync.Mutex
	err error
}

// parseHeadlessSize reads a WxH terminal size.
func parseHeadlessSize(value string) (width, height int, err error) {
	value = strings.TrimSpace(strings.ToLower(value))
	widthText, heightText, ok := strings.Cut(value, "x")
	if !ok {
		return 0, 0, fmt.Errorf("invalid size %q: use WxH, for example 120x40", value)
	}
	if width, err = strconv.Atoi(strings.TrimSpace(widthText)); err != nil {
		return 0, 0, fmt.Errorf("invalid size %q: width is not a number", value)
	}
	if height, err = strconv.Atoi(strings.TrimSpace(heightText)); err != nil {
		return 0, 0, fmt.Errorf("invalid size %q: height is not a number", value)
	}
	if width < 20 || width > 1000 || height < 5 || height > 400 {
		return 0, 0, fmt.Errorf("size %dx%d is out of range (20-1000 columns, 5-400 rows)", width, height)
	}
	return width, height, nil
}

// loadHeadlessRun reads a shot script from path ("-" is stdin) and parses it
// for a width x height terminal. Every parse error is reported before the run
// starts, so a mistyped script captures nothing rather than half a session.
func loadHeadlessRun(path string, width, height int) (*headlessRun, error) {
	var data []byte
	var err error
	if path == "-" {
		if data, err = io.ReadAll(os.Stdin); err != nil {
			return nil, fmt.Errorf("read shot script from stdin: %w", err)
		}
	} else if data, err = os.ReadFile(path); err != nil {
		return nil, fmt.Errorf("read shot script: %w", err)
	}

	run := &headlessRun{width: width, height: height, keys: make(chan ui.Event)}
	for i, raw := range strings.Split(string(data), "\n") {
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		step, err := parseHeadlessStep(i+1, text)
		if err != nil {
			return nil, err
		}
		run.steps = append(run.steps, step)
	}
	if len(run.steps) == 0 {
		return nil, errors.New("shot script has no steps")
	}
	return run, nil
}

// parseHeadlessStep parses one script line. A bare line is composer input that
// is typed and submitted; a leading ':' marks a directive, and '::' escapes a
// literal line that starts with one.
func parseHeadlessStep(line int, text string) (headlessStep, error) {
	step := headlessStep{line: line, kind: "submit", arg: text}
	if !strings.HasPrefix(text, ":") {
		return step, nil
	}
	if strings.HasPrefix(text, "::") {
		step.arg = text[1:]
		return step, nil
	}
	name, rest, _ := strings.Cut(text[1:], " ")
	step.kind, step.arg = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(rest)
	fail := func(format string, args ...any) (headlessStep, error) {
		return headlessStep{}, fmt.Errorf("shot script line %d: %s", line, fmt.Sprintf(format, args...))
	}

	var err error
	switch step.kind {
	case "submit", "type":
		if step.arg == "" {
			return fail(":%s takes the text to send", step.kind)
		}
	case "key":
		step.arg = strings.ToLower(step.arg)
		if _, ok := headlessKeys[step.arg]; !ok {
			return fail("unknown key %q: use enter, esc, tab, up, down, left, right, pgup, pgdn, home, end, insert, delete, backspace, space, or c-a to c-z", rest)
		}
	case "shot":
		step.arg = strings.Trim(step.arg, `"`)
		if step.arg == "" {
			return fail(":shot takes the PNG path to write")
		}
	case "size":
		if step.width, step.height, err = parseHeadlessSize(step.arg); err != nil {
			return fail("%v", err)
		}
	case "wait":
		if step.arg, step.duration, err = splitHeadlessWait(step.arg); err != nil {
			return fail("%v", err)
		}
	case "settle", "ready":
		fallback := headlessWaitDefault
		if step.kind == "ready" {
			fallback = headlessReadyDefault
		}
		if step.duration, err = headlessSeconds(step.arg, fallback); err != nil {
			return fail(":%s takes optional seconds: %v", step.kind, err)
		}
	case "sleep":
		millis, err := strconv.Atoi(step.arg)
		if err != nil || millis < 0 {
			return fail(":sleep takes milliseconds, for example :sleep 250")
		}
		step.duration = time.Duration(millis) * time.Millisecond
	case "quit":
		if step.arg != "" {
			return fail(":quit takes no argument")
		}
	default:
		return fail("unknown directive %q: use :key, :type, :submit, :shot, :size, :wait, :settle, :ready, :sleep, or :quit", step.kind)
	}
	return step, nil
}

// splitHeadlessWait splits a :wait argument into its pattern and its optional
// timeout. A quoted pattern keeps its spaces and may still be followed by
// seconds; otherwise a trailing number is the timeout, the rule a pattern with
// a number of its own avoids by quoting it: :wait "shot #2".
func splitHeadlessWait(rest string) (string, time.Duration, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", 0, errors.New(":wait takes a pattern to wait for")
	}
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return "", 0, fmt.Errorf("unterminated quote in %s", rest)
		}
		timeout, err := headlessSeconds(rest[end+2:], headlessWaitDefault)
		if err != nil {
			return "", 0, err
		}
		return rest[1 : 1+end], timeout, nil
	}
	if idx := strings.LastIndex(rest, " "); idx >= 0 {
		if timeout, err := headlessSeconds(rest[idx+1:], headlessWaitDefault); err == nil {
			return strings.TrimSpace(rest[:idx]), timeout, nil
		}
	}
	return rest, headlessWaitDefault, nil
}

// headlessSeconds parses an optional seconds argument, empty meaning the
// default.
func headlessSeconds(value string, fallback time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%q is not a positive number of seconds", value)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// headlessKeys maps a script key name to the tcell key a terminal reports for
// it; the loop's own converter then gives the event the same ID a typed key
// carries. Space is the one printable key with a name, kept as a rune.
var headlessKeys = func() map[string]tcell.Key {
	keys := map[string]tcell.Key{
		"enter": tcell.KeyEnter, "esc": tcell.KeyEsc, "escape": tcell.KeyEsc, "tab": tcell.KeyTab,
		"up": tcell.KeyUp, "down": tcell.KeyDown, "left": tcell.KeyLeft, "right": tcell.KeyRight,
		"pgup": tcell.KeyPgUp, "pgdn": tcell.KeyPgDn, "home": tcell.KeyHome, "end": tcell.KeyEnd,
		"insert": tcell.KeyInsert, "delete": tcell.KeyDelete, "backspace": tcell.KeyBackspace,
		"space": tcell.KeyRune,
	}
	for c := 'a'; c <= 'z'; c++ {
		keys["c-"+string(c)] = tcell.KeyCtrlA + tcell.Key(c-'a')
	}
	// A terminal sends these control characters as the keys they are.
	keys["c-h"], keys["c-i"], keys["c-m"] = tcell.KeyBackspace, tcell.KeyTab, tcell.KeyEnter
	return keys
}()

// headlessKeyEvent builds the event the loop would receive for a scripted key.
func headlessKeyEvent(name string) (ui.Event, bool) {
	code, ok := headlessKeys[name]
	if !ok {
		return ui.Event{}, false
	}
	str := ""
	if code == tcell.KeyRune {
		str = " "
	}
	return convertTcellKey(tcell.NewEventKey(code, str, tcell.ModNone)), true
}

// installScreen points the run at an off-screen screen of the scripted size
// instead of the terminal's own. The run wraps it in themedScreen like any
// other, so the frame is painted exactly as it would be on a terminal.
func (h *headlessRun) installScreen() error {
	sim, err := headlessscreen.New(h.width, h.height)
	if err != nil {
		return fmt.Errorf("start off-screen screen: %w", err)
	}
	h.screen = sim
	ui.DefaultBackend.Screen = sim
	return nil
}

// failure reports the first step that failed, once the run has ended.
func (h *headlessRun) failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// play drives the script and then asks the run to end. It runs on its own
// goroutine so that waiting for a pattern, a settled frame, or a timeout never
// blocks the loop that paints.
func (h *headlessRun) play(ctx context.Context, r *managedREPL) {
	var err error
	for _, step := range h.steps {
		if err = h.one(ctx, r, step); err != nil {
			if errors.Is(err, errHeadlessStop) {
				err = nil
			} else {
				err = fmt.Errorf("shot script line %d: %w", step.line, err)
			}
			break
		}
	}
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	r.requestQuit()
}

func (h *headlessRun) one(ctx context.Context, r *managedREPL, step headlessStep) error {
	switch step.kind {
	case "submit":
		if err := h.typeText(ctx, r, step.arg); err != nil {
			return err
		}
		return h.press(ctx, r, "enter")
	case "type":
		return h.typeText(ctx, r, step.arg)
	case "key":
		return h.press(ctx, r, step.arg)
	case "ready":
		return h.waitReady(ctx, r, step.duration)
	case "shot":
		return h.shot(ctx, r, step.arg)
	case "size":
		return h.onLoop(ctx, r, func() { h.screen.SetSize(step.width, step.height) })
	case "wait":
		return h.waitFor(ctx, r, step.arg, step.duration)
	case "settle":
		return h.settle(ctx, r, step.duration)
	case "sleep":
		return sleepHeadless(ctx, step.duration)
	case "quit":
		return errHeadlessStop
	}
	return fmt.Errorf("unknown step %q", step.kind)
}

// send hands one key event to the loop as the terminal would. The channel is
// unbuffered, so the loop has handled the key — and started whatever turn it
// queued — before the next step begins.
func (h *headlessRun) send(ctx context.Context, r *managedREPL, ev ui.Event) error {
	select {
	case h.keys <- ev:
		return nil
	case <-r.work.ctx.Done():
		return errHeadlessStop
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// onLoop runs fn on the event loop, where every screen read and write belongs,
// and returns once it has run. The loop paints a frame after it.
func (h *headlessRun) onLoop(ctx context.Context, r *managedREPL, fn func()) error {
	done := make(chan struct{})
	if !r.postUI(ctx, func() { fn(); close(done) }) {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return errHeadlessStop
	}
	select {
	case <-done:
		return nil
	case <-r.work.ctx.Done():
		return errHeadlessStop
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// press delivers one named key.
func (h *headlessRun) press(ctx context.Context, r *managedREPL, name string) error {
	ev, ok := headlessKeyEvent(name)
	if !ok {
		return fmt.Errorf("unknown key %q", name)
	}
	return h.send(ctx, r, ev)
}

// typeText delivers text one rune at a time, the way a terminal does. The
// first text of a run waits for a TUI that can take input: the startup
// workspace baseline runs behind the first frame, and input that arrives
// before it drains is queued rather than run — honest for a fast typist, but
// not what a script asking for a command's output means.
func (h *headlessRun) typeText(ctx context.Context, r *managedREPL, text string) error {
	if !h.typed {
		h.typed = true
		if err := h.waitReady(ctx, r, headlessReadyDefault); err != nil {
			return err
		}
	}
	for _, char := range text {
		ev := convertTcellKey(tcell.NewEventKey(tcell.KeyRune, string(char), tcell.ModNone))
		if err := h.send(ctx, r, ev); err != nil {
			return err
		}
	}
	return nil
}

// waitReady blocks until the composer would submit rather than queue.
func (h *headlessRun) waitReady(ctx context.Context, r *managedREPL, timeout time.Duration) error {
	ok, err := h.poll(ctx, timeout, func() (bool, error) {
		var ready bool
		err := h.onLoop(ctx, r, func() { ready = r.acceptsInput() })
		return ready, err
	})
	if err == nil && !ok {
		return fmt.Errorf("the TUI was not ready for input within %s", timeout)
	}
	return err
}

// waitFor blocks until the painted screen contains pattern.
func (h *headlessRun) waitFor(ctx context.Context, r *managedREPL, pattern string, timeout time.Duration) error {
	ok, err := h.poll(ctx, timeout, func() (bool, error) {
		text, err := h.screenText(ctx, r)
		return strings.Contains(text, pattern), err
	})
	if err == nil && !ok {
		return fmt.Errorf("waited %s for %q, which never appeared", timeout, pattern)
	}
	return err
}

// settle blocks until two reads of the screen agree, the headless form of
// waiting for a frame to stop moving.
func (h *headlessRun) settle(ctx context.Context, r *managedREPL, timeout time.Duration) error {
	previous, seen := "", false
	ok, err := h.poll(ctx, timeout, func() (bool, error) {
		text, err := h.screenText(ctx, r)
		same := seen && text == previous
		previous, seen = text, true
		return same, err
	})
	if err == nil && !ok {
		return fmt.Errorf("screen never settled within %s", timeout)
	}
	return err
}

// poll runs cond once per headlessPollInterval until it holds, reporting false
// once timeout has passed without it.
func (h *headlessRun) poll(ctx context.Context, timeout time.Duration, cond func() (bool, error)) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil || ok {
			return ok, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		if err := sleepHeadless(ctx, headlessPollInterval); err != nil {
			return false, err
		}
	}
}

// shot writes a PNG of the frame the screen holds — the one painted after the
// previous step — and reports it on stdout, which is this mode's only output.
// The path is environment-expanded, so a scenario can name its output
// directory once: :shot $POLLY_SHOT_DIR/splash.png.
func (h *headlessRun) shot(ctx context.Context, r *managedREPL, path string) error {
	var saved string
	var saveErr error
	if err := h.onLoop(ctx, r, func() {
		saved, saveErr = r.writeScreenshot(expandHomePath(os.ExpandEnv(path)))
	}); err != nil {
		return err
	}
	if saveErr != nil {
		return saveErr
	}
	fmt.Println(saved)
	return nil
}

// screenText reads the painted screen as plain text, trailing blanks trimmed,
// the way a terminal capture reports it.
func (h *headlessRun) screenText(ctx context.Context, r *managedREPL) (string, error) {
	var text string
	var captureErr error
	err := h.onLoop(ctx, r, func() {
		var frame *headlessscreen.Frame
		frame, captureErr = h.screen.Snapshot()
		if captureErr == nil {
			text = headlessScreenText(frame)
		}
	})
	if err != nil {
		// Cancellation may return before the queued task runs. Do not read the
		// values it owns until onLoop has observed its completion.
		return "", err
	}
	return text, captureErr
}

func headlessScreenText(screen screenimg.Source) string {
	if screen == nil {
		return ""
	}
	width, height := screen.Size()
	rows := make([]string, 0, height)
	for y := 0; y < height; y++ {
		var row strings.Builder
		for x := 0; x < width; {
			cell, _, cellWidth := screen.Get(x, y)
			if cellWidth < 1 {
				cellWidth = 1
			}
			row.WriteString(cell)
			x += cellWidth
		}
		rows = append(rows, strings.TrimRight(row.String(), " "))
	}
	return strings.Join(rows, "\n")
}

func sleepHeadless(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
