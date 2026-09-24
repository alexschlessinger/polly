package swarm

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// publicationDigestBytes bounds the text of a publication staged into a
// teammate's conversation; the rest is a swarm_read away. Every member of
// the run pays this for every publication, so it stays well under the mail
// clip.
const publicationDigestBytes = 1024

// pendingPublications lists what a member has not yet been shown, in posting
// order: the publications of run by other authors, later than the member's
// mark, and only the latest of a supersede chain. A member new to the run
// sees the run's earlier publications at its first boundary; the mark, not
// membership, decides. Publications are information for the members working
// beside the author, so the parent, whose input is delivered results, is
// never a reader here.
func pendingPublications(s *State, member, run string) []*Publication {
	m := s.Members[member]
	if m == nil || run == "" {
		return nil
	}
	superseded := map[string]bool{}
	for _, p := range s.Publications {
		if p.Supersedes != "" {
			superseded[p.Supersedes] = true
		}
	}
	var out []*Publication
	for _, p := range s.Publications {
		if p.Run != run || p.Author == member || superseded[p.ID] || !m.Publications.before(p) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Posted.Equal(out[j].Posted) {
			return out[i].ID < out[j].ID
		}
		return out[i].Posted.Before(out[j].Posted)
	})
	return out
}

// admittedPublicationText renders a publication for a teammate: who posted
// it and when, its sources, commit and artifacts, then the text, clipped
// with a pointer to the rest.
func admittedPublicationText(s *State, p *Publication) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Publication %s from %s (%s)", p.ID, p.Author, p.Posted.UTC().Format(time.RFC3339))
	if len(p.Sources) > 0 {
		fmt.Fprintf(&b, "; sources: %s", strings.Join(p.Sources, ", "))
	}
	if commit := snapshotCommit(s, p.Snapshot); commit != "" {
		fmt.Fprintf(&b, "; commit %s", commit)
	}
	if len(p.Artifacts) > 0 {
		ids := make([]string, 0, len(p.Artifacts))
		for _, ref := range p.Artifacts {
			ids = append(ids, ref.ID)
		}
		fmt.Fprintf(&b, "; artifacts for read_artifact: %s", strings.Join(ids, ", "))
	}
	if p.Supersedes != "" {
		fmt.Fprintf(&b, "; supersedes %s", p.Supersedes)
	}
	b.WriteString(":\n")
	if len(p.Text) <= publicationDigestBytes {
		b.WriteString(p.Text)
		return b.String()
	}
	fmt.Fprintf(&b, "%s\n(%d bytes; full text: swarm_read({view: \"publications\", id: %q}))", clipInspection(p.Text, publicationDigestBytes), len(p.Text), p.ID)
	return b.String()
}
