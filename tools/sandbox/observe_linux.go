//go:build linux

package sandbox

import (
	"context"
	"fmt"
)

// linuxTrialScript runs a trial's command between two listings of the
// private home directory, written to the report descriptor as NUL-terminated
// records: every entry before the command, then the directories and the
// other entries after it (see parseHomeReport). The command and the home
// directory arrive in the environment, never spliced into the script, and
// the command inherits neither those variables nor the descriptor. Listings
// stay on the home directory's own tmpfs, so they skip the grants bound into
// it, and the script exits with the command's status.
const linuxTrialScript = `polly_command=$POLLY_TRIAL_COMMAND polly_home=$POLLY_TRIAL_HOME
unset POLLY_TRIAL_COMMAND POLLY_TRIAL_HOME
find "$polly_home" -xdev -maxdepth %[2]d -print0 >&%[1]d 2>/dev/null
"$BASH" -c "$polly_command" %[1]d>&-
polly_status=$?
printf '%%s\0' %[3]s >&%[1]d
find "$polly_home" -xdev -maxdepth %[2]d -type d -print0 >&%[1]d 2>/dev/null
printf '%%s\0' %[4]s >&%[1]d
find "$polly_home" -xdev -maxdepth %[2]d ! -type d -print0 >&%[1]d 2>/dev/null
exit "$polly_status"`

// linuxObservationLimit is what a Linux trial cannot see.
const linuxObservationLimit = "Linux trials report only writes into the private home directory, which the sandbox discards; reads of hidden paths and writes refused elsewhere are not observed"

type observerState struct{ privateHome bool }

func (o *DenialObserver) initPlatform() error { return nil }

// Start checks that sb is the Linux backend, whose private home directory is
// a tmpfs the trial's script can list.
func (o *DenialObserver) Start(ctx context.Context, sb Sandbox) error {
	native, ok := sb.(*linuxSandbox)
	if !ok {
		return fmt.Errorf("%w: the trial's sandbox is not the Linux backend", ErrDenialsUnobservable)
	}
	o.privateHome = native.cfg.PrivateHome
	return nil
}

// Shell wraps command in linuxTrialScript, reporting to reportFD.
func (o *DenialObserver) Shell(command string, reportFD int) TrialShell {
	if !o.privateHome {
		return TrialShell{Script: command}
	}
	return TrialShell{
		Script: fmt.Sprintf(linuxTrialScript, reportFD, homeReportDepth, homeReportDirs, homeReportFiles),
		Env:    map[string]string{"POLLY_TRIAL_COMMAND": command, "POLLY_TRIAL_HOME": o.home},
		Report: true,
	}
}

// Finish reads the writes the command left in the private home directory
// from the script's report.
func (o *DenialObserver) Finish(ctx context.Context, sb Sandbox, report []byte) (Observation, error) {
	if !o.privateHome {
		return Observation{Limit: "Linux cannot automatically observe denied operations with readable home; inspect command output and propose only required access"}, nil
	}
	denials, complete := parseHomeReport(report, o.home)
	var set denialSet
	for _, d := range denials {
		set.add(d)
	}
	obs := Observation{Denials: set.denials, Truncated: set.truncated, Limit: linuxObservationLimit}
	if !complete {
		obs.Incomplete = "the command did not finish, so the writes it left in the private home directory were not listed"
	}
	return obs, nil
}

// Close does nothing on Linux.
func (o *DenialObserver) Close() error { return nil }
