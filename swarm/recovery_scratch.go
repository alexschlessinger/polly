package swarm

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/scratch"
)

// A refresh parks scratch before its mark transaction. If that commit returns
// an uncertain error, its durable Followup may still lack HeldScratch. Protect
// the deterministic hold while its exact refresh intent is preparing/interrupted.
// This is retention only: it neither grants paths nor adopts an uncertain hold.
// Enumerate actual directories in bounded pages, not all historical followups.
func (r *Runtime) retainUncertainScratch(ctx context.Context, live map[string]bool) error {
	dir, err := os.Open(scratch.Root())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	prefix := heldScratchName(r.ID, "")
	for {
		entries, readErr := dir.ReadDir(recoveryPageLimit)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		var pending []string
		for _, entry := range entries {
			id, ok := strings.CutPrefix(entry.Name(), prefix)
			if ok && id != "" && entry.IsDir() && !live[filepath.Join(scratch.Root(), entry.Name())] {
				pending = append(pending, id)
			}
		}
		if len(pending) != 0 {
			if err := repo.Read(ctx, func(rd *recordReader) error {
				for _, id := range pending {
					f, err := rd.Followup(id)
					if err != nil {
						return err
					}
					if f != nil && f.Refresh && (f.Phase == "preparing" || f.Phase == "interrupted") {
						live[filepath.Join(scratch.Root(), prefix+id)] = true
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}
