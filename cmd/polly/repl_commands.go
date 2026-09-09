package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

type replCommandContext struct {
	ctx    context.Context
	config *Config
	// settings are the session the commands act on: /get reads them, /set
	// changes and persists them. Nil when no session is attached, which
	// makes /set report that settings are unavailable.
	settings *Settings
	state    *conversationState
	registry *replCommandRegistry
	reply    func(string) error
	// replyMarkup, when set, appends a line that carries its own styling
	// (two-tone help rows); the line frontend leaves it nil and gets plain text.
	replyMarkup func(string)

	// Interactive-only operations are callbacks so the command parser stays
	// independent of the managed REPL's turn state. The fallback REPL
	// leaves them nil; handlers report that the operation is unavailable.
	clearTranscript   func() error
	resetConversation func() error
	// settingsApplied lets the interactive REPL refresh UI derived from config
	// (e.g. the status-row model name) after /set mutates it.
	settingsApplied func()
	// attachImage validates a local image, registers it, and inserts its
	// "[image #N]" token into the composer, returning the token.
	attachImage func(path string) (string, error)
	// setContextName updates the UI's displayed context name after /rename.
	setContextName func(name string)
	titleChanged   func()
	// Picker callbacks are managed-TUI operations. Keeping
	// them out of command parsing lets the fallback REPL retain textual /set.
	openModelPicker    func()
	openKeyManager     func()
	openSessionsPicker func()
	// Tab callbacks are managed-TUI operations too; the fallback REPL holds
	// one session and leaves them nil.
	newTab      func()
	closeTab    func()
	spawnAgent  func(subagent.Request)
	inspectView func(string)
	openSwarm   func(string)
}

func (c *replCommandContext) operationContext() context.Context {
	if c != nil && c.ctx != nil {
		return c.ctx
	}
	if c != nil && c.state != nil && c.state.session != nil {
		return c.state.session.Context()
	}
	return context.Background()
}

var defaultReplCommands = newDefaultReplCommandRegistry()

func newDefaultReplCommandRegistry() *replCommandRegistry {
	r := newReplCommandRegistry()
	registerInspectorCommands(r)
	registerSwarmCommands(r)
	r.register(replCommand{
		name:     "/attach",
		usage:    "/attach <image-path>",
		summary:  "attach a local image to the next prompt",
		busySafe: true,
		run:      replAttachCommand,
	})
	r.register(replCommand{
		name:     "/clear",
		usage:    "/clear",
		summary:  "clear the display (keep conversation history)",
		busySafe: true,
		run:      replClearCommand,
	})
	r.register(replCommand{
		name:     "/close",
		usage:    "/close",
		summary:  "close this tab (its session stays saved)",
		busySafe: true,
		run:      replCloseCommand,
	})
	r.register(replCommand{
		name:     "/context",
		usage:    "/context",
		summary:  "session tokens, capacity, counts",
		busySafe: true,
		run:      replContextCommand,
	})
	r.register(replCommand{
		name:    "/exit",
		aliases: []string{"/quit"},
		usage:   "/exit",
		summary: "leave the REPL",
		run: func(*replCommandContext, []string) replCommandResult {
			return replCommandResult{quit: true}
		},
	})
	r.register(replCommand{
		name:     "/help",
		usage:    "/help [command]",
		summary:  "show this help",
		busySafe: true,
		run:      replHelpCommand,
		complete: completeHelpCommand,
	})
	r.register(replCommand{
		name:    "/keys",
		usage:   "/keys",
		summary: "configure provider keys for this run",
		run:     replKeysCommand,
	})
	r.register(replCommand{
		name:    "/model",
		usage:   "/model",
		summary: "select a provider and model",
		run:     replModelCommand,
	})
	r.register(replCommand{
		name:     "/new",
		usage:    "/new",
		summary:  "open a new tab on a fresh session",
		busySafe: true,
		run:      replNewCommand,
	})
	r.register(replCommand{
		name:    "/rename",
		usage:   "/rename <name>",
		summary: "rename the current context",
		run:     replRenameCommand,
	})
	r.register(replCommand{
		name: "/title", usage: "/title <text>", summary: "edit the current session title", run: replTitleCommand,
	})
	r.register(replCommand{
		name:     "/sessions",
		aliases:  []string{"/resume"},
		usage:    "/sessions",
		summary:  "switch to an open session or resume a saved one",
		busySafe: true,
		run:      replSessionsCommand,
	})
	r.register(replCommand{
		name:    "/reset",
		usage:   "/reset confirm",
		summary: "clear durable conversation history",
		run:     replResetCommand,
	})
	r.register(replCommand{
		name:         "/set",
		usage:        "/set [key [value]]",
		summary:      "show or change settings",
		busySafeWhen: func(args []string) bool { return len(args) < 3 },
		run:          replSetCommand,
		complete:     completeSetCommand,
	})
	r.register(replCommand{
		name:     "/spawn",
		usage:    "/spawn [--read-only] <brief>",
		summary:  "start a background swarm member; inspect with /sessions",
		busySafe: true,
		run:      replSpawnCommand,
	})
	r.register(replCommand{
		name:     "/tools",
		usage:    "/tools [list [namespace]|show <name>]",
		summary:  "inspect loaded tools and skills",
		busySafe: true,
		run:      replToolsCommand,
		complete: completeToolsCommand,
	})
	return r
}

