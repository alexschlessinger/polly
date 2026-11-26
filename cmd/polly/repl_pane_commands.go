package main

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
)

// Commands as questions (see repl_pane_ask.go): /help lists what can be typed
// after a slash, and the row chosen is typed for the user: a command that
// needs nothing more runs, and one that needs an argument waits in the
// composer for it. /title asks for the title, and /model lists the
// provider's models. The host's list asks where there is one, and polly's
// own otherwise.

// The host's questions about commands.
const (
	askCommands = "commands"
	askTitle    = "title"
	askModels   = "models"
)

// The rows of those lists that are not a command, a skill or a model.
const (
	pickHelpPage  = "help"
	pickSetupForm = "setup"
	pickSkill     = "skill:"
)

// modelCatalogTime bounds loading the models to list.
const modelCatalogTime = 15 * time.Second

// pickCommandInHost lists the commands and skills and types the one chosen.
// False means nobody could ask, and the caller shows the help page. Caller
// holds the visible model's lock.
func (r *managedREPL) pickCommandInHost() bool {
	if r.hostAsking(askCommands) {
		return true // The list is up already.
	}
	registry := defaultReplCommands
	var choices []paneChoice
	commands := slices.Clone(registry.commands)
	sort.Slice(commands, func(i, j int) bool { return commands[i].name < commands[j].name })
	for _, cmd := range commands {
		if cmd.hidden {
			continue
		}
		choices = append(choices, paneChoice{
			id:       cmd.name,
			title:    cmd.usage,
			subtitle: cmd.summary,
			detail:   strings.Join(cmd.aliases, " "),
			icon:     paneIconCommand,
		})
	}
	if catalog := r.composerCatalog(); catalog != nil {
		for _, skill := range catalog.List() {
			// A skill a command activates is listed as that command.
			if _, reserved := registry.get("/" + skill.Name); reserved || skill.Command != "" {
				continue
			}
			choices = append(choices, paneChoice{
				id:       pickSkill + skill.Name,
				title:    "/" + skill.Name,
				subtitle: style.Truncate(strings.Join(strings.Fields(skill.Description), " "), pickTitleWidth),
				detail:   "skill",
				icon:     paneIconSkill,
			})
		}
	}
	choices = append(choices, paneChoice{id: pickHelpPage, title: "Keys and the full help", subtitle: "open the help page in polly", icon: paneIconPage})
	return r.ask(paneQuestion{title: "Polly commands", hint: "Filter commands and skills", choices: choices}, askCommands, func(answer paneAnswer) {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
		switch skill, isSkill := strings.CutPrefix(answer.id, pickSkill); {
		case !answer.given:
		case answer.id == pickHelpPage:
			if !r.dialogOpen() {
				r.openHelp(registry.helpLines(), "Help")
			}
		case isSkill:
			// A skill is named at the start of the prompt it is for.
			r.model.ed.setText("/" + skill + " " + r.model.ed.text())
		default:
			if cmd, ok := registry.get(answer.id); ok {
				r.typeCommandLocked(cmd)
			}
		}
	})
}

// typeCommandLocked does what typing a command chosen from a list would: one
// that needs an argument waits in the composer for it, and any other runs as
// Enter would run it, leaving a draft where it was. Caller holds the visible
// model's lock.
func (r *managedREPL) typeCommandLocked(cmd replCommand) {
	m := r.model
	draft := m.ed.text()
	drafted := strings.TrimSpace(draft) != ""
	if strings.Contains(cmd.usage, "<") {
		if drafted {
			// The draft is not this command's argument, and is not thrown away.
			m.appendNoticeLine("usage: " + cmd.usage)
			return
		}
		m.ed.setText(cmd.name + " ")
		return
	}
	m.ed.setText(cmd.name)
	r.submitComposerLocked()
	if drafted {
		m.ed.setText(draft)
	}
}

// editTitle is /title with nothing after it: the visible session's title, for
// the user to change. Caller holds the visible model's lock.
func (r *managedREPL) editTitle() {
	tab := r.visibleTab()
	if tab == nil || r.hostAsking(askTitle) {
		return
	}
	title := r.model.status.title
	r.ask(paneQuestion{title: "Session title", hint: "A title for this session", text: true, value: title, confirm: "Save title"}, askTitle, func(answer paneAnswer) {
		if !answer.given || strings.TrimSpace(answer.text) == "" || !slices.Contains(r.tabs, tab) {
			return // Declined, or the session closed while the question was up.
		}
		tab.model.mu.Lock()
		defer tab.model.mu.Unlock()
		r.runCommandOn(tab, "/title "+answer.text)
	})
}

// pickModelInHost is /model: the models the session's provider offers, for
// this session to switch to, with the setup form one row away. False means
// there is no list to show, and the caller opens the form. Caller holds the
// visible model's lock.
func (r *managedREPL) pickModelInHost() bool {
	if r.hostAsking(askModels) {
		return true // The list is up already.
	}
	tab, state := r.visibleTab(), r.state
	if tab == nil || state == nil || state.agent == nil || r.work == nil {
		return false
	}
	current := state.settings.Model
	provider, _, routed := strings.Cut(current, "/")
	if !routed {
		return false
	}
	agent, target := state.agent, r.browserTarget(provider, "")
	// The form is what the list falls back to, whatever goes wrong.
	form := func() {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
		if r.model == tab.model && !r.dialogOpen() {
			r.openSetupForm()
		}
	}
	return r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, modelCatalogTime)
		defer cancel()
		catalog, err := agent.ListModels(ctx, target, false)
		r.postUI(r.work.ctx, func() {
			choices := modelChoices(provider, current, catalog)
			if err != nil || len(choices) == 0 {
				form()
				return
			}
			choices = append(choices, paneChoice{id: pickSetupForm, title: "Setup…", subtitle: "provider, keys and launch defaults", icon: paneIconSetup})
			r.model.mu.Lock()
			asked := r.model == tab.model && r.ask(paneQuestion{title: "Model for this session", hint: "Filter " + provider + " models", choices: choices}, askModels, func(answer paneAnswer) {
				switch {
				case answer.given && answer.id == pickSetupForm:
					form()
				case answer.given && slices.Contains(r.tabs, tab):
					tab.model.mu.Lock()
					defer tab.model.mu.Unlock()
					r.runCommandOn(tab, "/set model "+answer.id)
				}
			})
			r.model.mu.Unlock()
			if !asked {
				form()
			}
		})
	})
}

// modelChoices is the rows of the models list: the models the catalog does
// not rule out for text and tools, as the setup form suggests them, each
// under the name /set takes.
func modelChoices(provider, current string, catalog llm.ModelCatalog) []paneChoice {
	var choices []paneChoice
	for _, info := range catalog.Models {
		base, effective := info.ModelCapabilities, info.EffectiveCapabilities("")
		if !textTools(base) && !textTools(effective) && (textToolsRuledOut(base) || textToolsRuledOut(effective)) {
			continue
		}
		choice := paneChoice{id: provider + "/" + info.ID, title: info.ID}
		if info.Name != "" && info.Name != info.ID {
			choice.subtitle = info.Name
		}
		if choice.id == current {
			choice.detail = "current"
		}
		choices = append(choices, choice)
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].id < choices[j].id })
	// One row is kept for the setup form.
	return choices[:min(len(choices), 1999)]
}
