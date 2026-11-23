package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

const missingPlanDraftRevision = "missing"

// A backup still has the displaced inode, not just a snapshot of its bytes.
// An editor can write to it arbitrarily late; only the user can retire it.
const maxPlanDraftBackups = 8

// The hex form keeps drafts created by older versions editable on resume.
var planDraftName = regexp.MustCompile(`^(?:[a-z]+-[a-z]+(?:-[0-9]+)?|[a-f0-9]{32})\.md$`)
var planDraftBackupSuffix = regexp.MustCompile(`^(?:([a-f0-9]{64})-)?[a-f0-9]{32}$`)
var planFileMu sync.Mutex

// openPlanDir only creates the two known directory components, relative to
// pinned parents. A symlinked plans or .pollytool must never redirect even a
// temporary file into the project. Resolve the user's home once, not the slots.
func openPlanDir(create bool) (*os.Root, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("plan storage requires an absolute home directory")
	}
	// Refuse a runtime slot inside the current project (e.g. a session
	// rooted at HOME or HOME supplied underneath a checkout).
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		return nil, err
	}
	if sandbox.PathWithin(filepath.Join(home, userConfigDirName, "plans"), wd) {
		return nil, fmt.Errorf("plan storage would be inside the workspace; choose a narrower project directory")
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{userConfigDirName, "plans"} {
		if create {
			if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				root.Close()
				return nil, err
			}
		}
		info, err := root.Lstat(name)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !info.IsDir() {
			root.Close()
			return nil, fmt.Errorf("unsafe plan storage directory %s", name)
		}
		// Retained editor files keep their original mode, so require a private
		// plans directory. Windows inherits the profile ACL; its mode is synthetic.
		unsafePerms := os.FileMode(0022)
		if name == "plans" {
			unsafePerms = 0077
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&unsafePerms != 0 {
			root.Close()
			return nil, fmt.Errorf("unsafe permissions on plan storage directory %q; restrict access (mode 0700)", filepath.Join(root.Name(), name))
		}
		child, err := root.OpenRoot(name)
		root.Close()
		if err != nil {
			return nil, err
		}
		actual, err := child.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			child.Close()
			return nil, fmt.Errorf("plan storage directory changed during open")
		}
		root = child
	}
	return root, nil
}

