package main

import (
	"image"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

func TestInspectorTitleReservesActions(t *testing.T) {
	progress := []inspectorHeaderAction{{"Stop", "stop", true, false}}
	both := []inspectorHeaderAction{{"Stop", "stop", true, false}, {"Review approval", "review", true, true}}
	for _, tc := range []struct {
		name, title, want string
		width             int
		actions           []inspectorHeaderAction
	}{
		{"short", "Agent", "Agent · Stop", 50, progress},
		{"long", "A very long agent title", "A very long agen… · Stop", 24, progress},
		{"unicode", "調査 e\u0301 long name", "調査 e\u0301 long name · Stop", 24, progress},
		{"literal markup", "[note] code", "[note] code · Stop", 50, progress},
		{"minimum title", "Agent", "… · Stop · Review approval", 26, both},
		{"overflow controls", "Agent", "… · Stop\nReview approval", 25, both},
		{"canceling", "Agent", "Agent · Stop", 50, []inspectorHeaderAction{{"Stop", "stop", false, false}}},
		{"approval", "Agent", "Agent · Stop · Review approval", 50, both},
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
