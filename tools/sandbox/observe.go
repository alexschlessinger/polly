package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// ErrDenialsUnobservable reports that Polly has no way to observe what a
// sandbox denies on this platform, or with this sandbox.
var ErrDenialsUnobservable = errors.New("sandbox denials cannot be observed here")

// Access is the kind of operation a sandbox denied.
type Access string

const (
	AccessRead    Access = "read"
	AccessWrite   Access = "write"
	AccessNetwork Access = "network"
)

// DenialCause is why a trial's policy refused an operation, as far as
// Polly's model of the policy can tell.
type DenialCause string

const (
	// CauseMasked: a denied path covers it, a credential path or a
	// configured deny path.
	CauseMasked DenialCause = "masked"
	// CausePrivate: a read inside the home directory, or another private
	// root, that no grant reaches.
	CausePrivate DenialCause = "private"
	// CauseNotWritable: a write outside every writable path, or inside a
	// read-only island of one.
	CauseNotWritable DenialCause = "not writable"
	// CauseNetwork: network access, a Unix socket included, that the policy
	// does not allow.
	CauseNetwork DenialCause = "network"
	// CauseUnexplained: the policy model allows it, so the platform refused
	// it for a reason Polly does not model.
	CauseUnexplained DenialCause = "unexplained"
)

// Denial is one path or address a sandbox refused a trial's command, merged
// across every report of it.
type Denial struct {
	Access Access
	// Path is the absolute path denied, or for network access the address
	// the platform names: a Unix socket's path or a remote endpoint.
	Path string
	// Operation is the platform's name for the first operation reported,
	// such as Seatbelt's "file-write-create".
	Operation string
	// Process names the process that was denied, where the platform says.
	Process string
	// Count is how many times the platform reported the denial, or for a
	// discarded write how many entries the command left under Path.
	Count int
	// Discarded marks a write that succeeded into the private home directory
	// of a Linux sandbox and was thrown away with it when the command ended:
	// the sandbox did not refuse it, but nothing written there persists.
	Discarded bool
	// Directory marks a discarded write whose Path the command created as a
	// directory. Where the platform reports a refused write, nothing says
	// whether the command meant a file or a directory, and it is false.
	Directory bool
	// Cause is why the policy refused it, set by ClassifyDenials.
	Cause DenialCause
}

// Observation is what a DenialObserver saw of one trial.
type Observation struct {
	// Denials are in the order they were first seen.
	Denials []Denial
	// Truncated reports that the command drew more distinct denials than
	// Denials keeps.
	Truncated bool
	// Incomplete says why this observation may be missing denials, "" when
	// nothing went wrong.
	Incomplete string
	// Limit says what the platform never observes, "" when it observes
	// every denial.
	Limit string
}

// DenialObserver watches the sandbox of one trial for what its policy
// refuses the command. Build the trial's sandbox from a config marked by
// Config, Start the observer on it, run the command Shell describes through
// it, then Finish, and Close when done.
//
// On macOS every deny rule of the trial's profile carries a random tag, and
// the observer reads the kernel's reports of them from the host's system
// log (docs/SANDBOX.md, "Observing denials"). On Linux, writes into the
// private home directory succeed and are discarded with it; the trial lists
// what the command left there before the sandbox ends. Reads of hidden paths
// are not observed there. Other platforms observe nothing.
//
// What a trial observes is evidence, not authority. The command runs with
// the trial policy's full reach, so it can provoke a denial, or avoid one,
// on purpose, and whatever acts on an observation must judge each grant it
// proposes on its own.
type DenialObserver struct {
	tag  string
	home string
	observerState
}

// NewDenialObserver returns an observer for one trial, or an error wrapping
// ErrDenialsUnobservable where the platform cannot observe denials.
func NewDenialObserver() (*DenialObserver, error) {
	home, err := privateHomeRoot()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("denial observer: %w", err)
	}
	o := &DenialObserver{tag: "polly-" + hex.EncodeToString(nonce), home: home}
	if err := o.initPlatform(); err != nil {
		return nil, err
	}
	return o, nil
}

