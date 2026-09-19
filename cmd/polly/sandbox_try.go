package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// /sandbox try runs a command as a sandbox trial (ToolRegistry.RunTrial) and
// proposes a profile item for each path the sandbox denied it. The user
// ticks what to allow, runs the command again with the ticked items, and
// allows them for the workspace or for this session alone. The rules of
// /sandbox allow judge every proposal, and every proposal starts unticked:
// the command is the workspace's own code, which can draw a denial of any
// path on purpose, so what a trial saw is evidence for the user to weigh,
// never a grant.

const (
	// sandboxTryTimeout bounds one trial.
	sandboxTryTimeout = 10 * time.Minute
	// sandboxTryGroupReads is how many denied reads inside one program's
	// directory make a single read proposal for the directory.
	sandboxTryGroupReads = 4
	// sandboxTryEvidence bounds the denied paths a proposal keeps to show.
	sandboxTryEvidence = 5
	// sandboxTryRows bounds the proposals one run lists.
	sandboxTryRows = 200
	// sandboxTryOffered bounds the failed commands /sandbox try offers.
	sandboxTryOffered = 20
)

// sandboxTry is one /sandbox try run: the command, the trials run so far, and
// what they proposed, with the user's ticks.
type sandboxTry struct {
	command  string
	registry *tools.ToolRegistry
	profile  *sandboxProfileState
	// run runs one trial: the registry's RunTrial, or a stand-in in tests.
	run func(ctx context.Context, command string, candidate sandbox.Config) (tools.TrialResult, error)
	// home is the canonical home directory and shared the directories in it
	// that programs of every kind keep their own directory in.
	home   string
	shared []sandbox.SharedHomeDir
	trials []sandboxTrial
	rows   []*sandboxProposal
	// unlisted counts the denials past sandboxTryRows.
	unlisted int
	// proposed marks the review of a model's proposal for /init, which the
	// user's answers name as sandbox setup.
	proposed bool
}

// name is what the run's messages call it.
func (t *sandboxTry) name() string {
	if t.proposed {
		return "sandbox setup"
	}
	return "sandbox try"
}

// sandboxTrial is what one trial saw.
type sandboxTrial struct {
	result  tools.TrialResult
	elapsed time.Duration
	// tried is how many ticked items the trial ran with.
	tried int
}

// sandboxProposal is one row of a review: an item that could be allowed,
// or a denial nothing can be proposed for, which says why.
type sandboxProposal struct {
	// kind is a profile item's kind, or "network". path is a read or write
	// item's, an env item's value resolved, or the denied address.
	kind string
	path string
	// name and value are an env or passenv item's, and members marks a
	// passenv item for swarm members too.
	name, value string
	members     bool
	// reason is the model's, for an item /init's model suggested: shown to
	// the user as the model's words, never trusted.
	reason string
	// refused says why the row cannot be allowed, and such a row is never
	// ticked: the rules of /sandbox allow refuse its item, or, with none
	// set, no item of a profile could cover the denial at all.
	refused    string
	none       bool
	credential bool
	// create marks a write whose directory does not exist. The sandbox drops
	// a grant of a missing path, so polly creates it, 0700, when the item is
	// tried or allowed.
	create bool
	ticked bool
	// The evidence: the denied paths the row covers, how many reports, the
	// process, the first and last trial that saw it, and whether the command
	// wrote there and the sandbox threw the writes away (Linux).
	denied              []string
	reports             int
	process             string
	firstSeen, lastSeen int
	discarded           bool
	// tried is the last trial that ran with the item ticked; cleared is set
	// when that trial saw no denial the item covers.
	tried   int
	cleared bool
}

// newSandboxTry starts a /sandbox try run in the session state holds.
func newSandboxTry(state *conversationState, command string) (*sandboxTry, error) {
	if err := sandboxTryReady(state); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	home = canonicalProfilePath(home)
	return &sandboxTry{
		command:  command,
		registry: state.toolRegistry,
		profile:  state.sandboxProfile,
		run:      state.toolRegistry.RunTrial,
		home:     home,
		shared:   sandbox.SharedHomeDirs(home),
	}, nil
}