func newPlanDraftPath() (string, error) {
	root, err := openPlanDir(true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	// Reserve each candidate atomically; concurrent sessions can't claim the
	// same name or overwrite an existing draft.
	name, err := generateContextName(func(candidate string) (bool, error) {
		f, err := root.OpenFile(candidate+".md", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, f.Close()
	})
	if err != nil {
		return "", err
	}
	return filepath.Join(root.Name(), name+".md"), nil
}

func openPlanDraftDir(path string) (*os.Root, string, error) {
	root, err := openPlanDir(false)
	if err != nil {
		return nil, "", err
	}
	name := filepath.Base(path)
	if !planDraftName.MatchString(name) || path != filepath.Join(root.Name(), name) {
		root.Close()
		return nil, "", fmt.Errorf("not a managed plan draft: %q", path)
	}
	return root, name, nil
}

// Open without following links and verify the entry in the pinned directory.
// In particular, never read bytes through an unrecognized hard link.
func readDraftEntry(root *os.Root, name string) (string, string, error) {
	f, err := openRegularDraft(root, name)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	if !singleLinkDraft(f, info) {
		return "", "", fmt.Errorf("plan draft %q must not be hard linked; close the editor and restore a single-link file (an interrupted older publication can leave another name starting with .new-)", filepath.Join(root.Name(), name))
	}
	at, err := root.Lstat(name)
	if err != nil {
		return "", "", err
	}
	if !at.Mode().IsRegular() || !os.SameFile(info, at) {
		return "", "", fmt.Errorf("plan draft changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, sessions.MaxPlanBytes+1))
	if err != nil {
		return "", "", err
	}
	at, err = root.Lstat(name)
	if err != nil {
		return "", "", err
	}
	if !os.SameFile(info, at) || info.Size() != at.Size() || !info.ModTime().Equal(at.ModTime()) || !singleLinkDraft(f, at) {
		return "", "", fmt.Errorf("plan draft changed while reading; finish the editor save and try again")
	}
	text := string(data)
	if err := validDraftText(text); err != nil {
		return "", "", err
	}
	return text, sessions.PlanRevision(text), nil
}

func validDraftText(text string) error {
	if len(text) > sessions.MaxPlanBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return fmt.Errorf("draft must be UTF-8 text of at most %d bytes", sessions.MaxPlanBytes)
	}
	return nil
}

func pendingDraftOriginal(name string) string { return name + ".previous" }
func stagedPlanDraft(name string) string      { return ".new-" + name }

// A fixed pending name records which original was displaced, even if there
// are older backups. A completed backup records the expected digest so a
// later operation can report writes made to that inode by a still-open editor.
type draftBackup struct {
	name, revision string
}

func draftBackups(root *os.Root, name string) ([]draftBackup, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	prefix := name + ".previous-"
	var backups []draftBackup
	for {
		names, err := dir.Readdirnames(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		for _, entry := range names {
			if suffix, ok := strings.CutPrefix(entry, prefix); ok {
				if parts := planDraftBackupSuffix.FindStringSubmatch(suffix); parts != nil {
					backups = append(backups, draftBackup{entry, parts[1]})
					if len(backups) > maxPlanDraftBackups { // only legacy/external files can exceed the cap
						return backups, nil
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return backups, nil
		}
	}
}

func draftRecoveryError(root *os.Root, name string, entries []string, cause error) error {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, fmt.Sprintf("%q", filepath.Join(root.Name(), entry)))
	}
	slices.Sort(paths)
	return fmt.Errorf("plan draft %q needs recovery. Retained: %s. Close the editor and other Polly instances using this draft. Restore a single-link regular original if the draft is missing, or compare and merge saved edits into the existing draft. Move reviewed backups out of the plans directory before retrying (remove only a redundant link if two names refer to the same file). Reason: %w", filepath.Join(root.Name(), name), strings.Join(paths, ", "), cause)
}

func backupNames(backups []draftBackup) []string {
	names := make([]string, 0, len(backups))
	for _, backup := range backups {
		names = append(names, backup.name)
	}
	return names
}

func checkDraftBackups(root *os.Root, name string, backups []draftBackup) error {
	for _, backup := range backups {
		if backup.revision == "" {
			return draftRecoveryError(root, name, []string{backup.name}, errors.New("previous update was interrupted or conflicted"))
		}
		_, rev, err := readDraftEntry(root, backup.name)
		if err != nil {
			return draftRecoveryError(root, name, []string{backup.name}, err)
		}
		if rev != backup.revision {
			return draftRecoveryError(root, name, []string{backup.name}, errors.New("a retained original changed, possibly from an editor still holding it open"))
		}
	}
	return nil
}

func readPlanDraft(path string) (string, string, error) {
	planFileMu.Lock()
	defer planFileMu.Unlock()
	root, name, err := openPlanDraftDir(path)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	backups, err := draftBackups(root, name)
	if err != nil {
		return "", "", err
	}
	pending := pendingDraftOriginal(name)
	if _, err := root.Lstat(pending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	} else if errors.Is(err, os.ErrNotExist) {
		pending = ""
	}
	if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		// Only a pending original or the sole legacy backup is unambiguous.
		// Never guess which historical version to resurrect after deletion.
		original := pending
		if original == "" && len(backups) == 1 && backups[0].revision == "" {
			original, backups = backups[0].name, nil
		}
		if original != "" {
			if _, _, err := readDraftEntry(root, original); err != nil {
				return "", "", draftRecoveryError(root, name, []string{original}, err)
			}
			if err := moveDraftEntry(root, original, name); err != nil {
				return "", "", draftRecoveryError(root, name, []string{original}, err)
			}
		} else if len(backups) > 0 {
			return "", "", draftRecoveryError(root, name, backupNames(backups), errors.New("draft is missing; no unambiguous original to restore"))
		}
	} else if err != nil {
		return "", "", err
	} else if pending != "" {
		return "", "", draftRecoveryError(root, name, []string{pending}, errors.New("both the draft and a pending original exist; neither was changed"))
	}
	if err := checkDraftBackups(root, name, backups); err != nil {
		return "", "", err
	}
	return readDraftEntry(root, name)
}

// The exclusive staging name serializes cooperating publishers for this draft
// and bounds abandoned temporary files. A leftover stage is never consumed or
// auto-deleted: it can belong to another live process.
func writePlanDraft(path, expectedRevision, text string) (string, error) {
	if err := validDraftText(text); err != nil {
		return "", err
	}
	planFileMu.Lock()
	defer planFileMu.Unlock()
	root, name, err := openPlanDraftDir(path)
	if err != nil {
		return "", err
	}
	defer root.Close()
	stage, pending := stagedPlanDraft(name), pendingDraftOriginal(name)
	f, err := root.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("draft publication still active or interrupted; after closing any other Polly writing this draft, inspect and move the staged file %q out of the plans directory, then read the draft again", filepath.Join(root.Name(), stage))
	}
	if err != nil {
		return "", err
	}
	stageInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", err
	}
	staged := true
	defer func() {
		_ = f.Close()
		if staged {
			if at, err := root.Lstat(stage); err == nil && os.SameFile(stageInfo, at) {
				_ = root.Remove(stage)
			}
		}
	}()
	if _, err := root.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return "", err
		}
		return "", draftRecoveryError(root, name, []string{pending}, errors.New("a pending original remains; read the draft again to recover it if possible"))
	}
	backups, err := draftBackups(root, name)
	if err != nil {
		return "", err
	}
	newRev := sessions.PlanRevision(text)
	if expectedRevision != missingPlanDraftRevision {
		if err := checkDraftBackups(root, name, backups); err != nil {
			return "", err
		}
		_, rev, err := readDraftEntry(root, name)
		if err != nil {
			return "", err
		}
		if rev != expectedRevision {
			return "", fmt.Errorf("draft changed; read again and merge the user's edits")
		}
		if rev == newRev {
			return rev, nil
		}
		if len(backups) >= maxPlanDraftBackups {
			return "", draftRecoveryError(root, name, backupNames(backups), fmt.Errorf("backup limit (%d) reached; no backup was deleted", maxPlanDraftBackups))
		}
	} else if len(backups) != 0 {
		return "", draftRecoveryError(root, name, backupNames(backups), errors.New("refusing to create a draft over recoverable history"))
	}
	_, err = f.WriteString(text)
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	if at, err := root.Lstat(stage); err != nil || !os.SameFile(stageInfo, at) {
		return "", fmt.Errorf("staged draft %q changed before publication; read the draft again", filepath.Join(root.Name(), stage))
	}
	if expectedRevision != missingPlanDraftRevision {
		if err := moveDraftEntry(root, name, pending); err != nil {
			return "", err
		}
		if err := installDraftReplacement(root, name, stage, expectedRevision); err != nil {
			return "", err
		}
		// The stage was consumed. It may already belong to another writer.
		staged = false
		backup := name + ".previous-" + expectedRevision + "-" + ids.New()
		if err := moveDraftEntry(root, pending, backup); err != nil {
			return "", draftRecoveryError(root, name, []string{pending}, fmt.Errorf("new draft installed but original could not be archived: %w", err))
		}
		if err := checkDraftBackups(root, name, []draftBackup{{backup, expectedRevision}}); err != nil {
			return "", fmt.Errorf("new draft installed: %w", err)
		}
	} else {
		if err := moveDraftEntry(root, stage, name); err != nil {
			return "", err
		}
		staged = false
	}
	return newRev, nil
}

