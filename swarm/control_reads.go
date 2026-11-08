package swarm

import (
	"context"

	"github.com/alexschlessinger/pollytool/sessions"
)

// AllMembersStopped reads the source controls needed by environment maintenance,
// not a display cache or a historical execution projection. It retains the same
// point-in-time check as State while excluding tasks, mail and execution bodies.
// An unstopped member ends the read immediately. This does not acquire a lease
// or replace the caller's maintenance/launch exclusion.
func (r *Runtime) AllMembersStopped(ctx context.Context) (bool, error) {
	stopped := true
	err := r.readRecords(ctx, func(rd *recordReader) error {
		q := sessions.RecordQuery{Limit: 64, MaxBytes: 128 << 10}
		for {
			page, err := rd.Members(q)
			if err != nil {
				return err
			}
			for _, record := range page.Records {
				if record.Value.Control != MemberControlStopped {
					stopped = false
					return nil
				}
			}
			if page.Next == "" {
				return nil
			}
			q.After = page.Next
		}
	})
	return stopped, err
}
