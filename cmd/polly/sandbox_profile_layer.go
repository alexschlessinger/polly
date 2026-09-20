package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// sandboxProfileState is one session's view of its workspace's sandbox
// profile: what the file holds, and what of it applies.
type sandboxProfileState struct {
	ws sandboxWorkspace
	// off says why the profile does not apply this launch, "" when it does.
	off string
	// profile is the file as last read or written. readErr says why the
	// workspace or the file could not be read; nothing applies then, and
	// /sandbox leaves the file alone.
	profile sandboxProfile
	readErr error
	// session holds the items /sandbox try allowed for this session alone:
	// judged and applied with the file's, never written to it.
	session []sandboxProfileItem
	// judged is the last judgement of the listed items, in order, and
	// applyErr why the sandbox refused the layer they made, when it did.
	judged   []profileItemState
	applyErr error
	// applied is the layer the session's registry holds, nil for none.
	applied *tools.SandboxLayer
	lease   *envstorage.Lease
}

func (s *sandboxProfileState) Close() error {
	if s == nil {
		return nil
	}
	return s.lease.Close()
}

func (s *sandboxProfileState) ensureLease() error {
	if s.lease != nil {
		return nil
	}
	lease, err := envstorage.OpenLease(filepath.Join(filepath.Dir(s.ws.profile), "environment.lock"))
	if err != nil {
		return err
	}
	s.lease = lease
	return nil
}

func (s *sandboxProfileState) profileLayer(file sandboxProfile, base sandbox.Config, session []sandboxProfileItem) ([]profileItemState, tools.SandboxLayer, error) {
	ws := s.ws
	ws.storage = file.Storage
	storage := file.Storage.Active()
	if len(storage.Allocations) > 0 && s.off == "" && !base.DenyWrite {
		for _, a := range storage.Allocations {
			p := ws.storageRoots().Path(a)
			if sandbox.DeniedBy(base.DenyPaths, p) || sandbox.DeniedBy(base.DenyWritePaths, p) {
				return nil, tools.SandboxLayer{}, fmt.Errorf("managed allocation %s is denied", a.Key())
			}
		}
		if err := s.ensureLease(); err != nil {
			return nil, tools.SandboxLayer{}, err
		}
		if err := ws.storageRoots().Ensure(storage); err != nil {
			return nil, tools.SandboxLayer{}, err
		}
		if file.ConfigurationCheckout != "" && file.ConfigurationCheckout != ws.checkoutKey {
			source := ws
			source.checkoutKey = file.ConfigurationCheckout
			for _, a := range storage.Allocations {
				if a.Kind == "config" {
					if err := envstorage.CopyConfig(source.storageRoots(), a, ws.storageRoots()); err != nil {
						return nil, tools.SandboxLayer{}, fmt.Errorf("seed configuration: %w", err)
					}
				}
			}
		}
	}
	states, layer := judgeSandboxProfile(ws, file.Items, base, session...)
	if len(storage.Allocations) > 0 && s.off == "" && !base.DenyWrite {
		for _, a := range storage.Allocations {
			layer.Config.WritablePaths = append(layer.Config.WritablePaths, ws.storageRoots().Path(a))
		}
		env := map[string]string{}
		for i, item := range append(slices.Clone(file.Items), session...) {
			if item.managed() && item.Kind == profileEnv && states[i].problem == "" {
				env[item.Name] = item.Value
			}
		}
		layer.Environment = &tools.SandboxEnvironment{Storage: storage, Roots: ws.storageRoots(), Env: env,
			CheckoutCacheRoot: filepath.Join(ws.managedCacheRoot(), "managed", "checkouts"), CheckoutDataRoot: ws.data}
	}
	return states, layer, nil
}

// profileItemState is the judgement of one item.
type profileItemState struct {
	credential bool
	// problem says why the item does not apply, "" when it does.
	problem string
}

// Problems a startup notice need not repeat: one the item has in this
// session only, and a read the working directory already covers, which is
// redundant here but may not be in another worktree sharing the profile.
const (
	profileWritesDenied    = "the sandbox denies all writes"
	profileAlreadyReadable = "it is inside the workspace, which is already readable"
)

// listed is every item the session applies, in the order /sandbox show
// numbers them: the file's, then this session's own.
func (s *sandboxProfileState) listed() []sandboxProfileItem {
	return slices.Concat(s.profile.Items, s.session)
}

// sessionOnly reports whether the listed item at i is this session's own.
func (s *sandboxProfileState) sessionOnly(i int) bool {
	return i >= len(s.profile.Items)
}

