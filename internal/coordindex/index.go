// Package coordindex freezes format-2 coordination selector and projection
// semantics without importing swarm or sessions. It never authorizes actors or
// applies domain transitions.
package coordindex

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

type Index string

const (
	Records                 Index = "records"
	Record                  Index = "record"
	Kind                    Index = "kind"
	MailRecipient           Index = "mail.recipient"
	MailPending             Index = "mail.pending"
	MemberNamePrefix        Index = "member.name-prefix"
	ExecutionMember         Index = "execution.member"
	ExecutionRun            Index = "execution.run"
	ExecutionWorkflow       Index = "execution.workflow"
	ExecutionStatus         Index = "execution.status"
	TaskOwner               Index = "task.owner"
	TaskExecution           Index = "task.execution"
	TaskDependency          Index = "task.dependency"
	TaskStatus              Index = "task.status"
	ContextOwner            Index = "context.owner"
	ContextRelease          Index = "context.release"
	WorkflowSteps           Index = "workflow.steps"
	PublicationAudience     Index = "publication.audience"
	PublicationClaim        Index = "publication.claim"
	PublicationSupersedes   Index = "publication.supersedes"
	PublicationReceipt      Index = "publication.receipt"
	PublicationExactReceipt Index = "publication.exact-receipt"
	PublicationSnapshot     Index = "publication.snapshot"
	OverviewDependency      Index = "overview.dependency"
	RunStatus               Index = "run.status"
	FollowupMember          Index = "followup.member"
	FollowupPreparingMember Index = "followup.preparing-member"
	PublicationActive       Index = "derived.publication.active"
	PublicationSurvivor     Index = "derived.publication.survivor"
	PublicationPending      Index = "derived.publication.pending"
	AdmissionNews           Index = "derived.admission.news"
	SemanticNews            Index = "semantic.news"
	SemanticMail            Index = "semantic.mail"
	Display                 Index = "display"
	DisplayFact             Index = "display.fact"

	// Internal materialized dependency partitions, not public query selectors.
	PublicationEquivalent Index = "derived.publication.equivalent"
	PublicationReader     Index = "derived.publication.reader"
	PublicationEvents     Index = "derived.publication.events"
)

var (
	ErrSelector = errors.New("unsupported coordination selector")
	ErrFormat   = errors.New("unsupported coordination record format")
)

