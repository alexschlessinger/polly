package swarm

import (
	"context"
	"errors"

	"github.com/alexschlessinger/pollytool/sessions"
)

type coordinatedSession interface {
	sessions.Session
	sessions.CoordinationSession
}

// A parent with coordination support does not establish that every handle
// returned by the store has it. Validate newly created and resumed members at
// acquisition, releasing an incompatible handle before returning the error.
func (r *Runtime) acquireCoordinatedSession(ctx context.Context, name string, options sessions.AcquireOptions) (coordinatedSession, error) {
	session, err := r.config.Store.Acquire(ctx, name, options)
	if err != nil {
		return nil, err
	}
	coordinated, ok := session.(coordinatedSession)
	if !ok {
		return nil, errors.Join(errors.New("member session does not support coordination"), session.Close())
	}
	return coordinated, nil
}

// Member names are cached display labels. If a handle changed, resolve the
// stable identity before acquiring; ExpectedID still fences a subsequent rename
// or deletion/reuse between that lookup and the lease transaction.
func (r *Runtime) acquireMemberSession(ctx context.Context, member *Member) (coordinatedSession, error) {
	options := sessions.AcquireOptions{ExpectedID: member.ID, ExistingOnly: true}
	session, err := r.acquireCoordinatedSession(ctx, member.Name, options)
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
		session, err = r.acquireCoordinatedSession(ctx, name, options)
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