// stateOf is the judgement of the listed item that grants what item does,
// the zero state when none does.
func (s *sandboxProfileState) stateOf(item sandboxProfileItem) profileItemState {
	listed := s.listed()
	for i := len(listed) - 1; i >= 0; i-- {
		if item.Kind != "" && sameSandboxProfileItem(listed[i], item) && i < len(s.judged) {
			return s.judged[i]
		}
	}
	return profileItemState{}
}

// openSandboxProfile finds the working directory's workspace and reads its
// profile. It never fails: a profile that cannot be read applies nothing and
// says why.
func openSandboxProfile(config *Config) *sandboxProfileState {
	state := &sandboxProfileState{}
	if config.NoSandboxProfile {
		state.off = "--nosandboxprofile"
	}
	wd, err := os.Getwd()
	if err == nil {
		state.ws, err = resolveSandboxWorkspace(wd)
	}
	if err != nil {
		state.readErr = err
		return state
	}
	state.profile, state.readErr = readSandboxProfile(state.ws.profile)
	return state
}

// apply judges the profile against base, the sandbox's prepared base policy,
// and returns the layer its items make when any applies. The layer must
// build under factory, the sandbox the registry uses, or the profile applies
// nothing: dropping a profile only narrows the policy, so it never stops a
// start.
func (s *sandboxProfileState) apply(base sandbox.Config, factory func(sandbox.Config) (sandbox.Sandbox, error)) (tools.SandboxLayer, bool) {
	if s.readErr != nil {
		return tools.SandboxLayer{}, false
	}
	states, layer, err := s.profileLayer(s.profile, base, s.session)
	if err != nil {
		s.applyErr = err
		return tools.SandboxLayer{}, false
	}
	s.judged = states
	s.ws.storage = s.profile.Storage.Clone()
	if s.off != "" || !layerGrants(layer) {
		return tools.SandboxLayer{}, false
	}
	cfg, err := sandbox.PrepareConfig(base.Merge(layer.Config))
	if err == nil {
		_, err = factory(cfg)
	}
	if err != nil {
		s.applyErr = err
		return tools.SandboxLayer{}, false
	}
	s.applied = &layer
	return layer, true
}

// judgeSandboxProfile judges every item against base and builds the layer
// from those that apply. Session items override matching saved items. Every
// item reaches swarm members but a passenv item not marked for them; the
// workspace's cache directory is created, and granted, when an env item
// points into it.
func judgeSandboxProfile(ws sandboxWorkspace, saved []sandboxProfileItem, base sandbox.Config, session ...sandboxProfileItem) ([]profileItemState, tools.SandboxLayer) {
	items := slices.Concat(saved, session)
	judge := newProfileJudge(ws)
	var cacheErr error
	for _, item := range items {
		if item.Kind == profileEnv && !item.managed() && usesProfileCache(item.Value) {
			cacheErr = os.MkdirAll(ws.cache, 0o700)
			break
		}
	}
	states := make([]profileItemState, len(items))
	var cfg, members sandbox.Config
	cache := false
	for i, item := range items {
		if i < len(saved) && slices.ContainsFunc(session, func(override sandboxProfileItem) bool { return sameSandboxProfileItem(item, override) }) {
			states[i].problem = "overridden for this session"
			continue
		}
		state := judge.judge(item, base)
		if _, explicit := base.Env[item.Name]; item.Automatic && explicit {
			state.problem = "overridden by an explicit base setting"
		}
		if state.problem == "" && cacheErr != nil && item.Kind == profileEnv && usesProfileCache(item.Value) {
			state.problem = fmt.Sprintf("the workspace cache directory could not be created: %v", cacheErr)
		}
		states[i] = state
		if state.problem != "" {
			continue
		}
		switch item.Kind {
		case profileRead:
			path := expandHomePath(item.Path)
			cfg.ReadPaths = append(cfg.ReadPaths, path)
			members.ReadPaths = append(members.ReadPaths, path)
		case profileWrite:
			path := expandHomePath(item.Path)
			cfg.WritablePaths = append(cfg.WritablePaths, path)
			members.WritablePaths = append(members.WritablePaths, path)
		case profileEnv:
			value, _ := judge.itemEnvPath(item)
			cfg.Env = withEnv(cfg.Env, item.Name, value)
			if !item.managed() {
				members.Env = withEnv(members.Env, item.Name, value)
			}
			cache = cache || !item.managed() && usesProfileCache(item.Value)
		case profilePassEnv:
			cfg.PassEnv = append(cfg.PassEnv, item.Name)
			if item.Members {
				members.PassEnv = append(members.PassEnv, item.Name)
			}
		}
	}
	if cache {
		cfg.WritablePaths = append(cfg.WritablePaths, ws.cache)
		members.WritablePaths = append(members.WritablePaths, ws.cache)
	}
	return states, tools.SandboxLayer{Config: cfg, Members: members}
}

