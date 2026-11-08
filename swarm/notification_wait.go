package swarm

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Keep logical readiness reads distinguishable from the shared revision source's
// callbacks. The source uses the runtime's context, never this operation context.
type notificationWaitReadKey struct{}

func notificationWaitContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, notificationWaitReadKey{}, true)
}

func notificationWaitErr(ctx context.Context, sub *ChangeSubscription) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-sub.Done():
		return context.Canceled // the provider or subscription was shut down
	default:
		return nil
	}
}

// Drain only before reading. A hint arriving during or after the read must stay
// buffered until the next read, including when Replace discovers new scopes.
func beforeNotificationRead(ctx context.Context, sub *ChangeSubscription) error {
	if err := notificationWaitErr(ctx, sub); err != nil {
		return err
	}
	select {
	case <-sub.Changes():
	default:
	}
	return nil
}

func waitForNotification(ctx context.Context, sub *ChangeSubscription, deadline <-chan time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sub.Done():
		return notificationWaitErr(ctx, sub)
	case <-sub.Changes():
	case <-deadline:
	}
	return notificationWaitErr(ctx, sub)
}

// Settlement remains a broad authority read. Only record commits and runtime
// activity can change that decision; display-only signals do not participate.
func (r *Runtime) subscribeSettlement(ctx context.Context) (*ChangeSubscription, error) {
	return r.Subscribe(ctx, []sessions.RevisionScope{
		{Index: sessions.CoordinationIndexRecords}, ActivityScope(),
	})
}

func invocationWaitScopes(member string) []sessions.RevisionScope {
	return []sessions.RevisionScope{
		{Index: sessions.CoordinationIndexMailPending, Key: member},
		{Index: sessions.CoordinationIndexPublicationPending, Key: member},
		waitRecordScope("member", member),
	}
}

func waitRecordScope(kind, id string) sessions.RevisionScope {
	return sessions.RevisionScope{Index: sessions.CoordinationIndexRecord, Key: sessions.CoordinationIndexKey(kind, id)}
}

// invocationWaitState reads only the assignment and its direct dependencies.
// Missing IDs are still subscribed: creating them can change readiness. The
// admission loader additionally reads owner/execution/snapshot authority, which
// is unnecessary for lifecycleWaitState and must not broaden this wait.
func invocationWaitState(reader *recordReader, memberID string, scopes *[]sessions.RevisionScope) (string, error) {
	member, err := reader.Member(memberID)
	if err != nil {
		return "", err
	}
	if member == nil {
		return "missing", nil
	}
	if member.Task == "" {
		return "", nil
	}
	*scopes = append(*scopes, waitRecordScope("task", member.Task))
	task, err := reader.Task(member.Task)
	if err != nil || task == nil {
		return "", err
	}
	dependencies := &taskDependencies{Task: task, Dependencies: make(map[string]*Task, len(task.Dependencies))}
	for _, id := range task.Dependencies {
		if _, seen := dependencies.Dependencies[id]; seen {
			continue
		}
		*scopes = append(*scopes, waitRecordScope("task", id))
		if dependencies.Dependencies[id], err = reader.Task(id); err != nil {
			return "", err
		}
	}
	return lifecycleWaitState(dependencies), nil
}

// waitInvocation returns when this yielded invocation has deliverable input, a
// changed assignment/dependency state, or its optional deadline expires. It does
// not mutate or finish the execution; the caller retains all lifecycle authority.
func (r *Runtime) waitInvocation(ctx context.Context, i *invocation) error {
	ctx = notificationWaitContext(ctx)
	sub, err := r.Subscribe(ctx, invocationWaitScopes(i.member))
	if err != nil {
		return err
	}
	defer sub.Close()
	var deadline <-chan time.Time
	if !i.waitUntil.IsZero() {
		timer := time.NewTimer(time.Until(i.waitUntil))
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		if err := beforeNotificationRead(ctx, sub); err != nil {
			return err
		}
		scopes := invocationWaitScopes(i.member)
		ready := false
		err := r.readRecords(ctx, func(reader *recordReader) error {
			mail, err := reader.Mails(sessions.RecordQuery{Index: sessions.CoordinationIndexMailPending, Key: i.member, Limit: 1})
			if err != nil {
				return err
			}
			if len(mail.Records) != 0 {
				ready = true
				return nil
			}
			publications, err := reader.Publications(sessions.RecordQuery{Index: sessions.CoordinationIndexPublicationPending, Key: i.member, Limit: 1})
			if err != nil {
				return err
			}
			if len(publications.Records) != 0 {
				ready = true
				return nil
			}
			state, err := invocationWaitState(reader, i.member, &scopes)
			ready = state != i.waitState
			return err
		})
		if err != nil {
			return err
		}
		added, err := sub.Replace(scopes)
		if err != nil {
			return err
		}
		if err := notificationWaitErr(ctx, sub); err != nil {
			return err
		}
		if added {
			continue
		}
		if ready || !i.waitUntil.IsZero() && !time.Now().Before(i.waitUntil) {
			return nil
		}
		if err := waitForNotification(ctx, sub, deadline); err != nil {
			return err
		}
	}
}
