package sessions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

// SemanticMark is an opaque root-, rules-version- and history-epoch-bound
// observation. Retain the original mark, not a later revision, as the baseline.
type SemanticMark string

type SemanticMode string

const (
	SemanticParentNews   SemanticMode = "parent_news"
	SemanticCoordination SemanticMode = "coordination"
)

// SemanticComparison keeps the original baseline through every recheck. After
// may only be a continuation returned by a false comparison in the same mode.
type SemanticComparison struct {
	Baseline SemanticMark
	Mode     SemanticMode
	After    string
}
type SemanticChanges struct {
	Different bool
	Observed  SemanticMark
	// Continue is empty on a difference. A false result can skip heads unchanged
	// since this observation, but still compares changed heads to Baseline.
	Continue string
}

// CoordinationSemanticCapabilities distinguishes durable indexed history from
// the functional, family-sized legacy adapter. It is independent of NativeIndexed.
type CoordinationSemanticCapabilities struct{ NativeHistory bool }

// CoordinationSemanticReader is optional and callback-local, just like the
// record reader. No method grants authority.
type CoordinationSemanticReader interface {
	SemanticCapabilities() CoordinationSemanticCapabilities
	SemanticMark() (SemanticMark, error)
	SemanticPendingMail(actor string) (bool, error)
	SemanticChanges(SemanticComparison) (SemanticChanges, error)
}

// SemanticReader uses only the supplied reader. It does not discover SQLite or
// open another backend. Legacy marks carry canonical baseline values and cost
// proportional to the family; native marks contain no per-item baseline copy.
func SemanticReader(r CoordinationReader) CoordinationSemanticReader {
	if native, ok := r.(CoordinationSemanticReader); ok {
		return native
	}
	return legacySemanticReader{r}
}

type semanticToken struct {
	Root    string `json:"root"`
	Version int    `json:"version"`
	Epoch   string `json:"epoch"`
	Stamp   int64  `json:"stamp"`
}
type semanticContinuation struct {
	Baseline SemanticMark `json:"baseline"`
	Mode     SemanticMode `json:"mode"`
	Observed SemanticMark `json:"observed"`
}

func semanticEncode(value any) string {
	b, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(b)
}
func semanticDecode(token string, value any) error {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) == 0 {
		return fmt.Errorf("%w: semantic mark", ErrCoordinationCursor)
	}
	if err = json.Unmarshal(b, value); err != nil || semanticEncode(value) != token {
		return fmt.Errorf("%w: malformed semantic mark", ErrCoordinationCursor)
	}
	return nil
}
func validSemanticMode(mode SemanticMode) bool {
	return mode == SemanticParentNews || mode == SemanticCoordination
}
func semanticPosition(mark SemanticMark, current semanticToken) (semanticToken, error) {
	var token semanticToken
	if err := semanticDecode(string(mark), &token); err != nil {
		return token, err
	}
	if token.Root != current.Root || token.Version != coordsem.Version || token.Epoch != current.Epoch || token.Stamp < 0 || token.Stamp > current.Stamp {
		return token, fmt.Errorf("%w: foreign, expired or forward semantic mark", ErrCoordinationCursor)
	}
	return token, nil
}
func semanticAfter(q SemanticComparison, current semanticToken, baseline semanticToken) (int64, error) {
	if !validSemanticMode(q.Mode) {
		return 0, fmt.Errorf("%w: semantic comparison mode", ErrCoordinationRequest)
	}
	if q.After == "" {
		return baseline.Stamp, nil
	}
	var after semanticContinuation
	if err := semanticDecode(q.After, &after); err != nil {
		return 0, err
	}
	if after.Baseline != q.Baseline || after.Mode != q.Mode {
		return 0, fmt.Errorf("%w: incompatible semantic continuation", ErrCoordinationCursor)
	}
	token, err := semanticPosition(after.Observed, current)
	if err != nil {
		return 0, err
	}
	if token.Stamp < baseline.Stamp {
		return 0, fmt.Errorf("%w: backward semantic continuation", ErrCoordinationCursor)
	}
	return token.Stamp, nil
}