type keyHelpRow struct{ key, desc string }

type keyHelpGroup struct {
	title string
	rows  []keyHelpRow
}

// keyHelpGroups is the key reference by task. Editing keys follow readline,
// so two rows cover the chords instead of one row each; no key column is
// wider than the widest navigation key, which keeps help legible at 80.
func keyHelpGroups() []keyHelpGroup {
	return []keyHelpGroup{
		{"Send and edit", []keyHelpRow{
			{"Enter", "Send the message"},
			{"Ctrl-J", "Insert a newline"},
			{"Tab", "Complete a slash command"},
			{"Ctrl-R", "Search history"},
			{"Ctrl-V", "Attach the clipboard image"},
			{"Ctrl-L", "Clear the display"},
			{"Ctrl-C", "Interrupt the turn · twice to quit"},
			{"Ctrl-Z", "Suspend to the shell (fg resumes)"},
			{"Esc", "Dismiss a dialog, search, or the inspector · interrupt"},
			{"Ctrl-A/E Ctrl-U/K", "Line start or end · clear to the start or end"},
			{"Ctrl-W Alt-B/F Alt-D", "Delete the previous word · move or delete by word"},
		}},
		{"Navigate", []keyHelpRow{
			{"Up / Down", "Move or recall history · scroll a focused inspector"},
			{"PgUp / PgDn", "Page the transcript · the inspector when it has focus"},
			{"Home / End", "Line start or end · focused inspector top or follow"},
			{"Alt-1..9 Alt-] Alt-[", "Switch workspace"},
			{"Ctrl-G", "Open the sessions picker"},
			{"Shift-drag", "Select terminal text"},
		}},
		{"Inspect", []keyHelpRow{
			{"Tab", "Focus the inspector from an empty composer · Esc returns"},
			{"Left / Right", "Previous or next tool or thought in a focused inspector"},
			{"Click detail", "Inspect an agent, tool result, or thought"},
			{"Click disclosure", "Expand thinking or tool calls"},
			{"Ctrl-O", "Toggle thinking for the latest turn"},
			{"Click thumbnail", "Open the image"},
		}},
		{"Approve", []keyHelpRow{
			{"y", "Allow"},
			{"n Enter Esc", "Deny"},
			{"a", "Allow the rest of the batch"},
		}},
	}
}

func (ctx *replCommandContext) configOrDefault() *Config {
	if ctx != nil && ctx.config != nil {
		return ctx.config
	}
	return &Config{}
}

func (ctx *replCommandContext) settingsOrDefault() *Settings {
	if ctx != nil && ctx.settings != nil {
		return ctx.settings
	}
	return &Settings{}
}

func (ctx *replCommandContext) replyLine(line string) error {
	if ctx != nil && ctx.reply != nil {
		return ctx.reply(line)
	}
	return nil
}

func (ctx *replCommandContext) replyLines(lines []string) error {
	for _, line := range lines {
		if err := ctx.replyLine(line); err != nil {
			return err
		}
	}
	return nil
}

