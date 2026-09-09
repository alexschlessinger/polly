package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TitleSource identifies who owns a descriptive title, independently of the
// session's resume handle and retention policy.
type TitleSource string

const (
	TitleSourceAgent TitleSource = "agent"
	TitleSourceUser  TitleSource = "user"
)

var (
	ErrInvalidTitle   = errors.New("invalid session title")
	ErrTitleProtected = errors.New("session title is user-owned")
)

// TitleSession is an optional capability for updating a session's display
// title. Only this operation changes title ownership; settings writes cannot.
type TitleSession interface {
	SetTitle(context.Context, string, TitleSource) (string, error)
}

// DisplayLabel returns the human-facing label without changing the resume
// handle. A child's original brief supplies a fallback for older sessions.
func DisplayLabel(md *Metadata) string {
	if md == nil {
		return ""
	}
	if title := strings.Join(strings.Fields(md.Title), " "); title != "" {
		return title
	}
	if md.Parent != "" {
		if description := strings.Join(strings.Fields(md.Description), " "); description != "" {
			return description
		}
	}
	return md.Name
}

func normalizeTitle(title string) (string, error) {
	if !utf8.ValidString(title) {
		return "", fmt.Errorf("%w: text must be valid UTF-8", ErrInvalidTitle)
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" || utf8.RuneCountInString(title) > 80 {
		return "", fmt.Errorf("%w: use 1 to 80 characters", ErrInvalidTitle)
	}
	for _, r := range title {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: control characters are not allowed", ErrInvalidTitle)
		}
	}
	return title, nil
}

func canonicalizeTitle(md *Metadata) {
	if md.Title == "" {
		md.TitleSource = ""
	} else if md.TitleSource != TitleSourceAgent && md.TitleSource != TitleSourceUser {
		md.TitleSource = TitleSourceUser
	}
}

func preserveTitle(md, current *Metadata) {
	md.Title, md.TitleSource = current.Title, current.TitleSource
}

func (s *sqliteSession) SetTitle(ctx context.Context, title string, source TitleSource) (string, error) {
	title, err := normalizeTitle(title)
	if err != nil {
		return "", err
	}
	if source != TitleSourceAgent && source != TitleSourceUser {
		return "", fmt.Errorf("%w: unknown title source", ErrInvalidTitle)
	}
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()
	err = s.store.withWrite(opCtx, func(conn *sql.Conn) error {
		if err := s.requireLease(opCtx, conn); err != nil {
			return err
		}
		snap, _, err := scanSnapshot(opCtx, conn, s.id)
		if err != nil {
			return err
		}
		md, err := metadataFromSnapshot(snap)
		if err != nil {
			return err
		}
		if source == TitleSourceAgent && md.TitleSource == TitleSourceUser {
			return ErrTitleProtected
		}
		if md.Title == title && md.TitleSource == source {
			return nil
		}
		md.Title, md.TitleSource = title, source
		settings, err := json.Marshal(md)
		if err != nil {
			return fmt.Errorf("encode session title: %w", err)
		}
		// Titles are display metadata, not activity: leave updated_ns and all
		// retention columns alone. ReadView revisions include settings_json.
		_, err = conn.ExecContext(opCtx, "UPDATE sessions SET settings_json = ? WHERE id = ?", settings, s.id)
		return err
	})
	if err != nil {
		return "", s.mapError(ctx, err)
	}
	return title, nil
}
