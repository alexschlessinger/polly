//go:build !darwin && !linux

package sandbox

import (
	"context"
	"fmt"
	"runtime"
)

type observerState struct{}

func (o *DenialObserver) initPlatform() error {
	return fmt.Errorf("%w: sandboxing is unsupported on %s", ErrDenialsUnobservable, runtime.GOOS)
}

func (o *DenialObserver) Start(ctx context.Context, sb Sandbox) error {
	return o.initPlatform()
}

func (o *DenialObserver) Shell(command string, reportFD int) TrialShell {
	return TrialShell{Script: command}
}

func (o *DenialObserver) Finish(ctx context.Context, sb Sandbox, report []byte) (Observation, error) {
	return Observation{}, o.initPlatform()
}

func (o *DenialObserver) Close() error { return nil }
