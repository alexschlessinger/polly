package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

const sandboxCommandUsage = "usage: /sandbox [show] | allow read|write <path> | allow env NAME=VALUE | allow passenv NAME [--members] | forget <n|path|NAME|all>"

// replSandboxCommand implements /sandbox: show the workspace's sandbox
// profile, allow an item into it, or forget items from it. A change is judged
// by the rules the profile loads under, applied to this session at once, and
// saved to the profile file; other open sessions pick it up when they next
// open. The model cannot run slash commands, so only the user changes the
// profile.
func replSandboxCommand(ctx *replCommandContext, args []string) replCommandResult {
	var lines []string
	switch {
	case len(args) == 1 || len(args) == 2 && args[1] == "show":
		lines = sandboxProfileShow(ctx)
	case args[1] == "allow":
		lines = []string{sandboxProfileAllow(ctx, args[2:])}
	case args[1] == "forget":
		lines = []string{sandboxProfileForget(ctx, args[2:])}
	default:
		lines = []string{sandboxCommandUsage}
	}
	return replCommandResult{err: ctx.replyLines(lines)}
}

// sandboxCommandBusySafe lets /sandbox show the profile mid-turn while a
// change queues behind the turn, like /add-dir.
func sandboxCommandBusySafe(args []string) bool {
	return len(args) == 1 || len(args) == 2 && args[1] == "show"
}

func completeSandboxCommand(ctx *replCommandContext, fields []string, prefix string) []string {
	switch completionArgPos(fields, prefix) {
	case 1:
		return matchingWords([]string{"show", "allow", "forget"}, prefix)
	case 2:
		switch fields[1] {
		case "allow":
			return matchingWords([]string{profileRead, profileWrite, profileEnv, profilePassEnv}, prefix)
		case "forget":
			words := []string{"all"}
			if profile, _ := sandboxProfileFor(ctx); profile != nil {
				for i := range profile.profile.Items {
					words = append(words, strconv.Itoa(i+1))
				}
			}
			return matchingWords(words, prefix)
		}
	}
	return nil
}

// sandboxProfileFor returns the session's profile, or why it has none.
func sandboxProfileFor(ctx *replCommandContext) (*sandboxProfileState, string) {
	if ctx == nil || ctx.state == nil || ctx.state.session == nil {
		return nil, "no active session"
	}
	if ctx.state.sandboxProfile == nil {
		return nil, "the sandbox is off (--nosandbox), so no sandbox profile applies"
	}
	return ctx.state.sandboxProfile, ""
}

// sandboxProfileShow lists the profile as this session applies it: each
// item numbered for /sandbox forget, with the credentials it exposes and
// why an item does not apply.
func sandboxProfileShow(ctx *replCommandContext) []string {
	profile, why := sandboxProfileFor(ctx)
	if profile == nil {
		return []string{why}
	}
	if profile.readErr != nil {
		return []string{"sandbox profile unreadable: " + profile.readErr.Error()}
	}
	file := homeRelativePath(profile.ws.profile)
	if len(profile.profile.Items) == 0 {
		return []string{fmt.Sprintf("no sandbox profile for this workspace (%s); add to it with /sandbox allow read|write|env|passenv", file)}
	}
	header := "sandbox profile · " + file
	switch {
	case profile.off != "":
		header += " · off this launch (" + profile.off + ")"
	case profile.applyErr != nil:
		header += " · not applied: the sandbox refused it: " + profile.applyErr.Error()
	}
	lines := []string{header}
	for i, item := range profile.profile.Items {
		line := fmt.Sprintf("  %d. %s", i+1, item)
		if i < len(profile.judged) {
			state := profile.judged[i]
			if state.credential {
				line += " · credential"
			}
			if state.problem != "" {
				line += " · not applied: " + state.problem
			}
		}
		lines = append(lines, line)
	}
	for _, item := range profile.profile.Items {
		if item.Kind == profileEnv && usesProfileCache(item.Value) {
			lines = append(lines, "  @cache is "+homeRelativePath(profile.ws.cache))
			break
		}
	}
	return lines
}