// Compare the entry actually displaced, not just the earlier read of name.
// Neither install nor rollback may replace a save that recreated name. No
// operation unlinks the displaced inode; it remains live for open editors.
func installDraftReplacement(root *os.Root, name, stage, expectedRevision string) error {
	pending := pendingDraftOriginal(name)
	_, rev, err := readDraftEntry(root, pending)
	if err != nil {
		return draftRecoveryError(root, name, []string{pending}, err)
	}
	if rev != expectedRevision {
		err = errors.New("draft changed during update; read it again and merge the user's edits")
	} else {
		err = moveDraftEntry(root, stage, name)
	}
	if err == nil {
		return nil
	}
	if restore := moveDraftEntry(root, pending, name); restore != nil {
		return draftRecoveryError(root, name, []string{pending}, errors.Join(err, restore))
	}
	return err
}

// The native rename must be relative to the pinned directory and fail if the
// destination exists. A check followed by os.Root.Rename isn't equivalent.
func moveDraftEntry(root *os.Root, from, to string) error {
	if !filepath.IsLocal(from) || !filepath.IsLocal(to) || filepath.Base(from) != from || filepath.Base(to) != to {
		return fmt.Errorf("draft moves require names within the plans directory")
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := renameDraftNoReplace(dir, from, to); err != nil {
		return &os.LinkError{Op: "rename without replacement", Old: filepath.Join(root.Name(), from), New: filepath.Join(root.Name(), to), Err: err}
	}
	return nil
}
