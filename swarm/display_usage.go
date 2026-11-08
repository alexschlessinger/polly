package swarm

import (
	"context"
	"maps"
	"slices"

	"github.com/alexschlessinger/pollytool/sessions"
)

// DisplayUsage is an owned, read-only observation. Revisions are opaque equality
// tokens, never authority. Unchanged omits Members; the caller retains its last
// successful values. Every historical execution contributes, not only the
// latest execution or current roster. Unrelated family kinds cannot invalidate it.
type DisplayUsage struct {
	Revisions []sessions.Revision
	Members   []sessions.DisplayMemberUsage
	Unchanged bool
}

func (r *Runtime) ReadDisplayUsage(ctx context.Context, known []sessions.Revision) (DisplayUsage, error) {
	// Display reads do not prepare/promote a backend or acquire another handle.
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return DisplayUsage{}, err
	}
	// A root may have only its parent-turn journal before the first delegation.
	// Validate that narrow exception, rather than bootstrapping a swarm to read
	// an empty usage total. The repository still rejects any formatless domain
	// records and formatless child handles.
	repo.parentJournal = true
	var out DisplayUsage
	err = repo.Read(ctx, func(rd *recordReader) error {
		if rd.journalOnly {
			return nil
		}
		if repo.Capabilities().DurableRevisions {
			out.Revisions, err = rd.Revisions([]sessions.RevisionScope{
				{Index: sessions.CoordinationIndexKind, Key: "execution"},
				{Index: sessions.CoordinationIndexKind, Key: "member"},
				{Index: sessions.CoordinationIndexKind, Key: "task"},
			})
			if err != nil {
				return err
			}
			if len(known) > 0 && slices.Equal(known, out.Revisions) {
				out.Unchanged = true
				return nil
			}
		}
		if reader, ok := rd.raw.(sessions.CoordinationUsageReader); ok {
			out.Members, err = reader.DisplayUsage()
		} else {
			out.Members, err = legacyDisplayUsage(rd)
		}
		return err
	})
	return out, err
}

// Compatibility readers remain functional but do not promise compact payload
// work. Only executions and their label dependencies are decoded here; a legacy
// coordination adapter may itself materialize the family before this callback.
func legacyDisplayUsage(rd *recordReader) ([]sessions.DisplayMemberUsage, error) {
	totals := map[string]sessions.DisplayMemberUsage{}
	for after := ""; ; {
		page, err := rd.Executions(sessions.RecordQuery{Index: sessions.CoordinationIndexKind, Key: "execution", After: after, Limit: 128})
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			execution := record.Value
			u := totals[execution.Member]
			u.Member = execution.Member
			usage := execution.Usage
			if usage.Samples != 0 || usage.InputTokens != nil || usage.OutputTokens != nil {
				if usage.InputTokens != nil {
					u.InputTokens += *usage.InputTokens
				} else {
					u.InputMissing = true
				}
				if usage.OutputTokens != nil {
					u.OutputTokens += *usage.OutputTokens
				} else {
					u.OutputMissing = true
				}
			}
			totals[execution.Member] = u
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	var out []sessions.DisplayMemberUsage
	for _, id := range slices.Sorted(maps.Keys(totals)) {
		u := totals[id]
		if id == "" {
			out = append(out, u)
			continue
		}
		member, err := rd.Member(id)
		if err != nil {
			return nil, err
		}
		if member != nil {
			u.Label = member.Label
			if member.Execution != "" {
				execution, err := rd.Execution(member.Execution)
				if err != nil {
					return nil, err
				}
				if execution != nil {
					u.Brief = execution.Request.Task
				}
			}
			if u.Brief == "" && member.Task != "" {
				task, err := rd.Task(member.Task)
				if err != nil {
					return nil, err
				}
				if task != nil {
					u.Brief = task.Description
				}
			}
			if u.Brief == "" {
				u.Brief = member.Name
			}
		}
		out = append(out, u)
	}
	return out, nil
}