// Key is an unambiguous, byte-preserving tuple codec. Each component has a
// canonical unsigned-varint byte length; the whole tuple is raw URL base64.
func Key(parts ...string) string {
	var b []byte
	for _, part := range parts {
		b = binary.AppendUvarint(b, uint64(len(part)))
		b = append(b, part...)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func SplitKey(key string) ([]string, error) {
	b, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed tuple", ErrSelector)
	}
	var parts []string
	for len(b) > 0 {
		n, used := binary.Uvarint(b)
		if used <= 0 || n > uint64(len(b)-used) {
			return nil, fmt.Errorf("%w: malformed tuple", ErrSelector)
		}
		parts = append(parts, string(b[used:used+int(n)]))
		b = b[used+int(n):]
	}
	if Key(parts...) != key {
		return nil, fmt.Errorf("%w: noncanonical tuple", ErrSelector)
	}
	return parts, nil
}

// Normalize validates selectors and normalizes only a trailing slash on a
// canonical member prefix. Scalar keys are otherwise exact, including empty
// fields (e.g. context.release="" means not marked for release).
func Normalize(index Index, key string) (Index, string, error) {
	if index == "" {
		index = Records
	}
	arity := 0
	switch index {
	case Records, SemanticNews, Display:
		if key != "" {
			return "", "", fmt.Errorf("%w: %s takes no key", ErrSelector, index)
		}
	case Record, OverviewDependency, PublicationExactReceipt, AdmissionNews, DisplayFact:
		arity = 2
	case ExecutionRunCall, MailFromCall, TaskFollowupCall, WorkflowStepID:
		arity = 2
	case Index("semantic.open-task"), Index("semantic.uncertain-apply"):
		if key != "" {
			return "", "", fmt.Errorf("%w: semantic selector takes no key", ErrSelector)
		}
	case Index("semantic.step-call"), Index("semantic.member-mail"), SemanticMail:
	case MemberName, MailPendingStartRecipient, SnapshotSource, ExecutionCallID:
		// Exact scalar values: unlike MemberNamePrefix, no path normalization.
	case PublicationClaim:
		arity = 3 // normalized scope, run (empty for machine), fact key
	case PublicationAudience, PublicationActive, PublicationSurvivor:
		parts, err := SplitKey(key)
		if err != nil {
			return "", "", err
		}
		if len(parts) == 1 && (parts[0] == "machine" || parts[0] == "parent") || len(parts) == 2 && parts[0] == "run" {
			return index, key, nil
		}
		return "", "", fmt.Errorf("%w: publication audience", ErrSelector)
	case MemberNamePrefix:
		key = strings.TrimSuffix(key, "/")
		if key != "" && key != "/root" && !strings.HasPrefix(key, "/root/") {
			return "", "", fmt.Errorf("%w: noncanonical member prefix", ErrSelector)
		}
		if strings.Contains(key, "//") || strings.HasSuffix(key, "/") {
			return "", "", fmt.Errorf("%w: member prefix", ErrSelector)
		}
	case Kind, MailRecipient, MailPending, ExecutionMember, ExecutionRun, ExecutionWorkflow, ExecutionStatus, TaskOwner, TaskExecution, TaskDependency, TaskStatus, ContextOwner, ContextRelease, WorkflowSteps, PublicationSupersedes, PublicationReceipt, PublicationSnapshot, RunStatus, FollowupMember, FollowupPreparingMember, PublicationPending, MemberLifecycle:
	default:
		return "", "", fmt.Errorf("%w: %q", ErrSelector, index)
	}
	if arity != 0 {
		parts, err := SplitKey(key)
		if err != nil {
			return "", "", err
		}
		if len(parts) != arity {
			return "", "", fmt.Errorf("%w: %s needs %d tuple parts", ErrSelector, index, arity)
		}
		if index == PublicationClaim && (parts[2] == "" || parts[0] != "machine" && parts[0] != "run" && parts[0] != "workspace" && parts[0] != "snapshot" || parts[0] == "machine" && parts[1] != "") {
			return "", "", fmt.Errorf("%w: claim partition", ErrSelector)
		}
	}
	return index, key, nil
}

func KnownKind(kind string) bool {
	switch kind {
	case "followup", "integration", "apply", "parent_turn", "parent", "run", "member", "task", "mail", "publication", "execution", "context", "snapshot", "preview", "workflow", "workflow_step", "format":
		return true
	}
	return false
}

// CheckFormat is required before interpreting any format-dependent record.
// Unknown kinds and parent_turn do not require a swarm format. The format row
// itself can always be fetched to diagnose incompatibility.
func CheckFormat(kind string, format json.RawMessage) error {
	if !KnownKind(kind) || kind == "parent_turn" || kind == "format" {
		return nil
	}
	var f struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(format, &f); err != nil {
		return fmt.Errorf("%w: missing or malformed format/swarm", ErrFormat)
	}
	if f.Version != 2 {
		return fmt.Errorf("%w: version %d, require 2", ErrFormat, f.Version)
	}
	return nil
}

type Relation struct {
	Index Index
	Key   string
}

// Order carries the canonical ordering position independently of payload bytes.
// Kind then ID are bytewise, except Posted for mail/publication and numeric
// Step within Workflow for workflow_step. Times compare instants, not strings.
type Order struct {
	Kind     string
	ID       string
	Posted   time.Time
	Workflow string
	Step     int64
}

func Less(a, b Order) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	switch a.Kind {
	case "mail", "publication":
		if !a.Posted.Equal(b.Posted) {
			return a.Posted.Before(b.Posted)
		}
	case "workflow_step":
		if a.Workflow != b.Workflow {
			return a.Workflow < b.Workflow
		}
		if a.Step != b.Step {
			return a.Step < b.Step
		}
	}
	return a.ID < b.ID
}

