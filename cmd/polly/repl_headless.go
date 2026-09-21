package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// A headless shot run paints the managed TUI on an off-screen simulation screen
// while a script supplies what a keyboard would, so a frame can be captured
// with no terminal, no pty host and no window. The script is played by its own
// goroutine that hands each step to the event loop, which is the only goroutine
// allowed to read or write the screen; the loop paints one frame after every
// step, so a :shot step captures the frame the step before it produced — the
// same "next frame" rule the interactive /screenshot uses.
//
// The script text lives in one place: docs/CLI.md documents it as the
// `polly --shot-script` language.
// headlessDefaultWidth and headlessDefaultHeight are the virtual terminal a
// shot run paints at unless --shot-size says otherwise.
const (
	headlessDefaultWidth  = 120
	headlessDefaultHeight = 40
	// headlessWaitDefault bounds a :wait or :settle no seconds were given for.
	headlessWaitDefault = 10 * time.Second
	// headlessReadyDefault bounds the wait for a TUI that can take input, which
	// is the startup workspace baseline rather than anything the script does.
	headlessReadyDefault = 30 * time.Second
	// headlessPollInterval is how often a waiting step looks at the screen.
	headlessPollInterval = 100 * time.Millisecond
)

// errHeadlessStop ends the script early without failing it: the script asked to
// quit, a scripted key did, or the run itself ended under the player.
var errHeadlessStop = errors.New("shot run ended")

// headlessStep is one script line, parsed.
type headlessStep struct {
	line int
	// kind is the step's directive: submit, type, key, shot, resize, wait,
	// settle, sleep, or quit. A bare script line is a submit.
	kind    string
	text    string
	path    string
	pattern string
	width   int
	height  int
	// timeout bounds a wait or settle step; wait holds a sleep step's duration.
	timeout time.Duration
	wait    time.Duration
}

// headlessRun is a parsed script plus what playing it produced.
type headlessRun struct {
	steps  []headlessStep
	width  int
	height int
	// typed reports that input has reached the TUI once already, so the startup
	// readiness wait happens before the first input step only.
	typed bool

	mu    sync.Mutex
	err   error
	shots []string
}

// headlessSize is the virtual terminal size a shot run paints at.
type headlessSize struct {
	width  int
	height int
}

// parseHeadlessSize reads a WxH argument, defaulting empty to 120x40.
func parseHeadlessSize(value string) (headlessSize, error) {
	size := headlessSize{width: headlessDefaultWidth, height: headlessDefaultHeight}
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return size, nil
	}
	widthText, heightText, ok := strings.Cut(value, "x")
	if !ok {
		return headlessSize{}, fmt.Errorf("invalid size %q: use WxH, for example 120x40", value)
	}
	width, err := strconv.Atoi(strings.TrimSpace(widthText))
	if err != nil {
		return headlessSize{}, fmt.Errorf("invalid size %q: width is not a number", value)
	}
	height, err := strconv.Atoi(strings.TrimSpace(heightText))
	if err != nil {
		return headlessSize{}, fmt.Errorf("invalid size %q: height is not a number", value)
	}
	if width < 20 || width > 1000 || height < 5 || height > 400 {
		return headlessSize{}, fmt.Errorf("size %dx%d is out of range (20-1000 columns, 5-400 rows)", width, height)
	}
	size.width, size.height = width, height
	return size, nil
}

