package codex

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// Usage is what the backend reports about the account's plan on every
// response: a short window and a long one, each as the share used and
// when it resets.
type Usage struct {
	Primary   Window
	Secondary Window
}

// Window is one rate-limit window.
type Window struct {
	// UsedPercent is how much of the window's allowance is spent.
	UsedPercent float64
	// WindowMinutes is the window's length; zero when not stated.
	WindowMinutes int
	// ResetAt is when the window opens again; zero when not stated.
	ResetAt time.Time
}

// parseUsage reads the meters off a response's headers; ok is false when
// the response carried none.
func parseUsage(h http.Header) (Usage, bool) {
	var u Usage
	ok := false
	for name, w := range map[string]*Window{"primary": &u.Primary, "secondary": &u.Secondary} {
		if v := h.Get("x-codex-" + name + "-used-percent"); v != "" {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				w.UsedPercent, ok = f, true
			}
		}
		if v := h.Get("x-codex-" + name + "-window-minutes"); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				w.WindowMinutes = n
			}
		}
		if v := h.Get("x-codex-" + name + "-reset-at"); v != "" {
			w.ResetAt = parseTimestamp(v)
		}
	}
	return u, ok
}

// parseTimestamp reads a time as Unix seconds or RFC 3339; zero otherwise.
func parseTimestamp(v string) time.Time {
	v = strings.TrimSpace(v)
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Unix(int64(secs), 0).UTC()
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// metadata renders the meters as plain JSON values for a reply's metadata.
func (u Usage) metadata() map[string]any {
	return map[string]any{"primary": u.Primary.metadata(), "secondary": u.Secondary.metadata()}
}

func (w Window) metadata() map[string]any {
	m := map[string]any{"used_percent": w.UsedPercent}
	if w.WindowMinutes > 0 {
		m["window_minutes"] = w.WindowMinutes
	}
	if !w.ResetAt.IsZero() {
		m["reset_at"] = w.ResetAt.UTC().Format(time.RFC3339)
	}
	return m
}

// UsageFrom reads the meters a reply recorded, whether it was just
// produced or reloaded from a session.
func UsageFrom(msg messages.ChatMessage) (Usage, bool) {
	provider, _ := msg.Metadata[MetadataKey].(map[string]any)
	raw, _ := provider["usage"].(map[string]any)
	if raw == nil {
		return Usage{}, false
	}
	return Usage{Primary: windowFrom(raw["primary"]), Secondary: windowFrom(raw["secondary"])}, true
}

func windowFrom(raw any) Window {
	m, _ := raw.(map[string]any)
	var w Window
	switch v := m["used_percent"].(type) {
	case float64:
		w.UsedPercent = v
	case int:
		w.UsedPercent = float64(v)
	}
	switch v := m["window_minutes"].(type) {
	case float64:
		w.WindowMinutes = int(v)
	case int:
		w.WindowMinutes = v
	}
	if s, ok := m["reset_at"].(string); ok {
		w.ResetAt = parseTimestamp(s)
	}
	return w
}