// Extract returns sorted, deduplicated relations and an ordering position.
// Publication claim partitions deliberately overselect: scope/run/key only.
// Cross-record active/survivor/pending relations use the shared publication
// projection helpers. Audience authorization and all transitions remain in swarm;
// structural overview dependencies do not assert presentation or news meaning.
func Extract(kind, id string, payload json.RawMessage) ([]Relation, Order, error) {
	order := Order{Kind: kind, ID: id}
	if kind == "" || id == "" || !json.Valid(payload) {
		return nil, order, fmt.Errorf("%w: invalid record", ErrFormat)
	}
	relations := []Relation{{Kind, kind}, {Record, Key(kind, id)}, {Records, ""}}
	add := func(index Index, key string) { relations = append(relations, Relation{index, key}) }
	if !KnownKind(kind) {
		return relations, order, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, order, fmt.Errorf("%w: %s must be an object", ErrFormat, kind)
	}
	var decodeErr error
	str := func(field string) string {
		var s string
		if b, ok := fields[field]; ok {
			if err := json.Unmarshal(b, &s); err != nil {
				decodeErr = err
			}
		}
		return s
	}
	list := func(field string) []string {
		var values []string
		if b, ok := fields[field]; ok {
			if err := json.Unmarshal(b, &values); err != nil {
				decodeErr = err
			}
		}
		return values
	}
	dep := func(targetKind, targetID string) {
		if targetID != "" {
			add(OverviewDependency, Key(targetKind, targetID))
		}
	}
	fieldDep := func(targetKind, field string) { dep(targetKind, str(field)) }
	if (kind == "mail" || kind == "publication") && fields["posted"] != nil {
		if err := json.Unmarshal(fields["posted"], &order.Posted); err != nil {
			decodeErr = err
		}
	}
	switch kind {
	case "run":
		add(RunStatus, str("status"))
	case "followup":
		add(FollowupMember, str("member"))
		var refresh bool
		if b := fields["refresh"]; b != nil {
			if err := json.Unmarshal(b, &refresh); err != nil {
				decodeErr = err
			}
		}
		if refresh && str("phase") == "preparing" {
			add(FollowupPreparingMember, str("member"))
		}
	case "apply":
		if coordsem.UncertainApply(str("status")) {
			add(Index("semantic.uncertain-apply"), "")
		}
		var refs []struct {
			Task string `json:"task"`
		}
		if b := fields["tasks"]; b != nil {
			if err := json.Unmarshal(b, &refs); err != nil {
				decodeErr = err
			}
		}
		for _, ref := range refs {
			dep("task", ref.Task)
		}
	case "mail":
		add(MailRecipient, str("to"))
		var delivered bool
		if b := fields["delivered"]; b != nil {
			if err := json.Unmarshal(b, &delivered); err != nil {
				decodeErr = err
			}
		}
		if str("kind") == "request" && str("replyID") == "" || str("kind") == "reply" && !delivered {
			add(Index("semantic.member-mail"), str("from"))
		}
		var start bool
		if b := fields["start"]; b != nil {
			if err := json.Unmarshal(b, &start); err != nil {
				decodeErr = err
			}
		}
		add(MailFromCall, Key(str("from"), str("callID")))
		if !delivered {
			add(MailPending, str("to"))
			if start {
				add(MailPendingStartRecipient, str("to"))
			}
		}
		fieldDep("member", "from")
		fieldDep("member", "to")
		fieldDep("mail", "replyTo")
		fieldDep("mail", "replyID")
		fieldDep("task", "task")
		fieldDep("execution", "execution")
		fieldDep("workflow", "workflow")
	case "member":
		name := str("agentName")
		if name == "" {
			memberID := str("id")
			if memberID == "" {
				memberID = id
			}
			name = "/root/agent_" + memberID
		}
		add(MemberName, name)
		add(MemberNamePrefix, "")
		for prefix := name; prefix != ""; {
			add(MemberNamePrefix, prefix)
			i := strings.LastIndexByte(prefix, '/')
			if i <= 0 {
				break
			}
			prefix = prefix[:i]
		}
		fieldDep("context", "context")
		fieldDep("task", "task")
		fieldDep("execution", "execution")
		fieldDep("workflow", "controller")
	case "execution":
		callID, err := requestCallID(fields["request"])
		if err != nil {
			decodeErr = err
		}
		add(ExecutionRunCall, Key(str("run"), callID))
		add(ExecutionCallID, callID)
		add(ExecutionMember, str("member"))
		add(ExecutionRun, str("run"))
		add(ExecutionWorkflow, str("workflow"))
		add(ExecutionStatus, str("status"))
		fieldDep("member", "member")
		fieldDep("run", "run")
		fieldDep("workflow", "workflow")
		fieldDep("context", "workspace")
		fieldDep("snapshot", "base")
	case "task":
		if status := str("status"); status != "done" && status != "canceled" {
			add(Index("semantic.open-task"), "")
		}
		add(TaskFollowupCall, Key(str("follows"), str("followupCallID")))
		add(TaskOwner, str("owner"))
		add(TaskExecution, str("execution"))
		add(TaskStatus, str("status"))
		for _, id := range list("dependencies") {
			add(TaskDependency, id)
			dep("task", id)
		}
		fieldDep("member", "owner")
		fieldDep("execution", "execution")
		fieldDep("run", "run")
		fieldDep("task", "follows")
		fieldDep("snapshot", "snapshot")
		fieldDep("snapshot", "startingSnapshot")
		var deferral struct{ Workflow string }
		if b := fields["deferral"]; b != nil {
			if err := json.Unmarshal(b, &deferral); err != nil {
				decodeErr = err
			}
		}
		dep("workflow", deferral.Workflow)
	case "snapshot":
		add(SnapshotSource, str("source"))
	case "context":
		add(ContextOwner, str("owner"))
		add(ContextRelease, str("release"))
		fieldDep("member", "owner")
	case "workflow":
		var steps []json.RawMessage
		if b := fields["steps"]; b != nil {
			if err := json.Unmarshal(b, &steps); err != nil {
				decodeErr = err
			}
		}
		if len(steps) != 0 {
			return nil, order, fmt.Errorf("%w: embedded workflow steps", ErrFormat)
		}
		fieldDep("run", "run")
	case "workflow_step":
		report, number, ok := strings.Cut(id, "/")
		n, err := strconv.ParseInt(number, 10, 64)
		if !ok || report == "" || err != nil || n < 0 || strconv.FormatInt(n, 10) != number {
			return nil, order, fmt.Errorf("%w: workflow step key %q", ErrFormat, id)
		}
		order.Workflow, order.Step = report, n
		add(WorkflowSteps, report)
		add(WorkflowStepID, Key(report, str("id")))
		add(Index("semantic.step-call"), str("id"))
		dep("workflow", report)
	case "publication":
		scope := PublicationScope(Publication{Scope: str("scope"), Kind: str("kind")})
		run := str("run")
		for _, audience := range PublicationAudiences(Publication{Scope: scope, Run: run}) {
			add(PublicationAudience, audience)
		}
		if key := str("key"); key != "" {
			claimRun := run
			if scope == "machine" {
				claimRun = ""
			}
			add(PublicationClaim, Key(scope, claimRun, key))
		}
		if old := str("supersedes"); old != "" {
			add(PublicationSupersedes, old)
			dep("publication", old)
		}
		for _, reader := range list("admittedTo") {
			add(PublicationReceipt, reader)
			add(PublicationExactReceipt, Key(reader, id))
		}
		if snapshot := str("snapshot"); snapshot != "" {
			add(PublicationSnapshot, snapshot)
			dep("snapshot", snapshot)
		}
		fieldDep("member", "author")
		fieldDep("run", "run")
		fieldDep("publication", "duplicateOf")
	case "integration":
		for _, field := range []string{"inputs", "repairs", "pending"} {
			var inputs []struct {
				Task string `json:"task"`
				Base struct {
					ID string `json:"id"`
				} `json:"base"`
				Submitted struct {
					ID string `json:"id"`
				} `json:"submitted"`
			}
			if b := fields[field]; b != nil {
				if err := json.Unmarshal(b, &inputs); err != nil {
					decodeErr = err
				}
			}
			for _, in := range inputs {
				dep("task", in.Task)
				dep("snapshot", in.Base.ID)
				dep("snapshot", in.Submitted.ID)
			}
		}
		fieldDep("run", "run")
		fieldDep("integration", "predecessor")
		fieldDep("integration", "successor")
	}
	if decodeErr != nil {
		return nil, order, fmt.Errorf("%w: %s/%s: %v", ErrFormat, kind, id, decodeErr)
	}
	sort.Slice(relations, func(i, j int) bool {
		if relations[i].Index != relations[j].Index {
			return relations[i].Index < relations[j].Index
		}
		return relations[i].Key < relations[j].Key
	})
	return slices.Compact(relations), order, nil
}