// judge judges one item against base: its own checks, then whether a
// credential item was allowed as one and for the workspace's origin, the
// base's denied paths, which win over an item, a base that denies every
// write, and whether a path still exists, since the sandbox drops a grant of
// a missing one.
func (j profileJudge) judge(item sandboxProfileItem, base sandbox.Config) profileItemState {
	credential, err := j.check(item)
	state := profileItemState{credential: credential}
	switch {
	case err != nil:
		state.problem = err.Error()
	case credential && !item.Credential:
		state.problem = "it exposes a credential and was not allowed as one; allow it again to keep it"
	case credential && item.Origin != j.ws.origin:
		state.problem = fmt.Sprintf("it was allowed for the origin %s and the workspace's origin is now %s; allow it again to keep it", originName(item.Origin), originName(j.ws.origin))
	case (item.Kind == profileRead || item.Kind == profileWrite) && sandbox.DeniedBy(base.DenyPaths, expandHomePath(item.Path)):
		state.problem = "a denied path of the sandbox covers it"
	case base.DenyWrite && (item.Kind == profileWrite || item.Kind == profileEnv && (item.Automatic || usesProfileCache(item.Value) || strings.HasPrefix(item.Value, "@state/") || strings.HasPrefix(item.Value, "@config/"))):
		state.problem = profileWritesDenied
	case item.Kind == profileRead || item.Kind == profileWrite:
		if _, err := os.Stat(expandHomePath(item.Path)); err != nil {
			state.problem = "it does not exist"
		}
	}
	return state
}

func originName(origin string) string {
	if origin == "" {
		return "none"
	}
	return origin
}

// usesProfileCache reports whether an env value points into the workspace's
// cache directory.
func usesProfileCache(value string) bool {
	return value == profileCacheVar || strings.HasPrefix(value, profileCacheVar+"/")
}

func withEnv(env map[string]string, name, value string) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	env[name] = value
	return env
}

// layerGrants reports whether a layer holds anything.
func layerGrants(layer tools.SandboxLayer) bool {
	cfg := layer.Config
	return len(cfg.ReadPaths)+len(cfg.WritablePaths)+len(cfg.Env)+len(cfg.PassEnv) > 0
}

// notices are the startup warnings about the profile: why none of it
// applies, or which items do not.
func (s *sandboxProfileState) notices() []string {
	switch {
	case s == nil || s.off != "":
		return nil
	case s.readErr != nil:
		return []string{"sandbox profile not applied: " + s.readErr.Error()}
	case s.applyErr != nil:
		return []string{"sandbox profile not applied: the sandbox refused it: " + s.applyErr.Error()}
	}
	var notices []string
	listed := s.listed()
	for i, state := range s.judged {
		if state.problem != "" && state.problem != profileWritesDenied && state.problem != profileAlreadyReadable {
			notices = append(notices, fmt.Sprintf("sandbox profile item %d (%s) not applied: %s", i+1, listed[i], state.problem))
		}
	}
	return notices
}

// summary is the profile's part of the sandbox posture.
func (s *sandboxProfileState) summary() string {
	switch {
	case s == nil:
		return ""
	case s.readErr != nil:
		return "profile unreadable"
	case len(s.listed()) == 0:
		return ""
	case s.off != "":
		return "profile off"
	case s.applyErr != nil:
		return "profile refused"
	}
	applied := 0
	for _, state := range s.judged {
		if state.problem == "" {
			applied++
		}
	}
	summary := fmt.Sprintf("profile: %d %s", applied, pluralWord(applied, "item", "items"))
	var notes []string
	if len(s.session) > 0 {
		notes = append(notes, fmt.Sprintf("%d this session only", len(s.session)))
	}
	if skipped := len(s.listed()) - applied; skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d not applied", skipped))
	}
	if len(notes) > 0 {
		summary += " (" + strings.Join(notes, ", ") + ")"
	}
	return summary
}

// grantedPaths lists the read and write paths the profile applies, spelled
// from the home directory, for the model's note on what it may reach.
func (s *sandboxProfileState) grantedPaths() (reads, writes []string) {
	if s == nil || s.readErr != nil || s.off != "" || s.applyErr != nil {
		return nil, nil
	}
	listed := s.listed()
	for i, state := range s.judged {
		if state.problem != "" {
			continue
		}
		switch item := listed[i]; item.Kind {
		case profileRead:
			reads = append(reads, homeRelativePath(expandHomePath(item.Path)))
		case profileWrite:
			writes = append(writes, homeRelativePath(expandHomePath(item.Path)))
		}
	}
	return reads, writes
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
