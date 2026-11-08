package main

import (
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
)

const agentMessageLimit = 160

type agentMessagePreview struct {
	peer, text       string
	outgoing, queued bool
}

// Pick the newest mail involving each member in one pass over the cached
// snapshot. Posting time, then ID, is the swarm inbox's ordering; delivery
// changes the queued marker, not which message is newest.
func latestAgentMessages(view swarm.DisplayView, parent string) map[string]*agentMessagePreview {
	s := view.DisplaySnapshot(parent)
	latest := map[string]*swarm.Mail{}
	for _, mail := range s.Messages {
		// Runtime notices (completion reports and the like) repeat what
		// the row already says.
		if mail.Kind == "info" {
			continue
		}
		for _, id := range []string{mail.From, mail.To} {
			if s.Members[id] == nil {
				continue
			}
			previous := latest[id]
			if previous == nil || mail.Posted.After(previous.Posted) || mail.Posted.Equal(previous.Posted) && mail.ID > previous.ID {
				latest[id] = mail
			}
		}
	}
	singleLine := func(text string) string {
		return strings.Join(strings.Fields(style.SanitizeImageText(text)), " ")
	}
	name := func(id string) string {
		if id == parent {
			return "parent"
		}
		if member := s.Members[id]; member != nil {
			return singleLine(swarmMemberLabel(s, member))
		}
		return singleLine(id)
	}
	previews := make(map[string]*agentMessagePreview, len(latest))
	for id, mail := range latest {
		text := singleLine(mail.Text)
		if runes := []rune(text); len(runes) > agentMessageLimit {
			text = string(runes[:agentMessageLimit-1]) + "…"
		}
		peer, outgoing := mail.From, mail.From == id
		if outgoing {
			peer = mail.To
		}
		previews[id] = &agentMessagePreview{peer: name(peer), text: text, outgoing: outgoing, queued: !mail.Delivered}
	}
	return previews
}

// The preview names only the other participant, relative to the listed
// agent. Keep direction, peer, text and delivery status on one clickable row.
func (p *agentMessagePreview) row(width int) string {
	if p == nil {
		return ""
	}
	direction := "from "
	if p.outgoing {
		direction = "to "
	}
	suffix := ""
	if p.queued {
		suffix = " · queued"
	}
	room := width - len(agentDetailIndent) - rw.StringWidth(direction+": "+suffix)
	if room < 2 {
		return ""
	}
	// A long name must not crowd the message text out of the preview.
	nameWidth := min(rw.StringWidth(p.peer), max(room/2, room-rw.StringWidth(p.text)))
	text := direction + rw.Truncate(p.peer, nameWidth, "…") + ": " + rw.Truncate(p.text, room-nameWidth, "…") + suffix
	return agentDetailIndent + style.Styled(text, "muted", "")
}