func newManagedReplCommandContext(r *managedREPL) *replCommandContext {
	cfg := &Config{}
	if r.config != nil {
		cfg = r.config
	}
	settings := r.sessionSettings()
	return &replCommandContext{
		config:   cfg,
		settings: settings,
		state:    r.state,
		registry: defaultReplCommands,
		reply: func(line string) error {
			r.model.appendNoticeLine(line)
			return nil
		},
		replyMarkup: r.model.appendLine,
		clearTranscript: func() error {
			r.model.clearDisplay()
			return nil
		},
		// Commands run on the event loop with the model lock held, so this
		// mutates the model directly like reply/clearTranscript do.
		setContextName: func(name string) {
			r.model.setContextName(name)
			if i := r.visibleTabIndex(); i >= 0 {
				r.tabs[i].name = name
			}
		},
		titleChanged: func() {
			r.refreshSessionTitle(r.visibleTab().viewID(), r.model)
		},
		resetConversation: func() error {
			if r.state == nil || r.state.session == nil {
				return fmt.Errorf("no active session")
			}
			queued, err := r.model.materializeQueuedImagesForReset(r.state.session.Context())
			if err != nil {
				r.model.discardQueuedInputs()
				return fmt.Errorf("preserve queued images: %w", err)
			}
			if err := r.state.session.Clear(r.state.session.Context()); err != nil {
				// A queued reset is a barrier. A Clear error means the history
				// was not cleared, so later prompts must not run against the old
				// history. Restore the snapshot only long enough to mark it not sent.
				r.model.queue = queued
				r.model.discardQueuedInputs()
				return err
			}
			r.model.clearDisplay()
			r.model.clearRestoredDraft()
			r.model.lastOutcome = turnOutcomeNone
			r.model.lastIn = 0
			r.model.lastOut = 0
			// Keep the epoch moving so an inspector on a wiped item sees the change.
			r.model.inspections = inspectionSource{epoch: r.model.inspections.epoch + 1}
			r.model.status.clearContextUsage(r.state.settings.MaxHistoryTokens)
			r.model.lastElapsed = 0
			r.model.turnHasOutput = false
			r.model.outcomeLabeled = false
			if err := r.model.restoreQueuedImagesAfterReset(r.state.session.Context(), queued); err != nil {
				r.model.discardQueuedInputs()
				return fmt.Errorf("restore queued images: %w", err)
			}
			return nil
		},
		attachImage: func(path string) (string, error) {
			img, ok := markdown.ResolveLocalImage(path, "", r.model.imageBaseDir)
			if !ok {
				return "", fmt.Errorf("not a readable local image")
			}
			token := r.model.registerAttachment(img.Path, filepath.Base(img.Path))
			r.model.insertEditorText(token + " ")
			return token, nil
		},
		settingsApplied: func() {
			if settings == nil {
				return
			}
			r.model.setModelName(settings.Model)
			r.model.status.rememberModel(settings.Model)
			r.model.status.clearContextUsage(settings.MaxHistoryTokens)
		},
		openModelPicker:    r.openModelPicker,
		openKeyManager:     r.openKeyManager,
		openSessionsPicker: r.openSessionsPicker,
		newTab:             r.requestNewTabLocked,
		closeTab:           r.requestCloseTabLocked,
		spawnAgent:         r.requestSpawnLocked,
		inspectView:        r.inspectCommand,
		openSwarm: func(section string) {
			target := tabViewTarget(r.visibleTab())
			target.kind = swarmViewKind
			target.item = section
			r.inspect(target)
		},
	}
}

func newWriterReplCommandContext(config *Config, state *conversationState, w io.Writer) *replCommandContext {
	ctx := &replCommandContext{
		ctx:      context.Background(),
		config:   config,
		state:    state,
		registry: defaultReplCommands,
		reply: func(line string) error {
			_, err := fmt.Fprintln(w, line)
			return err
		},
	}
	if state != nil {
		ctx.settings = &state.settings
	}
	if state != nil && state.session != nil {
		ctx.ctx = state.session.Context()
		ctx.resetConversation = func() error { return state.session.Clear(ctx.ctx) }
	}
	return ctx
}