// sandboxTryReady says why the session state holds cannot run trials, nil
// when it can.
func sandboxTryReady(state *conversationState) error {
	if state == nil || state.sandboxProfile == nil || state.toolRegistry == nil {
		return errors.New("the sandbox is off (--nosandbox), so there is nothing to try")
	}
	if state.sandboxProfile.readErr != nil {
		return fmt.Errorf("the workspace profile could not be read: %w", state.sandboxProfile.readErr)
	}
	if _, active, err := state.toolRegistry.BaseSandboxPolicy(); err != nil {
		return err
	} else if !active {
		return errors.New("the sandbox is off (--nosandbox), so there is nothing to try")
	}
	return nil
}

// programDir is the directory a grant for a denied path names, and whether
// a program would keep a directory of its own there. Outside the home
// directory it is the path itself. Inside it, it is the entry directly below
// the directory holding the path, the home directory or one of its shared
// directories (sandbox.SharedHomeDirs): a program's own directory, such as
// ~/.cache/go-build for a file deep inside it, or ~/.npm for one of npm's.
// A grant never names a shared directory whole. why says when there is
// none: the path is the home directory or a shared directory.
func (t *sandboxTry) programDir(path string) (dir string, programDirs bool, why string) {
	path = filepath.Clean(path)
	if t.home == "" || !sandbox.PathWithin(path, t.home) {
		return path, false, ""
	}
	if path == t.home {
		return "", false, "it is the home directory itself; the command's output may say what it wanted there"
	}
	holder := sandbox.SharedHomeDir{Path: t.home}
	for _, shared := range t.shared {
		if shared.Path == path {
			return "", false, "it holds every program's files, and a grant names one program's directory inside it"
		}
		if sandbox.PathWithin(path, shared.Path) && len(shared.Path) > len(holder.Path) {
			holder = shared
		}
	}
	rel, err := filepath.Rel(holder.Path, path)
	if err != nil {
		return path, false, ""
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return filepath.Join(holder.Path, first), holder.ProgramDirs, ""
}

// candidate is the policy the ticked rows add to a trial, and those rows.
func (t *sandboxTry) candidate() (sandbox.Config, []*sandboxProposal) {
	var cfg sandbox.Config
	var ticked []*sandboxProposal
	cache := false
	for _, p := range t.rows {
		if !p.ticked || p.refused != "" {
			continue
		}
		ticked = append(ticked, p)
		switch p.kind {
		case profileRead:
			cfg.ReadPaths = append(cfg.ReadPaths, p.path)
		case profileWrite:
			cfg.WritablePaths = append(cfg.WritablePaths, p.path)
		case profileEnv:
			cfg.Env = withEnv(cfg.Env, p.name, p.path)
			cache = cache || usesProfileCache(p.value)
		case profilePassEnv:
			cfg.PassEnv = append(cfg.PassEnv, p.name)
		}
	}
	if cache {
		cfg.WritablePaths = append(cfg.WritablePaths, t.profile.ws.cache)
	}
	return cfg, ticked
}

// ticked counts the ticked rows.
func (t *sandboxTry) ticked() int {
	_, ticked := t.candidate()
	return len(ticked)
}

// prepare makes ready the ticked rows a trial or an allow is about to use.
// It judges each again, since what a path names may have changed since the
// trial that proposed it, creates the missing directories of those that
// pass, and judges those once more for what they now name. A row the rules
// now refuse is unticked and returned.
func (t *sandboxTry) prepare() ([]*sandboxProposal, error) {
	judge, base := t.judge()
	_, ticked := t.candidate()
	var dropped []*sandboxProposal
	for _, p := range ticked {
		t.judgeRow(p, judge, base, sandbox.Denial{})
		if p.refused == "" && p.create {
			if err := os.MkdirAll(p.path, 0o700); err != nil {
				return dropped, fmt.Errorf("create %s: %w", homeRelativePath(p.path), err)
			}
			t.judgeRow(p, judge, base, sandbox.Denial{})
		}
		if p.refused == "" && p.kind == profileEnv && usesProfileCache(p.value) {
			// The sandbox drops a grant of a missing path, and the profile
			// creates the cache directory the same way when it loads.
			if err := os.MkdirAll(t.profile.ws.cache, 0o700); err != nil {
				return dropped, fmt.Errorf("create the workspace cache directory: %w", err)
			}
		}
		if p.refused != "" {
			p.ticked = false
			dropped = append(dropped, p)
		}
	}
	return dropped, nil
}

// judge returns the rules the rows are judged by and the base policy their
// grants would merge over.
func (t *sandboxTry) judge() (profileJudge, sandbox.Config) {
	base, _, _ := t.registry.BaseSandboxPolicy()
	return newProfileJudge(t.profile.ws), base
}

// trial runs the command with the ticked rows and records what the sandbox
// denied it. It returns the rows it unticked because the rules now refuse
// them. A command that fails is a result; an error means it could not run.
func (t *sandboxTry) trial(ctx context.Context) ([]*sandboxProposal, error) {
	dropped, err := t.prepare()
	if err != nil {
		return dropped, err
	}
	candidate, ticked := t.candidate()
	ctx, cancel := context.WithTimeout(ctx, sandboxTryTimeout)
	defer cancel()
	start := time.Now()
	result, err := t.run(ctx, t.command, candidate)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return dropped, fmt.Errorf("the trial ran past %s and was stopped", sandboxTryTimeout)
		}
		return dropped, err
	}
	n := len(t.trials) + 1
	t.trials = append(t.trials, sandboxTrial{result: result, elapsed: time.Since(start), tried: len(ticked)})
	for _, p := range ticked {
		// A trial clears a path's row by not denying it again; nothing it
		// sees speaks for a variable.
		p.tried, p.cleared = n, p.kind == profileRead || p.kind == profileWrite
	}
	t.record(n, result)
	return dropped, nil
}