// sandboxProfileAllow adds an item, or replaces the one granting the same
// path or variable.
func sandboxProfileAllow(ctx *replCommandContext, args []string) string {
	profile, why := sandboxProfileFor(ctx)
	if profile == nil {
		return why
	}
	item, err := parseSandboxProfileItem(profile.ws, args)
	if err != nil {
		return "sandbox profile: " + err.Error()
	}
	judge := newProfileJudge(profile.ws)
	credential, err := judge.check(item)
	if err == nil && item.Kind == profileWrite {
		err = judge.checkNewWrite(expandHomePath(item.Path))
	}
	if err != nil {
		return fmt.Sprintf("sandbox profile: %s refused: %v", item, err)
	}
	if credential {
		item.Credential, item.Origin = true, profile.ws.origin
	}
	state, err := profile.change(ctx.state.toolRegistry, func(items []sandboxProfileItem) []sandboxProfileItem {
		for i := range items {
			if sameSandboxProfileItem(items[i], item) {
				items[i] = item
				return items
			}
		}
		return append(items, item)
	}, item)
	if err != nil {
		return fmt.Sprintf("sandbox profile: allow %s failed: %v", item, err)
	}
	ctx.notifySandboxChanged()
	reply := "sandbox profile: allowed " + item.String()
	switch {
	case state.problem != "":
		reply += "; saved, but it does not apply: " + state.problem
	case profile.off != "":
		reply += "; saved for later launches (this one runs with " + profile.off + ")"
	case credential && item.Kind == profilePassEnv:
		reply += fmt.Sprintf("; a credential: sandboxed commands%s see it while the workspace's origin stays %s", membersToo(item), originName(item.Origin))
	case credential:
		reply += fmt.Sprintf("; a credential: sandboxed commands read it while the workspace's origin stays %s", originName(item.Origin))
	default:
		reply += "; it applies now"
	}
	if item.Kind == profilePassEnv {
		if _, set := os.LookupEnv(item.Name); !set {
			reply += fmt.Sprintf(" (%s is not set in polly's environment now)", item.Name)
		}
	}
	return reply
}

func membersToo(item sandboxProfileItem) string {
	if item.Members {
		return " and swarm members"
	}
	return ""
}

// sandboxProfileForget removes the items a selector names: a number from
// /sandbox show, a path, a variable name, or all.
func sandboxProfileForget(ctx *replCommandContext, args []string) string {
	profile, why := sandboxProfileFor(ctx)
	if profile == nil {
		return why
	}
	if len(args) != 1 {
		return "usage: /sandbox forget <n|path|NAME|all>"
	}
	items := profile.profile.Items
	var forget []sandboxProfileItem
	selector := args[0]
	if n, err := strconv.Atoi(selector); err == nil {
		if n < 1 || n > len(items) {
			return fmt.Sprintf("sandbox profile: no item %d; /sandbox show lists them", n)
		}
		forget = []sandboxProfileItem{items[n-1]}
	} else if selector == "all" {
		forget = slices.Clone(items)
	} else {
		path := filepath.Clean(expandHomePath(selector))
		for _, item := range items {
			if item.Name == selector || item.Path != "" && filepath.Clean(expandHomePath(item.Path)) == path {
				forget = append(forget, item)
			}
		}
	}
	if len(forget) == 0 {
		return fmt.Sprintf("sandbox profile: nothing matches %s; /sandbox show lists the items", selector)
	}
	_, err := profile.change(ctx.state.toolRegistry, func(items []sandboxProfileItem) []sandboxProfileItem {
		return slices.DeleteFunc(items, func(item sandboxProfileItem) bool {
			return slices.ContainsFunc(forget, func(gone sandboxProfileItem) bool { return sameSandboxProfileItem(gone, item) })
		})
	}, sandboxProfileItem{})
	if err != nil {
		return fmt.Sprintf("sandbox profile: forget failed: %v", err)
	}
	ctx.notifySandboxChanged()
	if selector == "all" {
		return fmt.Sprintf("sandbox profile: forgot all %d %s", len(forget), pluralWord(len(forget), "item", "items"))
	}
	names := make([]string, len(forget))
	for i, item := range forget {
		names[i] = item.String()
	}
	return "sandbox profile: forgot " + strings.Join(names, ", ")
}