// loadHeadlessRun reads a shot script from path ("-" is stdin) and parses it
// against size. Every parse error is reported before the run starts, so a
// mistyped script captures nothing rather than half a session.
func loadHeadlessRun(path string, size headlessSize) (*headlessRun, error) {
	var data []byte
	var err error
	if path == "-" {
		if data, err = io.ReadAll(os.Stdin); err != nil {
			return nil, fmt.Errorf("read shot script from stdin: %w", err)
		}
	} else if data, err = os.ReadFile(path); err != nil {
		return nil, fmt.Errorf("read shot script: %w", err)
	}

	run := &headlessRun{width: size.width, height: size.height}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(strings.TrimRight(scanner.Text(), "\r"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		step, err := parseHeadlessStep(line, text)
		if err != nil {
			return nil, err
		}
		run.steps = append(run.steps, step)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read shot script: %w", err)
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
	step := headlessStep{line: line}
	if !strings.HasPrefix(text, ":") {
		step.kind, step.text = "submit", text
		return step, nil
	}
	if strings.HasPrefix(text, "::") {
		step.kind, step.text = "submit", text[1:]
		return step, nil
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(text, ":"), " ")
	name = strings.ToLower(strings.TrimSpace(name))
	rest = strings.TrimSpace(rest)
	fail := func(format string, args ...any) (headlessStep, error) {
		return headlessStep{}, fmt.Errorf("shot script line %d: %s", line, fmt.Sprintf(format, args...))
	}

	switch name {
	case "submit":
		if rest == "" {
			return fail(":submit takes the text to send")
		}
		step.kind, step.text = "submit", rest
	case "type":
		if rest == "" {
			return fail(":type takes the text to type")
		}
		step.kind, step.text = "type", rest
	case "key":
		key := strings.ToLower(rest)
		if _, ok := headlessKeyIDs[key]; !ok {
			return fail("unknown key %q: use enter, esc, tab, up, down, left, right, pgup, pgdn, home, end, insert, delete, backspace, space, or c-a to c-z", rest)
		}
		step.kind, step.text = "key", key
	case "shot":
		path := strings.Trim(rest, `"`)
		if path == "" {
			return fail(":shot takes the PNG path to write")
		}
		step.kind, step.path = "shot", path
	case "size":
		size, err := parseHeadlessSize(rest)
		if err != nil {
			return fail("%v", err)
		}
		step.kind, step.width, step.height = "resize", size.width, size.height
	case "wait":
		pattern, seconds, err := splitHeadlessWait(rest)
		if err != nil {
			return fail("%v", err)
		}
		step.kind, step.pattern, step.timeout = "wait", pattern, seconds
	case "settle":
		seconds, err := headlessSeconds(rest, headlessWaitDefault)
		if err != nil {
			return fail(":settle takes optional seconds: %v", err)
		}
		step.kind, step.timeout = "settle", seconds
	case "ready":
		seconds, err := headlessSeconds(rest, headlessReadyDefault)
		if err != nil {
			return fail(":ready takes optional seconds: %v", err)
		}
		step.kind, step.timeout = "ready", seconds
	case "sleep":
		millis, err := strconv.Atoi(rest)
		if err != nil || millis < 0 {
			return fail(":sleep takes milliseconds, for example :sleep 250")
		}
		step.kind, step.wait = "sleep", time.Duration(millis)*time.Millisecond
	case "quit":
		if rest != "" {
			return fail(":quit takes no argument")
		}
		step.kind = "quit"
	default:
		return fail("unknown directive %q: use :key, :type, :submit, :shot, :size, :wait, :settle, :ready, :sleep, or :quit", name)
	}
	return step, nil
}

// splitHeadlessWait splits a :wait argument into its pattern and its optional
// timeout. A quoted pattern keeps its spaces and may still be followed by
// seconds; otherwise a trailing bare number is the timeout, the rule a pattern
// with a number of its own avoids by quoting it: :wait "shot #2".
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
		pattern := rest[1 : 1+end]
		tail := strings.TrimSpace(rest[end+2:])
		if tail == "" {
			return pattern, headlessWaitDefault, nil
		}
		timeout, err := headlessSeconds(tail, headlessWaitDefault)
		if err != nil {
			return "", 0, err
		}
		return pattern, timeout, nil
	}
	pattern, last := rest, ""
	if idx := strings.LastIndex(rest, " "); idx >= 0 {
		pattern, last = strings.TrimSpace(rest[:idx]), rest[idx+1:]
	}
	if pattern != "" && last != "" {
		if _, convErr := strconv.Atoi(last); convErr == nil {
			timeout, err := headlessSeconds(last, headlessWaitDefault)
			if err != nil {
				return "", 0, err
			}
			return pattern, timeout, nil
		}
	}
	return rest, headlessWaitDefault, nil
}

