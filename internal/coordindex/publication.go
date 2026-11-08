package coordindex

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

// Publication is the format-2 indexing projection, not an authorized mutation.
// IDs come from record keys; snapshot commits are resolved by the storage reader.
type Publication struct {
	ID          string    `json:"id"`
	Author      string    `json:"author"`
	Run         string    `json:"run"`
	Kind        string    `json:"kind"`
	Scope       string    `json:"scope"`
	Key         string    `json:"key"`
	Value       string    `json:"value"`
	Text        string    `json:"text"`
	Snapshot    string    `json:"snapshot"`
	DuplicateOf string    `json:"duplicateOf"`
	Supersedes  string    `json:"supersedes"`
	Posted      time.Time `json:"posted"`
	AdmittedTo  []string  `json:"admittedTo"`
}

func PublicationScope(p Publication) string {
	if p.Scope != "" {
		return p.Scope
	}
	if p.Kind == "host" {
		return "machine"
	}
	return "run"
}

// PublicationFactKey preserves byte identity and the existing exact equivalence:
// normalized scope, non-machine run, key, value (or trimmed text), retained commit.
// An unkeyed publication is never coalesced, even with identical text.
func PublicationFactKey(p Publication, commit string) string {
	scope, run, text := PublicationScope(p), p.Run, ""
	if scope == "machine" {
		run = ""
	}
	if p.Value == "" {
		text = strings.TrimSpace(p.Text)
	}
	return Key(scope, run, p.Key, p.Value, text, commit)
}
func SamePublicationFact(a, b Publication, aCommit, bCommit string) bool {
	return PublicationFactKey(a, aCommit) == PublicationFactKey(b, bCommit)
}
func PublicationAudiences(p Publication) []string {
	scope := PublicationScope(p)
	var out []string
	if scope == "machine" {
		out = append(out, Key("machine"))
	} else {
		out = append(out, Key("run", p.Run))
	}
	if scope != "run" {
		out = append(out, Key("parent"))
	}
	return out
}
func PublicationOrder(p Publication) Order {
	return Order{Kind: "publication", ID: p.ID, Posted: p.Posted}
}
func PublicationEvent(p Publication, active, survivor bool) bool {
	return active && (p.DuplicateOf == "" || survivor)
}

type PublicationReaderState struct {
	ID, Run string
	Parent  bool
	Posted  time.Time
	MarkID  string
}

func PublicationVisible(p Publication, r PublicationReaderState) bool {
	if r.Parent {
		return PublicationScope(p) != "run"
	}
	return PublicationScope(p) == "machine" || p.Run == r.Run
}
func PublicationUnread(p Publication, r PublicationReaderState) bool {
	if p.DuplicateOf != "" {
		return !slices.Contains(p.AdmittedTo, r.ID)
	}
	return Less(Order{Kind: "publication", ID: r.MarkID, Posted: r.Posted}, PublicationOrder(p))
}
func PublicationPendingFor(p Publication, active, survivor bool, r PublicationReaderState) bool {
	return PublicationEvent(p, active, survivor) && p.Author != r.ID && PublicationVisible(p, r) && PublicationUnread(p, r)
}

// AdmissionNews is deliberately a dependency projection, not parent wait policy.
// Callers retain control/visibility decisions. Raw payload revisions are separate.
func NewsProjection(kind string, payload json.RawMessage) ([]byte, error) {
	if payload == nil {
		return nil, nil
	}
	var names []string
	switch kind {
	case "member":
		names = []string{"control", "controller", "execution"}
	case "execution":
		names = []string{"member", "run", "workflow", "generation", "status"}
	case "task":
		names = []string{"status", "owner", "execution", "snapshot", "feedback", "revision", "acceptedRevision", "deferral", "run"}
	case "workflow":
		names = []string{"status", "acknowledged", "run"}
	case "mail":
		names = []string{"task", "execution", "revision", "delivered"}
	default:
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(names))
	for _, name := range names {
		if b := fields[name]; b != nil {
			var v any
			decoder := json.NewDecoder(bytes.NewReader(b))
			decoder.UseNumber() // generations/revisions must not round through float64
			if err := decoder.Decode(&v); err != nil {
				return nil, err
			}
			// Normalize omitted zero values and queued/running lifecycle identity.
			if kind == "execution" && name == "status" {
				if v == "queued" || v == "running" {
					v = "active"
				}
			}
			if v != nil && v != "" && v != false && v != json.Number("0") {
				out[name] = v
			}
		}
	}
	return json.Marshal(out)
}

const MemberLifecycle Index = "derived.member.lifecycle"

func Lifecycle(control, executionStatus string, hasExecution bool) string {
	var execution *coordsem.Execution
	if hasExecution {
		execution = &coordsem.Execution{Status: executionStatus}
	}
	lifecycle, _ := coordsem.Activity(control, execution, "")
	return lifecycle
}
