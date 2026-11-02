package main

import (
	"image"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

func TestInspectorTitleReservesActions(t *testing.T) {
	progress := []inspectorHeaderAction{{"Watch ↗", "watch", true, false}, {"Stop", "stop", true, false}}
	for _, tc := range []struct {
		name, title, want string
		width             int
		actions           []inspectorHeaderAction
	}{
		{"short", "Agent", "Agent · Watch ↗ · Stop", 50, progress},
		{"long", "A very long agent title", "A very… · Watch ↗ · Stop", 24, progress},
		{"unicode", "調査 e\u0301 long name", "調査 e\u0301… · Watch ↗ · Stop", 24, progress},
		{"literal markup", "[note] code", "[note] code · Watch ↗ · Stop", 50, progress},
		{"minimum title", "Agent", "… · Watch ↗ · Stop", 18, progress},
		{"overflow controls", "Agent", "… · Watch ↗\nStop", 17, progress},
		{"watch", "Agent", "Agent · Back to inspection · Message… · Stop", 80, []inspectorHeaderAction{
			{"Back to inspection", "inspection", true, false},
			{"Message…", "message", true, false},
			{"Stop", "stop", true, false},
		}},
		{"canceling", "Agent", "Agent · Watch ↗ · Stop", 50, []inspectorHeaderAction{
			{"Watch ↗", "watch", true, false},
			{"Stop", "stop", false, false},
		}},
		{"approval", "Agent", "Agent · Watch ↗ · Review approval", 50, []inspectorHeaderAction{
			{"Watch ↗", "watch", true, false},
			{"Review approval", "review", true, true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := image.Pt(7, 3)
			b := inspectorHeaderBuilder{width: tc.width, origin: origin}
			b.title(tc.title, tc.actions...)
			header := b.layout(10)
			if got := plainStyledText(header.text); got != tc.want || header.rows != strings.Count(tc.want, "\n")+1 {
				t.Fatalf("heading %q (%d rows), want %q", got, header.rows, tc.want)
			}
			checkInspectorHeaderGeometry(t, header, image.Rectangle{Min: origin, Max: origin.Add(image.Pt(tc.width, header.rows))})
			if title := headerButton(header.buttons, "parent"); title.Empty() || title.Min != origin {
				t.Fatalf("truncated title lost its parent link: %v", title)
			}
			for _, action := range tc.actions {
				if button := headerButton(header.buttons, action.action); button.Empty() == action.enabled {
					t.Fatalf("action %q enabled=%v has hitbox %v", action.action, action.enabled, button)
				}
				if !action.enabled && !strings.Contains(header.text, style.Styled(action.label, "muted", "")) {
					t.Fatalf("disabled action not muted: %s", header.text)
				}
				if action.selected && !strings.Contains(header.text, style.Styled(action.label, "active", "bold")) {
					t.Fatalf("approval action lost its emphasis: %s", header.text)
				}
			}
			// Height clipping must not leave the wrapped controls clickable.
			clipped := b.layout(1)
			checkInspectorHeaderGeometry(t, clipped, image.Rectangle{Min: origin, Max: origin.Add(image.Pt(tc.width, 1))})
		})
	}
}