// headlessSeconds parses an optional seconds argument, empty meaning the
// default.
func headlessSeconds(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%q is not a positive number of seconds", value)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// headlessKeyIDs maps a script key name to the event ID the loop dispatches,
// which is exactly what a real terminal event of that key carries (see
// convertTcellKey).
var headlessKeyIDs = map[string]string{
	"enter":     "<Enter>",
	"esc":       "<Escape>",
	"escape":    "<Escape>",
	"tab":       "<Tab>",
	"up":        "<Up>",
	"down":      "<Down>",
	"left":      "<Left>",
	"right":     "<Right>",
	"pgup":      "<PageUp>",
	"pgdn":      "<PageDown>",
	"home":      "<Home>",
	"end":       "<End>",
	"insert":    "<Insert>",
	"delete":    "<Delete>",
	"backspace": "<Backspace>",
	"space":     " ",
}

// headlessKeyCodes inverts tcellKeyMap, so a scripted key carries the same
// tcell key the terminal would report for it.
var headlessKeyCodes = func() map[string]tcell.Key {
	codes := make(map[string]tcell.Key, len(tcellKeyMap))
	for key, id := range tcellKeyMap {
		if _, taken := codes[id]; !taken {
			codes[id] = key
		}
	}
	// Both tcell backspace codes share one ID; the interleaved-delete one is
	// what a terminal sends for the key itself.
	codes["<Backspace>"] = tcell.KeyBackspace2
	return codes
}()

func init() {
	for c := 'a'; c <= 'z'; c++ {
		headlessKeyIDs["c-"+string(c)] = fmt.Sprintf("<C-%c>", c)
	}
	// These control characters are reported as named keys by the terminal.
	headlessKeyIDs["c-i"] = "<Tab>"
	headlessKeyIDs["c-m"] = "<Enter>"
}

// headlessKeyEvent builds the event the loop would receive for a scripted key.
func headlessKeyEvent(name string) (ui.Event, bool) {
	id, ok := headlessKeyIDs[name]
	if !ok {
		return ui.Event{}, false
	}
	if id == " " {
		return ui.Event{Type: ui.KeyboardEvent, ID: id, Payload: tcell.NewEventKey(tcell.KeyRune, " ", tcell.ModNone)}, true
	}
	code, ok := headlessKeyCodes[id]
	if !ok {
		return ui.Event{}, false
	}
	return ui.Event{Type: ui.KeyboardEvent, ID: id, Payload: tcell.NewEventKey(code, "", tcell.ModNone)}, true
}

// installScreen points the run at an off-screen screen of the scripted size
// instead of the terminal's own. The run wraps it in themedScreen like any
// other, so the frame is painted exactly as it would be on a terminal.
func (h *headlessRun) installScreen() error {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		return fmt.Errorf("start off-screen screen: %w", err)
	}
	sim.SetSize(h.width, h.height)
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
	err := h.walk(ctx, r)
	if errors.Is(err, errHeadlessStop) {
		err = nil
	}
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	r.requestQuit()
}

func (h *headlessRun) walk(ctx context.Context, r *managedREPL) error {
	for _, step := range h.steps {
		if err := h.one(ctx, r, step); err != nil {
			if errors.Is(err, errHeadlessStop) {
				return errHeadlessStop
			}
			return fmt.Errorf("shot script line %d: %w", step.line, err)
		}
	}
	return nil
}

func (h *headlessRun) one(ctx context.Context, r *managedREPL, step headlessStep) error {
	switch step.kind {
	case "submit":
		if err := h.ready(ctx, r); err != nil {
			return err
		}
		if err := h.typeText(ctx, r, step.text); err != nil {
			return err
		}
		return h.press(ctx, r, "enter")
	case "type":
		if err := h.ready(ctx, r); err != nil {
			return err
		}
		return h.typeText(ctx, r, step.text)
	case "key":
		return h.press(ctx, r, step.text)
	case "ready":
		return h.waitReady(ctx, r, step.timeout)
	case "shot":
		return h.shot(ctx, r, step.path)
	case "resize":
		return h.resize(ctx, r, step.width, step.height)
	case "wait":
		return h.waitFor(ctx, r, step.pattern, step.timeout)
	case "settle":
		return h.settle(ctx, r, step.timeout)
	case "sleep":
		return sleepHeadless(ctx, step.wait)
	case "quit":
		return errHeadlessStop
	}
	return fmt.Errorf("unknown step %q", step.kind)
}

