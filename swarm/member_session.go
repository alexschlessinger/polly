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

// createMemberSession creates the session of a new member, described by
// describe and titled title. A store that can create a session as it is to
// start does so in one transaction; any other acquires it, resets it to the
// description and titles it.
func (r *Runtime) createMemberSession(ctx context.Context, name string, options sessions.AcquireOptions, title string, describe func(*sessions.Metadata)) (coordinatedSession, error) {
	if creator, ok := r.config.Store.(sessions.SessionCreator); ok {
		session, err := creator.Create(ctx, name, options, func(meta *sessions.Metadata) {
			describe(meta)
			meta.Title, meta.TitleSource = title, sessions.TitleSourceAgent
		})
		if err != nil {
			return nil, err
		}
		coordinated, ok := session.(coordinatedSession)
		if !ok {
			return nil, errors.Join(errors.New("member session does not support coordination"), session.Close())
		}
		return coordinated, nil
	}
	session, err := r.acquireCoordinatedSession(ctx, name, options)
	if err != nil {
		return nil, err
	}
	meta, err := session.GetMetadata(ctx)
	if err == nil {
		describe(meta)
		err = session.Reset(ctx, meta)
	}
	if err == nil {
		if setter, ok := session.(sessions.TitleSession); ok {
			_, err = setter.SetTitle(ctx, title, sessions.TitleSourceAgent)
		}
	}
	if err != nil {
		return nil, errors.Join(err, session.Close())
	}
	return session, nil
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
		if err = r.edit(ctx, func(s *State) error {
			current := s.ownMember(member.ID)
			if current == nil {
				return sessions.ErrSessionNotFound
			}
			current.Name = name
			return nil
		}); err != nil {
			return nil, errors.Join(err, session.Close())
		}
		return session, nil
	}
	return nil, err
}