// record takes in trial n's denials: each lands on the row for the grant
// that would cover it, a new row when none does yet. Reads keep their own
// path, since a program reads a file of configuration where it writes a
// whole directory, until several fall in one program's directory. A ticked
// row the trial saw again is not cleared.
func (t *sandboxTry) record(n int, result tools.TrialResult) {
	judge, base := t.judge()
	reads := make(map[string][]sandbox.Denial)
	var order []string
	for _, d := range result.Observation.Denials {
		kind, path, programDirs, none := t.target(d)
		if kind == profileRead && none == "" && path != filepath.Clean(d.Path) {
			if _, ok := reads[path]; !ok {
				order = append(order, path)
			}
			reads[path] = append(reads[path], d)
			continue
		}
		t.saw(n, kind, path, programDirs, none, d, judge, base)
	}
	for _, dir := range order {
		denials := reads[dir]
		distinct := make(map[string]bool)
		for _, d := range denials {
			distinct[filepath.Clean(d.Path)] = true
		}
		if len(distinct) < sandboxTryGroupReads && t.find(profileRead, dir) == nil {
			for _, d := range denials {
				t.saw(n, profileRead, filepath.Clean(d.Path), false, "", d, judge, base)
			}
			continue
		}
		for _, d := range denials {
			t.saw(n, profileRead, dir, false, "", d, judge, base)
		}
		// The reads the directory's row now covers go, unless ticked.
		t.rows = slices.DeleteFunc(t.rows, func(p *sandboxProposal) bool {
			return p.kind == profileRead && !p.ticked && p.path != dir && sandbox.PathWithin(p.path, dir)
		})
	}
}

// saw notes denial d of trial n on the row for kind and path, adding the
// row when there is none yet: judged, or with none, the reason no item can
// cover the denial.
func (t *sandboxTry) saw(n int, kind, path string, programDirs bool, none string, d sandbox.Denial, judge profileJudge, base sandbox.Config) {
	p := t.find(kind, path)
	if p == nil {
		if len(t.rows) >= sandboxTryRows {
			t.unlisted++
			return
		}
		p = &sandboxProposal{kind: kind, path: path, refused: none, none: none != ""}
		if none == "" {
			t.judgeRow(p, judge, base, d)
			if p.kind == profileWrite && p.create && !programDirs && filepath.Clean(d.Path) == path && !d.Directory && d.Process != "mkdir" {
				// Nothing tells whether the command meant a file or a
				// directory here, and outside the directories programs keep
				// their own in, a file is as likely.
				p.create = false
				p.refused = "it does not exist, and the trial cannot tell whether the command meant a file or a directory: create it, then try again"
			}
		}
		t.rows = append(t.rows, p)
	}
	if p.firstSeen == 0 {
		p.firstSeen = n
	}
	p.lastSeen = n
	p.reports += max(1, d.Count)
	p.discarded = p.discarded || d.Discarded
	if p.process == "" {
		p.process = d.Process
	}
	if clean := filepath.Clean(d.Path); !slices.Contains(p.denied, clean) && len(p.denied) < sandboxTryEvidence {
		p.denied = append(p.denied, clean)
	}
	p.cleared = false
}

