package swarm

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// coordinatedSession is what the runtime needs of a member's session.
type coordinatedSession interface {
	sessions.Session
	sessions.CoordinationSession
	sessions.SpawnOutcomeSession
}

// memberStore is what the runtime needs of the store its members live in.
type memberStore interface {
	sessions.SessionStore
	sessions.SessionCreator
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
// describe and titled title, in the one transaction the store creates it in.
func (r *Runtime) createMemberSession(ctx context.Context, name string, options sessions.AcquireOptions, title string, describe func(*sessions.Metadata)) (coordinatedSession, error) {
	store, ok := r.config.Store.(memberStore)
	if !ok {
		return nil, errors.New("session store cannot create member sessions")
	}
	session, err := store.Create(ctx, name, options, func(meta *sessions.Metadata) {
		describe(meta)
		meta.Title, meta.TitleSource = title, sessions.TitleSourceAgent
	})
	if err != nil {
		return nil, err
	}
	coordinated, ok := session.(coordinatedSession)
	if !ok {
		// The session is of no use, and as a member its family would keep it.
		err := errors.Join(errors.New("member session does not support coordination"), session.Close())
		r.deleteMemberSession(ctx, name)
		return nil, err
	}
	return coordinated, nil
}

// dropUnrecordedMember deletes the session of a member whose spawn failed
// before a record named it. The family keeps a member's session until its
// root is removed, and nothing else would remove this one. An edit that
// failed only in waiting to be written has recorded the member, whose
// session then stays.
func (r *Runtime) dropUnrecordedMember(ctx context.Context, id, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	recorded := false
	if err := r.look(ctx, func(s *State) { recorded = s.Members[id] != nil }); err != nil {
		// Whether a record names the member is unknown, so its session stays.
		slog.Warn("member_session_retained", "session", name, "error", err)
		return
	}
	if !recorded {
		r.deleteMemberSession(ctx, name)
	}
}

// deleteMemberSession deletes the session of a member that will never run.
// What it cannot delete stays with the family until its root is removed.
func (r *Runtime) deleteMemberSession(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.config.Store.Delete(ctx, name); err != nil {
		slog.Warn("member_session_retained", "session", name, "error", err)
	}
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
