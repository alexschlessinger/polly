package swarm

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/sessions"
)

// swarmFormatVersion is the record shape this build reads and writes. A root
// records it on its first coordination mutation; every read checks it.
const swarmFormatVersion = 2

// FormatRecord names the shape a root's swarm records were written in.
type FormatRecord struct {
	Version int `json:"version"`
}

// ErrUnsupportedFormat reports swarm records this build cannot read. There is
// no migration: delegated work continues in a new root.
var ErrUnsupportedFormat = errors.New("unsupported swarm record format")

// The format is one record per root.
const formatKind, formatID = "format", "swarm"

// swarmRecordsPresent reports whether the root holds records that need a
// format. The parent's own journal (parent_turn) is written on every root
// turn, swarm or not, and never requires one.
func swarmRecordsPresent(raw *sessions.CoordinationState) bool {
	for kind, group := range raw.Records {
		if kind == formatKind || kind == "parent_turn" {
			continue
		}
		if len(group) > 0 {
			return true
		}
	}
	return false
}

func decodeFormat(raw *sessions.CoordinationState) (*FormatRecord, error) {
	data, ok := raw.Records[formatKind][formatID]
	if !ok {
		return nil, nil
	}
	var f FormatRecord
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("read swarm %s: %w", formatKind, err)
	}
	return &f, nil
}

func unsupportedFormat(root string, found *FormatRecord) error {
	have := "no format record"
	if found != nil {
		have = fmt.Sprintf("format version %d", found.Version)
	}
	return fmt.Errorf("swarm %s: %w: records carry %s, this build requires format version %d; recreate the swarm (no migration is provided)", root, ErrUnsupportedFormat, have, swarmFormatVersion)
}