// parseSandboxProfileItem reads the item /sandbox allow names. A path is
// taken from the working directory with ~ expanded and must exist; the home
// directory is spelled ~ in the profile. An env value that names a path
// inside the workspace or its cache directory is stored as @workspace or
// @cache.
func parseSandboxProfileItem(ws sandboxWorkspace, args []string) (sandboxProfileItem, error) {
	if len(args) < 2 {
		return sandboxProfileItem{}, errors.New(sandboxCommandUsage)
	}
	kind, rest := args[0], strings.Join(args[1:], " ")
	switch kind {
	case profileRead, profileWrite:
		path := expandHomePath(rest)
		if !filepath.IsAbs(path) {
			path = filepath.Join(ws.dir, path)
		}
		path = filepath.Clean(path)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return sandboxProfileItem{}, fmt.Errorf("%s does not exist", homeRelativePath(path))
		} else if err != nil {
			return sandboxProfileItem{}, err
		}
		return sandboxProfileItem{Kind: kind, Path: homeRelativePath(path)}, nil
	case profileEnv:
		name, value, ok := strings.Cut(rest, "=")
		if !ok || name == "" || value == "" {
			return sandboxProfileItem{}, errors.New("usage: /sandbox allow env NAME=VALUE, with a VALUE under @workspace or @cache")
		}
		return sandboxProfileItem{Kind: kind, Name: name, Value: profileEnvValue(ws, value)}, nil
	case profilePassEnv:
		names := slices.DeleteFunc(slices.Clone(args[1:]), func(arg string) bool { return arg == "--members" })
		if len(names) != 1 {
			return sandboxProfileItem{}, errors.New("usage: /sandbox allow passenv NAME [--members]")
		}
		return sandboxProfileItem{Kind: kind, Name: names[0], Members: len(names) < len(args)-1}, nil
	}
	return sandboxProfileItem{}, errors.New(sandboxCommandUsage)
}

// profileEnvValue spells an env value the way the profile stores it: a path
// inside the workspace's cache directory or the workspace from @cache or
// @workspace, anything else as given for the judge to refuse.
func profileEnvValue(ws sandboxWorkspace, value string) string {
	if strings.HasPrefix(value, "@") {
		return value
	}
	path := expandHomePath(value)
	if !filepath.IsAbs(path) {
		path = filepath.Join(ws.dir, path)
	}
	for _, root := range []struct{ name, path string }{{profileCacheVar, ws.cache}, {profileWorkspaceVar, ws.dir}} {
		for _, spelling := range pathSpellings(path) {
			if !sandbox.PathWithin(spelling, root.path) {
				continue
			}
			rel, _ := filepath.Rel(root.path, spelling)
			if rel == "." {
				return root.name
			}
			return root.name + "/" + filepath.ToSlash(rel)
		}
	}
	return value
}

// change applies edit to the profile file as it is now, which another
// session may have changed since this one read it, and applies the result
// to this session: the layer first, so a change the sandbox refuses leaves
// the file as it was, then the file, whose failure puts the earlier layer
// back. With the profile off this launch only the file changes. It returns
// the judgement of changed, when edit kept it.
func (s *sandboxProfileState) change(registry *tools.ToolRegistry, edit func([]sandboxProfileItem) []sandboxProfileItem, changed sandboxProfileItem) (profileItemState, error) {
	if s.readErr != nil {
		return profileItemState{}, fmt.Errorf("the profile could not be read: %w", s.readErr)
	}
	current, err := readSandboxProfile(s.ws.profile)
	if err != nil {
		return profileItemState{}, err
	}
	next := sandboxProfile{Version: sandboxProfileVersion, Workspace: s.ws.name(), Items: edit(slices.Clone(current.Items))}
	var base sandbox.Config
	active := false
	if registry != nil {
		if base, active, err = registry.BaseSandboxPolicy(); err != nil {
			return profileItemState{}, err
		}
	}
	states, layer := judgeSandboxProfile(s.ws, next, base)
	apply := s.off == "" && active
	var applied *tools.SandboxLayer
	if layerGrants(layer) {
		applied = &layer
	}
	if apply {
		if _, err := registry.SetSandboxLayer(sandboxProfileLayer, applied); err != nil {
			return profileItemState{}, err
		}
	}
	if err := writeSandboxProfile(s.ws.profile, next); err != nil {
		if apply {
			_, _ = registry.SetSandboxLayer(sandboxProfileLayer, s.applied)
		}
		return profileItemState{}, err
	}
	s.profile, s.judged, s.applyErr = next, states, nil
	if apply {
		s.applied = applied
	}
	for i, item := range next.Items {
		if changed.Kind != "" && sameSandboxProfileItem(item, changed) {
			return states[i], nil
		}
	}
	return profileItemState{}, nil
}

// name is what a profile's workspace field records: the repository's common
// Git directory, or the working directory outside Git.
func (ws sandboxWorkspace) name() string {
	if ws.commonDir != "" {
		return ws.commonDir
	}
	return ws.dir
}

// notifySandboxChanged lets the interactive REPL refresh what shows the
// sandbox posture after /sandbox changed the profile.
func (ctx *replCommandContext) notifySandboxChanged() {
	if ctx != nil && ctx.sandboxChanged != nil {
		ctx.sandboxChanged()
	}
}
