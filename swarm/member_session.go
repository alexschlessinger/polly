package swarm

import (
	"context"
	"errors"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Member names are cached display labels. If a handle changed, resolve the
// stable identity before acquiring; ExpectedID still fences a subsequent rename
// or deletion/reuse between that lookup and the lease transaction.
func (r *Runtime) acquireMemberSession(ctx context.Context, member *Member) (sessions.Session, error) {
	options := sessions.AcquireOptions{ExpectedID: member.ID, ExistingOnly: true}
	session, err := r.config.Store.Acquire(ctx, member.Name, options)
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		return session, err
	}
	summaries, readErr := r.config.Store.ListSummaries(ctx)
	if readErr != nil {
		return nil, readErr
	}
	for _, summary := range summaries {
		if summary.ID != member.ID || summary.Metadata == nil {
			continue
		}
		name := summary.Metadata.Name
		session, err = r.config.Store.Acquire(ctx, name, options)
		if err != nil {
			return nil, err
		}
		if err = r.update(ctx, func(s *State) error {
			current := s.Members[member.ID]
			if current == nil {
				return sessions.ErrSessionNotFound
			}
			current.Name = name
			return nil
		}); err != nil {
			return nil, errors.Join(err, session.Close())
		}
		member.Name = name
		return session, nil
	}
	return nil, err
}