// Config returns cfg marked so that a sandbox built from it, alone or merged
// over another config, reports its denials to o. The mark changes no
// decision of the policy. On macOS the command may also read the metadata of
// the home directory and its shared directories, their entries alone: a
// tool that stats ~/.cache before making ~/.cache/tool is otherwise denied
// the stat, and the denial names ~/.cache, where no grant belongs, instead
// of the directory the tool wanted. A grant of that directory lets its
// ancestors be stat'ed anyway, so the trial fails where the real command
// would.
func (o *DenialObserver) Config(cfg Config) Config {
	cfg.denialTag = o.tag
	stat := []string{o.home}
	for _, dir := range SharedHomeDirs(o.home) {
		stat = append(stat, dir.Path)
	}
	cfg.statPaths = concatStrings(cfg.statPaths, stat)
	return cfg
}

// TrialShell is how a trial runs its command under a DenialObserver: the
// script to run with bash -c, the variables it reads, which the caller
// passes to the target as explicit environment, and whether it writes a
// report to the descriptor Shell was given, to be handed to Finish.
type TrialShell struct {
	Script string
	Env    map[string]string
	Report bool
}

// maxDenials bounds the distinct denials one observation keeps.
const maxDenials = 2000

// denialSet merges reports into denials, one per access and path, in the
// order first seen.
type denialSet struct {
	index     map[denialKey]int
	denials   []Denial
	truncated bool
}

type denialKey struct {
	access Access
	path   string
}

func (s *denialSet) add(d Denial) {
	key := denialKey{d.Access, d.Path}
	if i, ok := s.index[key]; ok {
		s.denials[i].Count += d.Count
		return
	}
	if len(s.denials) >= maxDenials {
		s.truncated = true
		return
	}
	if s.index == nil {
		s.index = make(map[denialKey]int)
	}
	s.index[key] = len(s.denials)
	s.denials = append(s.denials, d)
}

// seatbeltReport matches the kernel's report of one Seatbelt denial,
// "Sandbox: <process>(<pid>) deny(<n>) <operation> <target>", after an
// optional "<n> duplicate report(s) for " prefix when the kernel coalesced
// repeats of it.
var seatbeltReport = regexp.MustCompile(`^(?:([0-9]+) duplicate reports? for )?Sandbox: (.+)\(([0-9]+)\) deny\(([0-9]+)\) ([a-z*-]+) (.+)$`)

// maxDenialTarget bounds the length of a reported path or address.
const maxDenialTarget = 4096

// parseSeatbeltReport parses one report message into a denial. The message
// must end in a line holding exactly tag, which the profile's deny rules
// carry; a report of another sandbox, of an operation that is not a file or
// network access, or of a target holding control characters is not one.
func parseSeatbeltReport(message, tag string) (Denial, bool) {
	body, last, ok := cutLastLine(message)
	if !ok || last != tag || strings.ContainsFunc(body, unicode.IsControl) {
		return Denial{}, false
	}
	m := seatbeltReport.FindStringSubmatch(body)
	if m == nil {
		return Denial{}, false
	}
	access, ok := seatbeltAccess(m[5])
	target := m[6]
	if !ok || len(target) > maxDenialTarget || access != AccessNetwork && !filepath.IsAbs(target) {
		return Denial{}, false
	}
	count, err := strconv.Atoi(m[4])
	if m[1] != "" {
		count, err = strconv.Atoi(m[1])
	}
	if err != nil || count < 1 {
		count = 1
	}
	return Denial{Access: access, Path: target, Operation: m[5], Process: m[2], Count: count}, true
}

func cutLastLine(message string) (body, last string, ok bool) {
	i := strings.LastIndexByte(message, '\n')
	if i < 0 {
		return "", "", false
	}
	return message[:i], message[i+1:], true
}

// seatbeltAccess maps a Seatbelt operation to the access it denies.
func seatbeltAccess(operation string) (Access, bool) {
	switch {
	case strings.HasPrefix(operation, "file-read"):
		return AccessRead, true
	case strings.HasPrefix(operation, "file-write"):
		return AccessWrite, true
	case strings.HasPrefix(operation, "network"):
		return AccessNetwork, true
	}
	return "", false
}