func (t *sandboxTry) find(kind, path string) *sandboxProposal {
	for _, p := range t.rows {
		if p.kind == kind && p.path == path {
			return p
		}
	}
	return nil
}

// target is the row a denial belongs to: the kind of item, the path its
// grant would name (for reads, the program directory the caller groups
// them by), whether a program keeps a directory of its own there, and why
// no item can cover the denial, when none can.
func (t *sandboxTry) target(d sandbox.Denial) (kind, path string, programDirs bool, none string) {
	if d.Access == sandbox.AccessNetwork {
		if filepath.IsAbs(d.Path) {
			return "network", d.Path, false, "a profile grants no Unix sockets; the ssh preset passes the SSH agent's"
		}
		return "network", d.Path, false, "the --sandbox preset keeps the network off, and a profile does not turn it on"
	}
	kind = profileRead
	if d.Access == sandbox.AccessWrite {
		kind = profileWrite
	}
	if d.Cause == sandbox.CauseUnexplained {
		return kind, filepath.Clean(d.Path), false, "the policy polly knows allows it, so the platform refused it for a reason polly does not model"
	}
	dir, programDirs, why := t.programDir(d.Path)
	if why != "" {
		return kind, filepath.Clean(d.Path), false, why
	}
	return kind, dir, programDirs, ""
}

