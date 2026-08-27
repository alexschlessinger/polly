// Package artifacts describes large or binary conversation payloads. The
// authoritative bytes are owned by a session Store; transcript messages retain
// only stable, provider-neutral Ref values.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// Kind describes how an artifact may be projected back to a model.
type Kind string

const (
	KindText   Kind = "text"
	KindImage  Kind = "image"
	KindBinary Kind = "binary"
)

// Ref is the durable, provider-neutral description stored in a transcript.
// ID is an opaque, canonical SHA-256 content identifier.
type Ref struct {
	ID         string `json:"id"`
	Kind       Kind   `json:"kind"`
	MIMEType   string `json:"mime_type,omitempty"`
	Name       string `json:"name,omitempty"`
	ImageToken string `json:"image_token,omitempty"`
	Reference  string `json:"reference,omitempty"`
	Bytes      int64  `json:"bytes"`
	Lines      int    `json:"lines,omitempty"`
}

// Blob is a payload plus the display/projection metadata used to build Ref.
type Blob struct {
	Kind       Kind
	MIMEType   string
	Name       string
	ImageToken string
	Reference  string
	Data       []byte
}

// Store owns immutable artifact bytes for one session. RemoveAll removes only
// that session's ownership; implementations may retain bytes shared by another
// session.
type Store interface {
	Put(context.Context, Blob) (Ref, error)
	Open(context.Context, string) (io.ReadCloser, error)
	RemoveAll(context.Context) error
}

var ErrInvalidID = errors.New("invalid artifact id")

const idPrefix = "sha256:"

// ValidID reports whether id is a canonical full artifact ID.
func ValidID(id string) bool {
	if !strings.HasPrefix(id, idPrefix) || len(id) != len(idPrefix)+sha256.Size*2 {
		return false
	}
	digest := strings.TrimPrefix(id, idPrefix)
	if digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// RefForBlob derives the durable reference metadata for blob. Storage
// implementations use this before atomically persisting blob.Data.
func RefForBlob(blob Blob) Ref {
	sum := sha256.Sum256(blob.Data)
	id := idPrefix + hex.EncodeToString(sum[:])
	lines := 0
	if blob.Kind == KindText {
		lines = lineCount(blob.Data)
	}
	imageToken := blob.ImageToken
	if blob.Kind == KindImage && imageToken == "" {
		if strings.HasPrefix(blob.Reference, "[image ") && strings.HasSuffix(blob.Reference, "]") {
			imageToken = blob.Reference
		} else {
			imageToken = "[image " + id + "]"
		}
	}
	return Ref{
		ID:         id,
		Kind:       blob.Kind,
		MIMEType:   blob.MIMEType,
		Name:       blob.Name,
		ImageToken: imageToken,
		Reference:  blob.Reference,
		Bytes:      int64(len(blob.Data)),
		Lines:      lines,
	}
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}
