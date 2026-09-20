package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// trialReportLimit bounds what a trial's command reports to its observer.
const trialReportLimit = 8 << 20

// TrialResult is what one sandbox trial saw.
type TrialResult struct {
	// ExitCode is the command's exit status, or 128 plus the signal that
	// ended it.
	ExitCode int
	// Output is what the command printed, standard output and error
	// interleaved, bounded like bash's.
	Output string
	// Observation is what the sandbox denied the command, each denial's
	// cause classified against Policy.
	Observation sandbox.Observation
	// Policy is the trial's prepared policy: the one bash starts from merged
	// with the candidate.
	Policy sandbox.Config
}

// RunTrial runs command with bash in the registry's execution root under a
// trial policy, the policy bash starts from (the base and its layers) merged
// with candidate, and reports what the sandbox denied it. The registry's own
// policy does not change: the candidate reaches this one command. The
// command has the trial policy's full reach, like a bash call, so a trial is
// something the user chose to run. What it observes is evidence of what the
// command reached for, not proof that a grant is needed or safe (see
// sandbox.DenialObserver). Where denials cannot be observed the command
// still runs, and Observation.Incomplete says why nothing was seen.
//
// A command that fails is a result, not an error. RunTrial returns an error
// when the command could not run at all, including when ctx ends it.
func (r *ToolRegistry) RunTrial(ctx context.Context, command string, candidate sandbox.Config) (TrialResult, error) {
	if strings.TrimSpace(command) == "" {
		return TrialResult{}, errors.New("a trial needs a command")
	}
	if r.executionPolicy != nil {
		return TrialResult{}, errors.New("a registry bound to an execution context cannot run sandbox trials")
	}
	if r.sandboxFactory == nil {
		return TrialResult{}, errors.New("sandbox trials need the sandbox")
	}
	release, err := r.TryEnvironmentUse()
	if err != nil {
		return TrialResult{}, err
	}
	defer release()
	var obs sandbox.Observation
	overlay := candidate
	observer, err := sandbox.NewDenialObserver()
	if err != nil {
		obs.Incomplete = err.Error()
	} else {
		defer observer.Close()
		overlay = observer.Config(candidate)
	}
	sb, policy, err := r.newSandboxFor("trial", &overlay)
	if err != nil {
		return TrialResult{}, fmt.Errorf("sandbox for the trial: %w", err)
	}
	shell := sandbox.TrialShell{Script: command}
	if observer != nil {
		if err := observer.Start(ctx, sb); err != nil {
			if ctx.Err() != nil {
				return TrialResult{}, ctx.Err()
			}
			obs.Incomplete = err.Error()
			observer = nil
		} else {
			shell = observer.Shell(command, trialReportFD)
		}
	}

	output := newBoundedBuffer(capturedOutputLimit)
	spec := finiteCommand{
		name: "bash", args: []string{"-c", shell.Script}, dir: r.executionRoot, env: shell.Env,
		stdout: output, stderr: output, acknowledge: true,
	}
	if shell.Report {
		spec.report = newBoundedBuffer(trialReportLimit)
	}
	_, runErr := runFiniteCommand(ctx, sb, spec)
	result := TrialResult{Output: strings.TrimSpace(output.String()), Policy: policy}
	if runErr != nil {
		code, ok := shellExitCode(runErr)
		if !ok {
			return result, runErr
		}
		result.ExitCode = code
	}

	if observer != nil {
		var report []byte
		if spec.report != nil {
			report = spec.report.Bytes()
		}
		if obs, err = observer.Finish(ctx, sb, report); err != nil {
			obs.Incomplete = err.Error()
		}
		if spec.report != nil && spec.report.Truncated() && obs.Incomplete == "" {
			obs.Incomplete = "the trial's report was cut short, so some denials are missing"
		}
	}
	if err := sandbox.ClassifyDenials(policy, obs.Denials); err != nil {
		return result, fmt.Errorf("classify the trial's denials: %w", err)
	}
	result.Observation = obs
	return result, nil
}
