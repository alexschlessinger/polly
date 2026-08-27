package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

type suspendTrackingScreen struct {
	tcell.Screen
	steps      *[]string
	suspendErr error
	resumeErr  error
}

func (s suspendTrackingScreen) Suspend() error {
	*s.steps = append(*s.steps, "terminal suspend")
	return s.suspendErr
}

func (s suspendTrackingScreen) Resume() error {
	*s.steps = append(*s.steps, "terminal resume")
	return s.resumeErr
}

func TestCtrlZRequestsSuspendWithoutChangingComposerState(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.ed.setText("unfinished draft")
	r.model.searching = true

	if quit := r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-z>"}); quit {
		t.Fatal("Ctrl-Z requested quit")
	}
	select {
	case <-r.suspend:
	default:
		t.Fatal("Ctrl-Z did not request process suspension")
	}
	if got := r.model.ed.text(); got != "unfinished draft" {
		t.Fatalf("composer after Ctrl-Z = %q, want unchanged draft", got)
	}
	if !r.model.searching {
		t.Fatal("Ctrl-Z should preserve reverse search across fg")
	}
}

func TestSuspendTerminalRestoresBeforeStopAndResumesAfter(t *testing.T) {
	var steps []string
	screen := suspendTrackingScreen{steps: &steps}
	err := suspendTerminal(screen, func() error {
		steps = append(steps, "process suspend")
		return nil
	})
	if err != nil {
		t.Fatalf("suspendTerminal() error = %v", err)
	}
	want := []string{"terminal suspend", "process suspend", "terminal resume"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("suspension order = %#v, want %#v", steps, want)
	}
}

func TestSuspendTerminalResumesWhenSignalFails(t *testing.T) {
	var steps []string
	screen := suspendTrackingScreen{steps: &steps}
	signalErr := errors.New("signal failed")
	err := suspendTerminal(screen, func() error {
		steps = append(steps, "process suspend")
		return signalErr
	})
	if !errors.Is(err, signalErr) {
		t.Fatalf("suspendTerminal() error = %v, want wrapped signal failure", err)
	}
	want := []string{"terminal suspend", "process suspend", "terminal resume"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("failure order = %#v, want %#v", steps, want)
	}
}

func TestSuspendTerminalDoesNotStopProcessWhenTerminalReleaseFails(t *testing.T) {
	var steps []string
	releaseErr := errors.New("release failed")
	screen := suspendTrackingScreen{steps: &steps, suspendErr: releaseErr}
	err := suspendTerminal(screen, func() error {
		steps = append(steps, "process suspend")
		return nil
	})
	if !errors.Is(err, releaseErr) {
		t.Fatalf("suspendTerminal() error = %v, want wrapped release failure", err)
	}
	want := []string{"terminal suspend"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("release-failure order = %#v, want %#v", steps, want)
	}
}
