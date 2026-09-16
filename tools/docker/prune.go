package docker

import (
	"context"
	"errors"
	"strings"
)

// Pruned describes a container prune removed or would remove.
type Pruned struct {
	ID      string
	Name    string
	Session string
	Root    string
}

// Prune removes the containers polly created on the daemon at host (empty
// for the usual resolution) whose session no longer exists: keep reports
// whether a session identity is still known. With all set, every polly
// container goes. dryRun lists without removing. Startup never reaps;
// this is the explicit reaper.
func Prune(ctx context.Context, host string, keep func(session string) bool, all, dryRun bool) ([]Pruned, error) {
	ep, err := resolveEndpoint(host)
	if err != nil {
		return nil, err
	}
	e := newEngine(ep)
	if err := e.ping(ctx); err != nil {
		return nil, err
	}
	containers, err := e.containerList(ctx, []string{labelSession})
	if err != nil {
		return nil, err
	}
	var pruned []Pruned
	var errs []error
	for _, c := range containers {
		session := c.Labels[labelSession]
		if !all && keep != nil && !strings.HasPrefix(session, "anonymous-") && keep(session) {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		entry := Pruned{ID: c.ID, Name: name, Session: session, Root: c.Labels[labelRoot]}
		if !dryRun {
			if err := e.containerRemove(ctx, c.ID); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		pruned = append(pruned, entry)
	}
	return pruned, errors.Join(errs...)
}
