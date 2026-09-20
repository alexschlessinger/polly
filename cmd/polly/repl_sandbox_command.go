package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

const sandboxCommandUsage = "usage: /sandbox [show] | storage | clean caches | reset environment | try [command] | allow read|write <path> | allow env NAME=VALUE | allow passenv NAME [--members] | forget <n|path|NAME|all>"

// replSandboxCommand implements /sandbox: show the workspace's sandbox
// profile, try a command to see what the sandbox denies it, allow an item
// into the profile, or forget items from it. A change is judged by the rules
// the profile loads under, applied to this session at once, and saved to the
// profile file; other open sessions pick it up when they next open. The
// model cannot run slash commands; /init can separately prepare owned storage.
func replSandboxCommand(ctx *replCommandContext, args []string) replCommandResult {
	var lines []string
	switch {
	case len(args) == 1 || len(args) == 2 && args[1] == "show":
		lines = sandboxProfileShow(ctx)
	case args[1] == "try":
		lines = sandboxTryCommand(ctx)
	case args[1] == "allow":
		lines = []string{sandboxProfileAllow(ctx, args[2:])}
	case args[1] == "forget":
		lines = []string{sandboxProfileForget(ctx, args[2:])}
	case args[1] == "storage" || args[1] == "clean" || args[1] == "reset":
		return replCommandResult{err: sandboxStorageCommand(ctx, args[1:])}
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
		return matchingWords([]string{"show", "storage", "clean", "reset", "try", "allow", "forget"}, prefix)
	case 2:
		switch fields[1] {
		case "clean":
			return matchingWords([]string{"caches"}, prefix)
		case "reset":
			return matchingWords([]string{"environment"}, prefix)
		case "allow":
			return matchingWords([]string{profileRead, profileWrite, profileEnv, profilePassEnv}, prefix)
		case "forget":
			words := []string{"all"}
			if profile, _ := sandboxProfileFor(ctx); profile != nil {
				for i := range profile.listed() {
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
// item numbered for /sandbox forget, the items allowed for this session
// alone after the file's, with the credentials each exposes and why an item
// does not apply.
func sandboxProfileShow(ctx *replCommandContext) []string {
	profile, why := sandboxProfileFor(ctx)
	if profile == nil {
		return []string{why}
	}
	if profile.readErr != nil {
		return []string{"sandbox profile unreadable: " + profile.readErr.Error()}
	}
	file := homeRelativePath(profile.ws.profile)
	listed := profile.listed()
	if len(listed) == 0 && len(profile.profile.Storage.Allocations) == 0 {
		return []string{fmt.Sprintf("no sandbox profile for this workspace (%s); add to it with /sandbox try <command> or /sandbox allow read|write|env|passenv", file)}
	}
	header := "sandbox profile · " + file
	switch {
	case profile.off != "":
		header += " · off this launch (" + profile.off + ")"
	case profile.applyErr != nil:
		header += " · not applied: the sandbox refused it: " + profile.applyErr.Error()
	}
	lines := []string{header}
	for _, a := range profile.profile.Storage.Allocations {
		status := "automatic write access"
		if a.Disabled {
			status = "forgotten; tracked for cleanup only"
		}
		lines = append(lines, fmt.Sprintf("  %s · %s · forget with /sandbox forget %s", a.Key(), status, a.Key()))
	}
	for i, item := range listed {
		line := fmt.Sprintf("  %d. %s", i+1, item)
		if item.Automatic {
			line += " · automatic"
		}
		if profile.sessionOnly(i) {
			line += " · this session only"
		}
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
	for _, item := range listed {
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
// /sandbox show, a path, a variable name, or all, whether the file holds
// them or this session alone.
func sandboxProfileForget(ctx *replCommandContext, args []string) string {
	profile, why := sandboxProfileFor(ctx)
	if profile == nil {
		return why
	}
	if len(args) != 1 {
		return "usage: /sandbox forget <n|path|NAME|all>"
	}
	items := profile.listed()
	var forget []sandboxProfileItem
	selector := args[0]
	storageSelector := selector == "all" || slices.ContainsFunc(profile.profile.Storage.Allocations, func(a envstorage.Allocation) bool { return a.Key() == selector })
	if storageSelector && len(profile.profile.Storage.Allocations) > 0 {
		err := profile.updateProfile(ctx.state.toolRegistry, func(file *sandboxProfile) error {
			for i, a := range file.Storage.Allocations {
				if selector == "all" || a.Key() == selector {
					file.Storage.Allocations[i].Disabled = true
				}
			}
			file.Items = slices.DeleteFunc(file.Items, func(item sandboxProfileItem) bool {
				return selector == "all" || item.Value == selector || strings.HasPrefix(item.Value, selector+"/")
			})
			return nil
		}, func(items []sandboxProfileItem) []sandboxProfileItem {
			return slices.DeleteFunc(items, func(item sandboxProfileItem) bool {
				return selector == "all" || item.Value == selector || strings.HasPrefix(item.Value, selector+"/")
			})
		}, true)
		if err != nil {
			return "sandbox profile: forget failed: " + err.Error()
		}
		ctx.notifySandboxChanged()
		return "sandbox profile: forgot " + selector + "; owned storage remains tracked for cleanup"
	}
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
	gone := func(item sandboxProfileItem) bool {
		return slices.ContainsFunc(forget, func(g sandboxProfileItem) bool { return sameSandboxProfileItem(g, item) })
	}
	drop := func(items []sandboxProfileItem) []sandboxProfileItem { return slices.DeleteFunc(items, gone) }
	var fromFile, fromSession func([]sandboxProfileItem) []sandboxProfileItem
	if n, err := strconv.Atoi(selector); err == nil {
		if profile.sessionOnly(n - 1) {
			fromSession = drop
		} else {
			fromFile = drop
		}
	} else {
		if slices.ContainsFunc(profile.profile.Items, gone) {
			fromFile = drop
		}
		if slices.ContainsFunc(profile.session, gone) {
			fromSession = drop
		}
	}
	if err := profile.update(ctx.state.toolRegistry, fromFile, fromSession); err != nil {
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
			return sandboxProfileItem{}, errors.New("usage: /sandbox allow env NAME=VALUE, with a VALUE under @workspace, @cache, @state or @config")
		}
		value = profileEnvValue(ws, value)
		_, _, managedErr := ws.storage.Active().Lookup(value)
		return sandboxProfileItem{Kind: kind, Name: name, Value: value, Managed: managedErr == nil}, nil
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

// change applies edit to the profile file's items and returns the
// judgement of changed, when edit kept it (see update).
func (s *sandboxProfileState) change(registry *tools.ToolRegistry, edit func([]sandboxProfileItem) []sandboxProfileItem, changed sandboxProfileItem) (profileItemState, error) {
	if err := s.update(registry, edit, func(items []sandboxProfileItem) []sandboxProfileItem {
		return slices.DeleteFunc(items, func(item sandboxProfileItem) bool { return sameSandboxProfileItem(item, changed) })
	}); err != nil {
		return profileItemState{}, err
	}
	return s.stateOf(changed), nil
}

// update applies editFile to the profile file's items as the file is now,
// which another session may have changed since this one read it, and
// editSession to the items this session holds alone, then applies the
// result to this session: stage the rebuilt layer, save the file, then publish
// the layer. A refusal or persistence failure leaves the old tools intact.
// A nil edit leaves its items alone, and the file is
// written only when editFile is set. A session item identical to a saved
// item goes; a different value overrides the saved item for this session.
// With the profile off this launch nothing applies, and only the file changes.
func (s *sandboxProfileState) update(registry *tools.ToolRegistry, editFile, editSession func([]sandboxProfileItem) []sandboxProfileItem) error {
	return s.updateProfile(registry, func(file *sandboxProfile) error {
		if editFile != nil {
			file.Items = editFile(slices.Clone(file.Items))
		}
		return nil
	}, editSession, editFile != nil)
}

func (s *sandboxProfileState) updateProfile(registry *tools.ToolRegistry, edit func(*sandboxProfile) error, editSession func([]sandboxProfileItem) []sandboxProfileItem, save bool) error {
	return s.updateProfileContext(context.Background(), registry, edit, editSession, save)
}

func (s *sandboxProfileState) updateProfileContext(ctx context.Context, registry *tools.ToolRegistry, edit func(*sandboxProfile) error, editSession func([]sandboxProfileItem) []sandboxProfileItem, save bool) error {
	if registry != nil {
		release, err := registry.TryEnvironmentUse()
		if err != nil {
			return err
		}
		defer release()
	}
	if s.readErr != nil {
		return fmt.Errorf("the profile could not be read: %w", s.readErr)
	}
	unlock, err := envstorage.LockContext(ctx, filepath.Join(filepath.Dir(s.ws.profile), "profile.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	file := s.profile
	if save {
		current, err := readSandboxProfile(s.ws.profile)
		if err != nil {
			return err
		}
		file = current
		file.Workspace = s.ws.name()
	}
	file.Items = slices.Clone(file.Items)
	file.Storage = file.Storage.Clone()
	if edit != nil {
		if err := edit(&file); err != nil {
			return err
		}
	}
	session := slices.Clone(s.session)
	if editSession != nil {
		session = editSession(session)
	}
	session = slices.DeleteFunc(session, func(item sandboxProfileItem) bool {
		return slices.ContainsFunc(file.Items, func(saved sandboxProfileItem) bool {
			return sameSandboxProfileItem(saved, item) && saved.Value == item.Value && saved.Members == item.Members && saved.Credential == item.Credential && saved.Origin == item.Origin && saved.Automatic == item.Automatic && saved.Managed == item.Managed
		})
	})
	var base sandbox.Config
	active := false
	if registry != nil {
		var err error
		if base, active, err = registry.BaseSandboxPolicy(); err != nil {
			return err
		}
	}
	states, layer, err := s.profileLayer(file, base, session)
	if err != nil {
		return err
	}
	apply := s.off == "" && active
	var applied *tools.SandboxLayer
	if layerGrants(layer) {
		applied = &layer
	}
	commit := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if save {
			return writeSandboxProfile(s.ws.profile, file)
		}
		return nil
	}
	if apply {
		if _, err := registry.SetSandboxLayerAndCommit(sandboxProfileLayer, applied, commit); err != nil {
			return err
		}
	} else if err := commit(); err != nil {
		return err
	}
	if save {
		file.Version = sandboxProfileVersion
	}
	s.profile, s.session, s.judged, s.applyErr = file, session, states, nil
	s.ws.storage = file.Storage.Clone()
	if apply {
		s.applied = applied
	}
	return nil
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