func (r *managedREPL) runCommand(line string) (handled, quit bool) {
	handled, quit, err := defaultReplCommands.dispatch(line, newManagedReplCommandContext(r))
	if err != nil {
		r.model.appendNoticeLine("Error: " + err.Error())
		return true, false
	}
	return handled, quit
}

func replHelpCommand(ctx *replCommandContext, args []string) replCommandResult {
	if ctx == nil || ctx.registry == nil {
		return replCommandResult{}
	}
	if len(args) > 1 {
		return replCommandResult{err: ctx.replyLines(ctx.registry.helpFor(args[1]))}
	}
	if ctx.replyMarkup != nil {
		for _, line := range ctx.registry.helpLinesStyled(true) {
			ctx.replyMarkup(line)
		}
		return replCommandResult{}
	}
	return replCommandResult{err: ctx.replyLines(ctx.registry.helpLines())}
}

func replAttachCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) < 2 {
		return replCommandResult{err: ctx.replyLine("usage: /attach <image-path>")}
	}
	if ctx == nil || ctx.attachImage == nil {
		return replCommandResult{err: ctx.replyLine("attachments require the managed REPL")}
	}
	// Fields-split args lose original spacing; rejoining and reusing the
	// drag-drop splitter recovers quoted and escaped paths with spaces.
	raw := strings.Join(args[1:], " ")
	paths := splitDroppedPaths(raw)
	if len(paths) == 0 {
		paths = []string{raw}
	}
	var lines []string
	for _, path := range paths {
		token, err := ctx.attachImage(path)
		if err != nil {
			lines = append(lines, fmt.Sprintf("attach %s: %v", path, err))
			continue
		}
		lines = append(lines, fmt.Sprintf("attached %s as %s", filepath.Base(path), token))
	}
	return replCommandResult{err: ctx.replyLines(lines)}
}

func replClearCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /clear")}
	}
	if ctx == nil || ctx.clearTranscript == nil {
		return replCommandResult{err: ctx.replyLine("display clear unavailable")}
	}
	if err := ctx.clearTranscript(); err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("failed to clear display: %v", err))}
	}
	return replCommandResult{err: ctx.replyLine("display cleared")}
}

func replRenameCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 2 {
		return replCommandResult{err: ctx.replyLine("usage: /rename <name>")}
	}
	if ctx.state == nil || ctx.state.session == nil {
		return replCommandResult{err: ctx.replyLine("no active session")}
	}
	opCtx := ctx.operationContext()
	oldName, err := ctx.state.session.GetName(opCtx)
	if err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("rename failed: %v", err))}
	}
	newName := args[1]
	if err := ctx.state.session.Rename(opCtx, newName); err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("rename failed: %v", err))}
	}
	if ctx.setContextName != nil {
		ctx.setContextName(newName)
	}
	return replCommandResult{err: ctx.replyLine(fmt.Sprintf("renamed context '%s' to '%s'", oldName, newName))}
}

func replTitleCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) < 2 {
		return replCommandResult{err: ctx.replyLine("usage: /title <text>")}
	}
	if ctx.state == nil || ctx.state.session == nil {
		return replCommandResult{err: ctx.replyLine("no active session")}
	}
	setter, ok := ctx.state.session.(sessions.TitleSession)
	if !ok {
		return replCommandResult{err: ctx.replyLine("session titles are unavailable")}
	}
	if _, err := setter.SetTitle(ctx.operationContext(), strings.Join(args[1:], " "), sessions.TitleSourceUser); err != nil {
		return replCommandResult{err: ctx.replyLine("title update failed: " + err.Error())}
	}
	if ctx.titleChanged != nil {
		ctx.titleChanged()
	}
	return replCommandResult{}
}

func replResetCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 2 || args[1] != "confirm" {
		return replCommandResult{err: ctx.replyLine("reset clears durable conversation history; run /reset confirm")}
	}
	if ctx == nil || ctx.resetConversation == nil {
		return replCommandResult{err: ctx.replyLine("conversation reset unavailable")}
	}
	if err := ctx.resetConversation(); err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("failed to reset conversation: %v", err))}
	}
	return replCommandResult{err: ctx.replyLine("conversation reset")}
}

func replModelCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /model")}
	}
	if ctx == nil || ctx.openModelPicker == nil {
		return replCommandResult{err: ctx.replyLine("model picker unavailable here; use /set model provider/model")}
	}
	ctx.openModelPicker()
	return replCommandResult{}
}

func replKeysCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /keys")}
	}
	if ctx == nil || ctx.openKeyManager == nil {
		return replCommandResult{err: ctx.replyLine("key manager is available only in the managed TUI")}
	}
	ctx.openKeyManager()
	return replCommandResult{}
}

func replSessionsCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /sessions")}
	}
	if ctx == nil || ctx.openSessionsPicker == nil {
		return replCommandResult{err: ctx.replyLine("session picker is available only in the managed TUI")}
	}
	ctx.openSessionsPicker()
	return replCommandResult{}
}

func replNewCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /new")}
	}
	if ctx == nil || ctx.newTab == nil {
		return replCommandResult{err: ctx.replyLine("tabs are available only in the managed TUI")}
	}
	ctx.newTab()
	return replCommandResult{}
}

func replCloseCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /close")}
	}
	if ctx == nil || ctx.closeTab == nil {
		return replCommandResult{err: ctx.replyLine("tabs are available only in the managed TUI")}
	}
	ctx.closeTab()
	return replCommandResult{}
}

func replSpawnCommand(ctx *replCommandContext, args []string) replCommandResult {
	args = args[1:]
	readOnly := len(args) > 0 && args[0] == "--read-only"
	if readOnly {
		args = args[1:]
	}
	brief := strings.TrimSpace(strings.Join(args, " "))
	if brief == "" {
		return replCommandResult{err: ctx.replyLine("usage: /spawn [--read-only] <brief>")}
	}
	if ctx == nil || ctx.spawnAgent == nil {
		return replCommandResult{err: ctx.replyLine("agents are available only in the managed TUI")}
	}
	ctx.spawnAgent(subagent.Request{Task: brief, Label: brief, ReadOnly: readOnly})
	return replCommandResult{}
}

func replContextCommand(ctx *replCommandContext, args []string) replCommandResult {
	if ctx == nil || ctx.state == nil || ctx.state.session == nil {
		return replCommandResult{err: ctx.replyLine("no active session")}
	}
	settings := ctx.settingsOrDefault()
	s := ctx.state.session
	opCtx := ctx.operationContext()
	name, err := s.GetName(opCtx)
	if err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("context unavailable: %v", err))}
	}
	lines := []string{"context: " + name}
	md, err := s.GetMetadata(opCtx)
	if err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("context unavailable: %v", err))}
	}
	if label := sessions.DisplayLabel(md); label != name {
		lines = append(lines, "title: "+label)
	}
	if settings.Model != "" {
		lines = append(lines, "model: "+llm.ModelName(settings.Model))
	}
	totalTokens, err := s.GetTotalTokens(opCtx)
	if err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("context unavailable: %v", err))}
	}
	lines = append(lines, "transcript: "+humanizeTokens(totalTokens)+" estimated tokens (durable)")
	if settings.MaxHistoryTokens > 0 {
		line := "model budget: " + humanizeTokens(settings.MaxHistoryTokens) + " estimated tokens"
		if md, err := s.GetMetadata(opCtx); err == nil && md != nil {
			if window := md.ContextWindows[settings.Model]; window > 0 {
				if clamped := llm.ClampContextBudget(settings.MaxHistoryTokens, window, settings.MaxTokens); clamped < settings.MaxHistoryTokens {
					line = "model budget: " + humanizeTokens(clamped) + " estimated tokens (clamped from " +
						humanizeTokens(settings.MaxHistoryTokens) + " by the model's " + humanizeTokens(window) + "-token window)"
				}
			}
		}
		lines = append(lines, line)
	} else {
		lines = append(lines, "model budget: unlimited")
	}
	c, err := s.GetMessageCounts(opCtx)
	if err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("context unavailable: %v", err))}
	}
	lines = append(lines, fmt.Sprintf("messages: user %d · assistant %d · tool %d · system %d",
		c["user"], c["assistant"], c["tool"], c["system"]))
	toolCalls, err := s.GetToolCallCount(opCtx)
	if err != nil {
		return replCommandResult{err: ctx.replyLine(fmt.Sprintf("context unavailable: %v", err))}
	}
	lines = append(lines, fmt.Sprintf("tool calls: %d", toolCalls))
	return replCommandResult{err: ctx.replyLines(lines)}
}

