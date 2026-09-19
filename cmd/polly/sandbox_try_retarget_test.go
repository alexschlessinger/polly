package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestSandboxTryRequiresANewReviewAfterAPathChanges(t *testing.T) {
	for _, action := range []string{"trial", "save", "session"} {
		t.Run(action, func(t *testing.T) {
			home, state := sandboxTryState(t)
			path := filepath.Join(home, ".toolrc")
			secret := filepath.Join(home, ".ssh", "review-fixture")
			writeFile(t, path, "ordinary configuration")
			writeFile(t, secret, "fake credential")
			try, trials := scriptedTry(t, state, denied())
			p, err := try.suggested("read ~/.toolrc", "read the tool's configuration")
			if err != nil || p.refused != "" || p.credential {
				t.Fatalf("initial row = %+v, %v", p, err)
			}
			try.rows = append(try.rows, p)
			if try.tickReads() != 1 {
				t.Fatal("ordinary read was not selected")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, path); err != nil {
				t.Fatal(err)
			}
			if action == "trial" {
				dropped, err := try.trial(context.Background())
				if err != nil || len(dropped) != 1 || len(trials.candidates[0].ReadPaths) != 0 {
					t.Fatalf("trial applied a changed read: dropped %v, err %v, candidates %+v", dropped, err, trials.candidates)
				}
			} else if _, err := try.allow(action == "save"); err != nil {
				t.Fatal(err)
			}
			if p.ticked || !p.credential || !strings.Contains(p.refused, "start a new review") {
				t.Fatalf("changed row was not withdrawn: %+v", p)
			}
			if cfg, _, err := state.toolRegistry.SandboxReadPolicy(); err != nil || sandbox.ReadAllowed(cfg, secret) == nil {
				t.Fatalf("changed read exposed the credential: %+v, %v", cfg, err)
			}
			if saved, err := readSandboxProfile(state.sandboxProfile.ws.profile); err != nil || len(saved.Items) != 0 {
				t.Fatalf("changed read was saved: %+v, %v", saved, err)
			}
			// A fresh review names it as a credential, leaves bulk selection
			// off, and accepts only a new individual selection.
			renewed, _ := scriptedTry(t, state)
			row, err := renewed.suggested("read ~/.toolrc", "read the credential")
			if err != nil || !row.credential || row.refused != "" {
				t.Fatalf("fresh row = %+v, %v", row, err)
			}
			renewed.rows = append(renewed.rows, row)
			if renewed.tickReads() != 0 {
				t.Fatal("bulk selection included a credential")
			}
			if err := renewed.toggle(0); err != nil {
				t.Fatal(err)
			}
			if _, err := renewed.allow(false); err != nil {
				t.Fatal(err)
			}
			if cfg, _, err := state.toolRegistry.SandboxReadPolicy(); err != nil || sandbox.ReadAllowed(cfg, secret) != nil {
				t.Fatalf("fresh individual consent did not grant the read: %+v, %v", cfg, err)
			}
		})
	}
}

func TestSandboxTryRejectsRetargetingBetweenOrdinaryPaths(t *testing.T) {
	home, state := sandboxTryState(t)
	first, second, link := filepath.Join(home, "first"), filepath.Join(home, "second"), filepath.Join(home, ".toolrc")
	writeFile(t, first, "first configuration")
	writeFile(t, second, "other configuration")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	try, _ := scriptedTry(t, state)
	p, err := try.suggested("read ~/.toolrc", "read configuration")
	if err != nil || p.refused != "" {
		t.Fatalf("initial row = %+v, %v", p, err)
	}
	try.rows = append(try.rows, p)
	try.tickReads()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if dropped, err := try.prepare(); err != nil || len(dropped) != 1 || p.ticked || p.credential {
		t.Fatalf("ordinary retarget kept its selection: %+v, dropped %v, %v", p, dropped, err)
	}
}