// onLoop runs fn on the event loop, where every screen read and write belongs.
func (h *headlessRun) onLoop(ctx context.Context, r *managedREPL, fn func()) error {
	done := make(chan struct{})
	task := func() {
		fn()
		close(done)
	}
	select {
	case r.headlessTasks <- task:
	case <-r.headlessDone:
		return errHeadlessStop
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	select {
	case <-done:
		return nil
	case <-r.headlessDone:
		return errHeadlessStop
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// press delivers one key. It mirrors the event loop's own arm: the key is
// handled, and the follow-ups that start a turn the input queued — or end the
// run — happen here too, or a scripted Enter would queue a prompt forever.
func (h *headlessRun) press(ctx context.Context, r *managedREPL, name string) error {
	ev, ok := headlessKeyEvent(name)
	if !ok {
		return fmt.Errorf("unknown key %q", name)
	}
	stop := false
	err := h.onLoop(ctx, r, func() {
		if r.handleEvent(ev) {
			stop = true
			return
		}
		r.afterInput()
	})
	if err != nil {
		return err
	}
	if stop {
		return errHeadlessStop
	}
	return nil
}

// typeText delivers text one rune at a time, the way a terminal does; the
// frame is painted once when the step ends.
func (h *headlessRun) typeText(ctx context.Context, r *managedREPL, text string) error {
	stop := false
	err := h.onLoop(ctx, r, func() {
		for _, char := range text {
			id := string(char)
			ev := ui.Event{Type: ui.KeyboardEvent, ID: id, Payload: tcell.NewEventKey(tcell.KeyRune, id, tcell.ModNone)}
			if r.handleEvent(ev) {
				stop = true
				return
			}
		}
		r.afterInput()
	})
	if err != nil {
		return err
	}
	if stop {
		return errHeadlessStop
	}
	return nil
}

// ready waits for a TUI that can take input before the first typed line of a
// run. The startup workspace baseline runs behind the first frame, and input
// that arrives before it drains is queued rather than run — honest for a fast
// typist, but not what a script asking for a command's output means.
func (h *headlessRun) ready(ctx context.Context, r *managedREPL) error {
	if h.typed {
		return nil
	}
	h.typed = true
	return h.waitReady(ctx, r, headlessReadyDefault)
}

// waitReady blocks until the composer would submit rather than queue.
func (h *headlessRun) waitReady(ctx context.Context, r *managedREPL, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var ready bool
		if err := h.onLoop(ctx, r, func() { ready = r.acceptsInput() }); err != nil {
			return err
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the TUI was not ready for input within %s", timeout)
		}
		if err := sleepHeadless(ctx, headlessPollInterval); err != nil {
			return err
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
		saved, saveErr = r.writeScreenshot(expandUserPath(os.ExpandEnv(path)))
	}); err != nil {
		return err
	}
	if saveErr != nil {
		return saveErr
	}
	h.mu.Lock()
	h.shots = append(h.shots, saved)
	h.mu.Unlock()
	fmt.Println(saved)
	return nil
}

// resize changes the virtual terminal the frame is laid out for.
func (h *headlessRun) resize(ctx context.Context, r *managedREPL, width, height int) error {
	return h.onLoop(ctx, r, func() {
		if sim, ok := ui.DefaultBackend.Screen.(themedScreen); ok {
			if screen, ok := sim.Screen.(tcell.SimulationScreen); ok {
				screen.SetSize(width, height)
			}
		}
	})
}

// waitFor polls the painted screen until it contains pattern. The read happens
// on the event loop, so it never races a paint.
func (h *headlessRun) waitFor(ctx context.Context, r *managedREPL, pattern string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		text, err := h.screenText(ctx, r)
		if err != nil {
			return err
		}
		if strings.Contains(text, pattern) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waited %s for %q, which never appeared", timeout, pattern)
		}
		if err := sleepHeadless(ctx, headlessPollInterval); err != nil {
			return err
		}
	}
}

// settle waits until two reads of the screen agree, the headless form of
// waiting for a frame to stop moving.
func (h *headlessRun) settle(ctx context.Context, r *managedREPL, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	previous, err := h.screenText(ctx, r)
	if err != nil {
		return err
	}
	for {
		if err := sleepHeadless(ctx, headlessPollInterval); err != nil {
			return err
		}
		current, err := h.screenText(ctx, r)
		if err != nil {
			return err
		}
		if current == previous {
			return nil
		}
		previous = current
		if time.Now().After(deadline) {
			return fmt.Errorf("screen never settled within %s", timeout)
		}
	}
}

// screenText reads the painted screen as plain text, trailing blanks trimmed,
// the way a terminal capture reports it.
func (h *headlessRun) screenText(ctx context.Context, r *managedREPL) (string, error) {
	var text string
	err := h.onLoop(ctx, r, func() {
		text = headlessScreenText(ui.DefaultBackend.Screen)
	})
	return text, err
}

func headlessScreenText(screen tcell.Screen) string {
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