func completeSetCommand(_ *replCommandContext, fields []string, prefix string) []string {
	switch completionArgPos(fields, prefix) {
	case 1:
		return matchingWords(replSettingKeys, prefix)
	case 2:
		if spec, ok := settingSpecFor(fields[1]); ok && spec.setWords != nil {
			return matchingWords(spec.setWords, prefix)
		}
	}
	return nil
}

// replSetCommand shows every setting, one setting, or changes one: /set,
// /set key, /set key value.
func replSetCommand(ctx *replCommandContext, args []string) replCommandResult {
	switch len(args) {
	case 1:
		lines := []string{"settings:"}
		for _, k := range replSettingKeys {
			v, _ := replSettingValue(ctx, k)
			lines = append(lines, fmt.Sprintf("  %s: %s", k, v))
		}
		return replCommandResult{err: ctx.replyLines(lines)}
	case 2:
		value, ok := replSettingValue(ctx, args[1])
		if !ok {
			return replCommandResult{err: ctx.replyLine("unknown key: " + args[1] + " (keys: " + strings.Join(replSettingKeys, ", ") + ")")}
		}
		return replCommandResult{err: ctx.replyLine(args[1] + ": " + value)}
	case 3:
		if ctx == nil || ctx.settings == nil {
			return replCommandResult{err: ctx.replyLine("settings unavailable")}
		}
		line, err := applyAndPersistSetting(ctx, args[1], args[2])
		if err != nil {
			return replCommandResult{err: ctx.replyLine(err.Error())}
		}
		return replCommandResult{err: ctx.replyLine(line)}
	default:
		return replCommandResult{err: ctx.replyLine("usage: /set [key [value]]. settable: " + strings.Join(replSettableKeys, ", "))}
	}
}

func applyAndPersistSetting(ctx *replCommandContext, key, value string) (string, error) {
	spec, ok := settingSpecFor(key)
	if !ok || spec.parse == nil {
		return "", fmt.Errorf("unknown or read-only key: %s (settable: %s)", key, strings.Join(replSettableKeys, ", "))
	}
	if ctx.settings == nil {
		return "", fmt.Errorf("settings unavailable")
	}
	if err := spec.parse(ctx.settings, value); err != nil {
		return "", err
	}
	if spec.postReplSet != nil {
		spec.postReplSet(ctx)
	}
	if ctx.settingsApplied != nil {
		ctx.settingsApplied()
	}
	line := key + ": " + spec.show(ctx, ctx.settingsOrDefault())
	if err := persistReplSettings(ctx); err != nil {
		line += " (applied for this run; persisting failed: " + err.Error() + ")"
	}
	return line, nil
}

// persistReplSettings writes the resolved settings back to session metadata —
// the same fields updateContextInfo records at startup — so a /set survives
// into the next launch of this context.
func persistReplSettings(ctx *replCommandContext) error {
	if ctx.state == nil || ctx.state.session == nil {
		return nil
	}
	settings := ctx.settingsOrDefault()
	// What /set can change, /set must persist: every settable row reaches
	// metadata here, or the change would silently die at relaunch.
	return updateMetadata(ctx.operationContext(), ctx.state.session, func(md *sessions.Metadata) {
		for _, spec := range settingSpecs {
			if spec.parse != nil {
				spec.toMeta(settings, md)
			}
		}
	})
}

func replSettingValue(ctx *replCommandContext, key string) (string, bool) {
	spec, ok := settingSpecFor(key)
	if !ok || spec.show == nil {
		return "", false
	}
	return spec.show(ctx, ctx.settingsOrDefault()), true
}

func completeHelpCommand(ctx *replCommandContext, fields []string, prefix string) []string {
	if completionArgPos(fields, prefix) != 1 || ctx == nil || ctx.registry == nil {
		return nil
	}
	return matchingWords(ctx.registry.commandNames(), prefix)
}
