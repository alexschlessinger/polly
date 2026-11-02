package swarm

import (
	"regexp"
	"sort"
	"strings"
)

const (
	PublicationScopeRun       = "run"
	PublicationScopeMachine   = "machine"
	PublicationScopeWorkspace = "workspace"
	PublicationScopeSnapshot  = "snapshot"
)

var publicationKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)

// Old records retain their original audience. Explicit local scopes also reach
// the parent, with provenance, but never become facts for a later run's workers.
func publicationScope(p *Publication) string {
	if p.Scope != "" {
		return p.Scope
	}
	if publicationKind(p) == PublicationKindHost {
		return PublicationScopeMachine
	}
	return PublicationScopeRun
}

func preparePublicationScope(s *State, actor string, p *Publication) error {
	if p.Key != "" && !publicationKeyPattern.MatchString(p.Key) {
		return fail("invalid_args", "key must be 1-128 lowercase letters, digits, dots, underscores, slashes or hyphens, starting with a letter or digit")
	}
	if p.Value != "" && (p.Key == "" || len(p.Value) > 256) {
		return fail("invalid_args", "value requires a fact key and must be at most 256 bytes; use a short canonical claim and put evidence in text")
	}
	switch publicationScope(p) {
	case PublicationScopeMachine:
		if publicationKind(p) != PublicationKindHost {
			return fail("invalid_args", "machine scope requires kind:host; use workspace or snapshot for local setup observations")
		}
	case PublicationScopeRun, PublicationScopeWorkspace, PublicationScopeSnapshot:
		if publicationKind(p) == PublicationKindHost {
			return fail("invalid_args", "kind:host is machine-wide; use kind:finding for run, workspace or snapshot scope")
		}
	default:
		return fail("invalid_args", "scope must be run, machine, workspace or snapshot")
	}
	if publicationScope(p) == PublicationScopeSnapshot && p.Snapshot == "" {
		if m := s.Members[actor]; m != nil {
			if e := s.Executions[m.Execution]; e != nil {
				p.Snapshot = e.Base
			}
		}
		if p.Snapshot == "" {
			return fail("invalid_args", "snapshot scope requires a captured baseline; supply commit or use workspace scope for a live workspace observation")
		}
	}
	if publicationScope(p) == PublicationScopeSnapshot && !validTaskSnapshot(s.Snapshots[p.Snapshot], p.Snapshot) {
		return unavailableWorkspace()
	}
	return nil
}

// A canonical value lets independent reports phrase their evidence differently.
// Different values remain visible, not last-writer-wins. Without a value, only
// exact text confirmations coalesce; no semantic equivalence is inferred.
func samePublicationFact(s *State, a, b *Publication) bool {
	if a.Key != b.Key || publicationScope(a) != publicationScope(b) || a.Value != b.Value {
		return false
	}
	if a.Value == "" && strings.TrimSpace(a.Text) != strings.TrimSpace(b.Text) {
		return false
	}
	if publicationScope(a) != PublicationScopeMachine && a.Run != b.Run {
		return false
	}
	return snapshotCommit(s, a.Snapshot) == snapshotCommit(s, b.Snapshot)
}

// Confirmations describe current support, not historical DuplicateOf links.
// A correction withdraws only its predecessor, including when that predecessor
// was a confirmation. Siblings of a promoted representative remain discoverable.
func publicationConfirmations(s *State, id string) []string {
	p := s.Publications[id]
	if p == nil || p.Key == "" {
		return nil
	}
	var ids []string
	active := false
	for _, other := range activePublications(s) {
		if other.ID == id {
			active = true
		} else if samePublicationFact(s, p, other) {
			ids = append(ids, other.ID)
		}
	}
	if !active {
		return nil
	}
	sort.Strings(ids)
	return ids
}