// judgeRow judges a row by the rules of /sandbox allow against base, and
// decides whether its path exists or polly creates it. d is the denial that
// proposed the row, the zero Denial when judging it again.
func (t *sandboxTry) judgeRow(p *sandboxProposal, judge profileJudge, base sandbox.Config, d sandbox.Denial) {
	paths := p.kind == profileRead || p.kind == profileWrite
	credential, err := judge.check(p.item())
	switch {
	case err != nil:
		p.refused = err.Error()
	case paths && sandbox.DeniedBy(base.DenyPaths, p.path):
		p.refused = "a denied path of the sandbox covers it"
	case base.DenyWrite && (p.kind == profileWrite || p.kind == profileEnv && usesProfileCache(p.value)):
		p.refused = profileWritesDenied
	}
	if p.refused != "" {
		return
	}
	p.credential = credential
	if !paths {
		return
	}
	info, err := os.Stat(p.path)
	create := p.create
	p.create = false
	switch {
	case err == nil:
		if p.kind == profileWrite && info.IsDir() {
			if err := judge.checkNewWrite(p.path); err != nil {
				p.refused = err.Error()
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		p.refused = err.Error()
	case create:
		p.create = true
	case p.kind == profileRead:
		p.refused = "it does not exist now"
	case !sandbox.PathWithin(p.path, t.home):
		p.refused = "it does not exist, and polly creates directories only in the home directory"
	case d.Discarded && !d.Directory && filepath.Clean(d.Path) == p.path:
		p.refused = "it does not exist, and the command wrote a file there, which polly does not create: create it, then try again"
	default:
		p.create = true
	}
}

// toggle ticks or unticks row i and says why when it cannot.
func (t *sandboxTry) toggle(i int) error {
	if i < 0 || i >= len(t.rows) {
		return fmt.Errorf("there is no row %d", i+1)
	}
	p := t.rows[i]
	if p.refused != "" {
		return fmt.Errorf("%s cannot be allowed: %s", p.label(), p.refused)
	}
	p.ticked = !p.ticked
	return nil
}

// tickReads ticks every read row that exposes no credential, which each
// need a tick of their own, and reports how many it ticked.
func (t *sandboxTry) tickReads() int {
	n := 0
	for _, p := range t.rows {
		if p.kind == profileRead && p.refused == "" && !p.credential && !p.ticked {
			p.ticked = true
			n++
		}
	}
	return n
}

// allow applies the ticked rows by the rules of /sandbox allow: saved to the
// profile file when save is set, else for this session alone. It returns
// the lines that report it.
func (t *sandboxTry) allow(save bool) ([]string, error) {
	if t.ticked() == 0 {
		return nil, errors.New("nothing is ticked")
	}
	if !save && t.profile.off != "" {
		return nil, fmt.Errorf("the workspace profile is off this launch (%s), so nothing applies to this session; save to the profile for later launches instead", t.profile.off)
	}
	dropped, err := t.prepare()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, p := range dropped {
		lines = append(lines, fmt.Sprintf("%s: %s refused: %s", t.name(), p.label(), p.refused))
	}
	var items []sandboxProfileItem
	_, ticked := t.candidate()
	for _, p := range ticked {
		item := p.item()
		if p.credential {
			item.Credential, item.Origin = true, t.profile.ws.origin
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return lines, nil
	}
	add := func(current []sandboxProfileItem) []sandboxProfileItem {
		for _, item := range items {
			if i := slices.IndexFunc(current, func(c sandboxProfileItem) bool { return sameSandboxProfileItem(c, item) }); i >= 0 {
				current[i] = item
			} else {
				current = append(current, item)
			}
		}
		return current
	}
	where := "for this session only"
	if save {
		err = t.profile.update(t.registry, add, nil)
		where = "and saved to the workspace profile"
	} else {
		err = t.profile.update(t.registry, nil, add)
	}
	if err != nil {
		return lines, err
	}
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.String()
	}
	lines = append(lines, fmt.Sprintf("sandbox profile: allowed %s %s", strings.Join(names, ", "), where))
	for _, item := range items {
		state := t.profile.stateOf(item)
		switch {
		case state.problem != "":
			lines = append(lines, fmt.Sprintf("  %s does not apply: %s", item, state.problem))
		case item.Credential && item.Kind == profilePassEnv:
			lines = append(lines, fmt.Sprintf("  %s is a credential: sandboxed commands%s see it while the workspace's origin stays %s", item, membersToo(item), originName(item.Origin)))
		case item.Credential:
			lines = append(lines, fmt.Sprintf("  %s is a credential: sandboxed commands read it while the workspace's origin stays %s", item, originName(item.Origin)))
		}
	}
	if save && t.profile.off != "" {
		lines = append(lines, "  it applies from the next launch: this one runs with "+t.profile.off)
	}
	return lines, nil
}

// label names a row the way /sandbox allow takes it.
func (p *sandboxProposal) label() string {
	return p.kind + " " + p.subject()
}

// subject is what a row's item names after its kind: the path, the variable
// and its value, or the variable passed.
func (p *sandboxProposal) subject() string {
	switch p.kind {
	case profileEnv:
		return p.name + "=" + p.value
	case profilePassEnv:
		if p.members {
			return p.name + " --members"
		}
		return p.name
	}
	return homeRelativePath(p.path)
}

// item is the profile item the row would allow.
func (p *sandboxProposal) item() sandboxProfileItem {
	switch p.kind {
	case profileEnv:
		return sandboxProfileItem{Kind: p.kind, Name: p.name, Value: p.value}
	case profilePassEnv:
		return sandboxProfileItem{Kind: p.kind, Name: p.name, Members: p.members}
	}
	return sandboxProfileItem{Kind: p.kind, Path: homeRelativePath(p.path)}
}

// badge is the short note a row carries after its path: what the last
// trial with it saw, and what allowing it would be.
func (p *sandboxProposal) badge() string {
	switch {
	case p.none:
		return "not proposed"
	case p.refused != "":
		return "refused"
	}
	var parts []string
	switch {
	case p.tried > 0 && p.tried == p.lastSeen:
		parts = append(parts, "still denied")
	case p.cleared:
		parts = append(parts, "✓ cleared")
	}
	switch {
	case p.credential:
		parts = append(parts, "credential")
	case p.create:
		parts = append(parts, "new directory")
	}
	return strings.Join(parts, " · ")
}

// details explains a row: what the trials saw, what allowing it would do,
// and why it cannot be allowed when it cannot. They are polly's words; the
// model's reason for a row it suggested is shown apart (reasonLine).
func (p *sandboxProposal) details() []string {
	var lines []string
	switch {
	case p.kind == profileEnv && p.refused == "":
		lines = append(lines, fmt.Sprintf("Sandboxed commands get %s set to %s.", p.name, homeRelativePath(p.path)))
	case p.kind == profilePassEnv && p.refused == "":
		if _, set := os.LookupEnv(p.name); !set {
			lines = append(lines, fmt.Sprintf("%s is not set in polly's environment now, so this passes nothing until it is.", p.name))
		}
	}
	switch {
	case p.reports == 0:
	case p.discarded:
		lines = append(lines, fmt.Sprintf("The command wrote %d %s here in trial %d; they succeeded into the sandbox's private home and were thrown away when it ended.", p.reports, pluralWord(p.reports, "entry", "entries"), p.lastSeen))
	default:
		seen := fmt.Sprintf("Denied %d× in trial %d", p.reports, p.lastSeen)
		if p.firstSeen != p.lastSeen {
			seen = fmt.Sprintf("Denied %d× in trials %d–%d", p.reports, p.firstSeen, p.lastSeen)
		}
		if p.process != "" {
			seen += " (" + p.process + ")"
		}
		if len(p.denied) > 1 || len(p.denied) == 1 && p.denied[0] != p.path {
			shown := make([]string, len(p.denied))
			for i, path := range p.denied {
				shown[i] = homeRelativePath(path)
			}
			seen += ": " + strings.Join(shown, ", ")
			if len(p.denied) == sandboxTryEvidence {
				seen += ", …"
			}
		}
		lines = append(lines, seen+".")
	}
	switch {
	case p.tried > 0 && p.tried == p.lastSeen:
		lines = append(lines, fmt.Sprintf("Still denied in trial %d, which ran with it allowed.", p.tried))
	case p.cleared:
		lines = append(lines, fmt.Sprintf("Not denied in trial %d, which ran with it allowed.", p.tried))
	}
	switch {
	case p.none:
		lines = append(lines, "Nothing to propose: "+p.refused+".")
	case p.refused != "":
		lines = append(lines, "Cannot be allowed: "+p.refused+".")
	case p.credential:
		lines = append(lines, "A credential: sandboxed commands could read it and, with the network on, send it anywhere. It needs a tick of its own.")
	case p.kind == profileWrite:
		lines = append(lines, "Sandboxed commands could change anything kept here, including what the host later runs from it.")
	}
	if p.create {
		lines = append(lines, "It does not exist: polly creates it (mode 0700) when you try or allow it.")
	}
	return lines
}

// reasonLine is the model's reason for a row it suggested, "" for others.
func (p *sandboxProposal) reasonLine() string {
	if p.reason == "" {
		return ""
	}
	return "The model's reason: " + p.reason
}

// summary is a trial's one-line result.
func (t *sandboxTrial) summary(n int) string {
	line := fmt.Sprintf("trial %d · exit %d · %s", n, t.result.ExitCode, t.elapsed.Round(100*time.Millisecond))
	if t.tried > 0 {
		line += fmt.Sprintf(" · with %d %s", t.tried, pluralWord(t.tried, "item", "items"))
	}
	denied := len(t.result.Observation.Denials)
	return line + fmt.Sprintf(" · %d %s", denied, pluralWord(denied, "denial", "denials"))
}

// notes are what a trial's observation says about itself: why it saw
// nothing, what it cannot see, and when it saw more than it lists.
func (t *sandboxTry) notes() []string {
	if len(t.trials) == 0 {
		return nil
	}
	obs := t.trials[len(t.trials)-1].result.Observation
	var notes []string
	if obs.Incomplete != "" {
		notes = append(notes, "Denials were not all seen: "+obs.Incomplete+".")
	}
	if obs.Limit != "" {
		notes = append(notes, obs.Limit+".")
	}
	if obs.Truncated || t.unlisted > 0 {
		notes = append(notes, "The command drew more denials than are listed.")
	}
	return notes
}

// recentFailedCommands lists the session's bash commands that exited with a
// failure, the latest first, each once, at most sandboxTryOffered.
func recentFailedCommands(ctx context.Context, state *conversationState) ([]string, error) {
	if state == nil || state.session == nil {
		return nil, nil
	}
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return nil, err
	}
	commands := make(map[string]string)
	var failed []string
	for _, msg := range history {
		for _, call := range msg.ToolCalls {
			if call.Name != "bash" {
				continue
			}
			var args struct {
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(call.Arguments), &args) == nil && strings.TrimSpace(args.Command) != "" {
				commands[call.ID] = args.Command
			}
		}
		if msg.Role == messages.MessageRoleTool {
			if command, ok := commands[msg.ToolCallID]; ok && exitCodeFromResult(msg) > 0 {
				failed = append(failed, command)
			}
		}
	}
	var offered []string
	for i := len(failed) - 1; i >= 0 && len(offered) < sandboxTryOffered; i-- {
		if !slices.Contains(offered, failed[i]) {
			offered = append(offered, failed[i])
		}
	}
	return offered, nil
}
