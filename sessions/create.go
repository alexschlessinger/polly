package sessions

import (
	"context"
	"errors"
	"fmt"
)

// ErrSessionExists reports that Create found a session with the name.
var ErrSessionExists = errors.New("session already exists")

// SessionCreator is optional. Create is Acquire for a session that must not
// exist yet, and stores it as the caller wants it to start in the transaction
// that creates it. configure, when given, edits the metadata the session
// starts with, the store's defaults, and may give it a title; the store sets
// the name, the parent and the times after it. It may run more than once and
// must not call the store.
type SessionCreator interface {
	Create(ctx context.Context, name string, options AcquireOptions, configure func(*Metadata)) (Session, error)
}

func (s *SQLiteStore) Create(ctx context.Context, name string, options AcquireOptions, configure func(*Metadata)) (Session, error) {
	if options.ExistingOnly || options.ExpectedID != "" {
		return nil, errors.New("a session to create cannot be an existing one")
	}
	options.create, options.configure = true, configure
	return s.Acquire(ctx, name, options)
}

// configureNew applies a Create's configure to the metadata of the session
// being created and checks what it left.
func configureNew(metadata *Metadata, configure func(*Metadata)) error {
	if configure == nil {
		return nil
	}
	configure(metadata)
	metadata.Transcript = nil
	if metadata.TTL < 0 {
		return fmt.Errorf("session TTL cannot be negative")
	}
	if metadata.Title == "" {
		metadata.TitleSource = ""
		return nil
	}
	title, err := normalizeTitle(metadata.Title)
	if err != nil {
		return err
	}
	if !metadata.TitleSource.valid() {
		return fmt.Errorf("%w: unknown title source", ErrInvalidTitle)
	}
	metadata.Title = title
	return nil
}