// seatbeltNoise reports whether a denial is one that processes under a
// Seatbelt profile draw whatever they are asked to do: dyld's DTrace helper,
// the controlling terminal a new session lacks, and CoreFoundation's text
// encoding and Apple preference files.
func seatbeltNoise(d Denial, home string) bool {
	switch d.Path {
	case "/dev/dtracehelper", "/dev/tty":
		return true
	case filepath.Join(home, ".CFUserTextEncoding"):
		return d.Access == AccessRead
	}
	if d.Access == AccessRead && filepath.Dir(d.Path) == filepath.Join(home, "Library", "Preferences") {
		name := filepath.Base(d.Path)
		return strings.HasPrefix(name, "com.apple.") || strings.HasPrefix(name, ".GlobalPreferences")
	}
	return false
}

// Records a Linux trial writes to its report descriptor between the
// listings of its private home: the listing before the command is followed
// by the directories left after it, then everything else left after it.
// Neither marker is an absolute path, so no listed entry can equal one.
const (
	homeReportDirs  = "polly-trial-dirs"
	homeReportFiles = "polly-trial-files"
)

// homeReportDepth bounds how deep below the home directory a Linux trial
// lists; the writes it reports are summarized at their shallowest new
// entries, so deeper ones add nothing.
const homeReportDepth = 8

// parseHomeReport reads a Linux trial's report of its private home, a list
// of NUL-terminated records, into the writes the command left there: every
// entry after the command that was not there before, summarized as one
// discarded write per new tree. Of a new directory that holds a single new
// directory and nothing else, the one inside is reported, so a tool's
// ~/.cache/tool comes back as that and not as ~/.cache. complete is false
// when the report stops before the listings after the command, because the
// command did not finish.
func parseHomeReport(report []byte, home string) (denials []Denial, complete bool) {
	records := strings.Split(string(report), "\x00")
	before := make(map[string]bool)
	dirs := make(map[string]bool)
	var entries []string
	section := 0
	for _, record := range records {
		switch {
		case record == homeReportDirs && section == 0:
			section = 1
			continue
		case record == homeReportFiles && section == 1:
			section = 2
			continue
		case section == 0:
			before[record] = true
			continue
		}
		if before[record] || !strings.HasPrefix(record, home+string(filepath.Separator)) || strings.ContainsFunc(record, unicode.IsControl) || len(record) > maxDenialTarget {
			continue
		}
		if section == 1 {
			dirs[record] = true
		}
		entries = append(entries, record)
	}
	return summarizeNewEntries(entries, dirs), section == 2
}

// summarizeNewEntries reports each tree of new entries as one discarded
// write at its root, or at the directory a chain of single new directories
// below the root leads to, with the tree's entry count.
func summarizeNewEntries(entries []string, dirs map[string]bool) []Denial {
	isNew := make(map[string]bool, len(entries))
	for _, entry := range entries {
		isNew[entry] = true
	}
	children := make(map[string][]string)
	var roots []string
	for _, entry := range entries {
		if parent := filepath.Dir(entry); isNew[parent] {
			children[parent] = append(children[parent], entry)
		} else {
			roots = append(roots, entry)
		}
	}
	var size func(string) int
	size = func(entry string) int {
		n := 1
		for _, child := range children[entry] {
			n += size(child)
		}
		return n
	}
	slices.Sort(roots)
	denials := make([]Denial, 0, len(roots))
	for _, root := range roots {
		path := root
		for len(children[path]) == 1 && dirs[children[path][0]] {
			path = children[path][0]
		}
		denials = append(denials, Denial{Access: AccessWrite, Path: path, Operation: "write", Count: size(root), Discarded: true, Directory: dirs[path]})
	}
	return denials
}

// ClassifyDenials sets the Cause of each denial from cfg, the trial's
// prepared policy.
func ClassifyDenials(cfg Config, denials []Denial) error {
	reads, err := CompileReadPolicy(cfg)
	if err != nil {
		return err
	}
	masks, err := compileReadPolicy(cfg, nil)
	if err != nil {
		return err
	}
	for i := range denials {
		d := &denials[i]
		switch {
		case d.Access == AccessNetwork:
			d.Cause = CauseNetwork
		case masks.Allowed(d.Path) != nil:
			d.Cause = CauseMasked
		case d.Access == AccessRead && reads.Allowed(d.Path) != nil:
			d.Cause = CausePrivate
		case d.Access == AccessWrite && WriteAllowed(cfg, d.Path) != nil:
			d.Cause = CauseNotWritable
		default:
			d.Cause = CauseUnexplained
		}
	}
	return nil
}
