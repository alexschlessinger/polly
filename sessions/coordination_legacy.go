package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/messages"
)

// OpenCoordinationAccess selects only capabilities on handle. It never opens
// a path, unwraps a host adapter or discovers a different authoritative store.
// The legacy implementation materializes the family once per operation; its
// revision tokens deliberately invalidate on every new callback.
func OpenCoordinationAccess(handle any) (CoordinationAccess, error) {
	if nilCoordinationHandle(handle) {
		return nil, ErrCoordinationUnsupported
	}
	if access, ok := handle.(CoordinationAccess); ok {
		return access, nil
	}
	if native, ok := handle.(CoordinationRecords); ok {
		return &nativeCoordinationAccess{CoordinationRecords: native, capabilities: coordinationCapabilities(handle)}, nil
	}
	if legacy, ok := handle.(CoordinationSession); ok {
		return &legacyCoordinationAccess{source: legacy}, nil
	}
	return nil, ErrCoordinationUnsupported
}

// OpenCoordinationViewAccess supplies the same selectors for trusted root-only
// inspection, without acquiring a lease or granting mutation capability.
func OpenCoordinationViewAccess(handle any) (CoordinationViewAccess, error) {
	if nilCoordinationHandle(handle) {
		return nil, ErrCoordinationUnsupported
	}
	if access, ok := handle.(CoordinationViewAccess); ok {
		return access, nil
	}
	if native, ok := handle.(CoordinationRecordsViewStore); ok {
		return &nativeCoordinationViewAccess{CoordinationRecordsViewStore: native, capabilities: coordinationCapabilities(handle)}, nil
	}
	if legacy, ok := handle.(CoordinationViewStore); ok {
		return &legacyCoordinationViewAccess{source: legacy}, nil
	}
	return nil, ErrCoordinationUnsupported
}

func nilCoordinationHandle(handle any) bool {
	if handle == nil {
		return true
	}
	v := reflect.ValueOf(handle)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		return v.IsNil()
	}
	return false
}

func coordinationCapabilities(handle any) CoordinationCapabilities {
	if reporter, ok := handle.(CoordinationCapabilityReporter); ok {
		return reporter.CoordinationCapabilities()
	}
	return CoordinationCapabilities{}
}

type nativeCoordinationAccess struct {
	CoordinationRecords
	capabilities CoordinationCapabilities
}

func (a *nativeCoordinationAccess) Capabilities() CoordinationCapabilities { return a.capabilities }

type nativeCoordinationViewAccess struct {
	CoordinationRecordsViewStore
	capabilities CoordinationCapabilities
}

func (a *nativeCoordinationViewAccess) Capabilities() CoordinationCapabilities { return a.capabilities }

type legacyCoordinationAccess struct{ source CoordinationSession }

func (*legacyCoordinationAccess) Capabilities() CoordinationCapabilities {
	return CoordinationCapabilities{}
}

func (a *legacyCoordinationAccess) ReadRecords(ctx context.Context, fn func(CoordinationReader) error) error {
	if fn == nil {
		return fmt.Errorf("%w: callback required", ErrCoordinationRequest)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := a.source.ReadCoordination(ctx)
	if err != nil {
		return err
	}
	return readLegacyRecords(ctx, raw, fn)
}

func readLegacyRecords(ctx context.Context, raw *CoordinationState, fn func(CoordinationReader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("%w: backend returned nil state", ErrCoordinationRequest)
	}
	r := newLegacyRecordTx(ctx, raw)
	defer r.close()
	if err := r.ready(); err != nil {
		return err
	}
	if err := fn(struct{ CoordinationReader }{r}); err != nil {
		return err
	}
	return ctx.Err()
}

func (a *legacyCoordinationAccess) UpdateRecords(ctx context.Context, fn func(CoordinationTx) error) (CoordinationCommit, error) {
	if fn == nil {
		return CoordinationCommit{}, fmt.Errorf("%w: callback required", ErrCoordinationRequest)
	}
	if err := ctx.Err(); err != nil {
		return CoordinationCommit{}, err
	}
	var commit CoordinationCommit
	called := false
	err := a.source.UpdateCoordination(ctx, func(raw *CoordinationState) error {
		if called {
			return fmt.Errorf("%w: backend retried coordination callback", ErrCoordinationRequest)
		}
		called = true
		if err := ctx.Err(); err != nil {
			return err
		}
		if raw == nil {
			return fmt.Errorf("%w: backend returned nil state", ErrCoordinationRequest)
		}
		tx := newLegacyRecordTx(ctx, raw)
		defer tx.close()
		if err := tx.ready(); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		changed, err := changedCoordinationScopes(ctx, raw.ParentID, raw.Records, tx.records)
		if err != nil {
			return err
		}
		if tx.effects.ExpectedSequence != nil && *tx.effects.ExpectedSequence != raw.Sequence {
			return fmt.Errorf("%w: checkpoint sequence changed", ErrCoordinationRequest)
		}
		if raw.Sequence < 0 || int64(len(tx.effects.Append)) > int64(^uint64(0)>>1)-raw.Sequence {
			return fmt.Errorf("%w: sequence overflow", ErrCoordinationRequest)
		}
		commit = CoordinationCommit{Sequence: raw.Sequence + int64(len(tx.effects.Append)), Changed: changed}
		// Only publish the staged state after callback success. The legacy
		// backend owns lease/sequence fencing and atomic persistence of all fields.
		raw.Records = copyCoordinationRecords(tx.records)
		raw.Append = tx.effects.Append
		raw.ExpectedSequence = tx.effects.ExpectedSequence
		raw.Transcript = tx.effects.Transcript
		raw.Pins, raw.PinOwner, raw.Members = tx.effects.Pins, tx.effects.PinOwner, tx.effects.Members
		return nil
	})
	if err != nil {
		return CoordinationCommit{}, err
	}
	if !called {
		return CoordinationCommit{}, fmt.Errorf("%w: backend omitted coordination callback", ErrCoordinationRequest)
	}
	return commit, nil
}

type legacyCoordinationViewAccess struct{ source CoordinationViewStore }

func (*legacyCoordinationViewAccess) Capabilities() CoordinationCapabilities {
	return CoordinationCapabilities{}
}
func (a *legacyCoordinationViewAccess) ReadRecordsView(ctx context.Context, root string, fn func(CoordinationReader) error) error {
	if root == "" || fn == nil {
		return fmt.Errorf("%w: root and callback required", ErrCoordinationRequest)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := a.source.ReadCoordinationView(ctx, root)
	if err != nil {
		return err
	}
	if raw == nil || raw.ParentID != root || raw.ActorID != root {
		return ErrSessionNotFound
	}
	return readLegacyRecords(ctx, raw, fn)
}

type legacyRecordTx struct {
	ctx        context.Context
	active     bool
	identity   CoordinationIdentity
	records    map[string]map[string]json.RawMessage
	effects    CoordinationEffects
	nonce      string
	generation uint64
}

func newLegacyRecordTx(ctx context.Context, raw *CoordinationState) *legacyRecordTx {
	return &legacyRecordTx{ctx: ctx, active: true, identity: CoordinationIdentity{raw.ActorID, raw.ParentID, raw.Sequence}, records: copyCoordinationRecords(raw.Records), nonce: ids.New()}
}
func (r *legacyRecordTx) close() {
	r.active = false
	r.records = nil
	r.effects = CoordinationEffects{}
}
func (r *legacyRecordTx) ready() error {
	if !r.active {
		return ErrCoordinationClosed
	}
	return r.ctx.Err()
}
func (r *legacyRecordTx) Identity() (CoordinationIdentity, error) {
	if err := r.ready(); err != nil {
		return CoordinationIdentity{}, err
	}
	return r.identity, nil
}
func (r *legacyRecordTx) revision(scope RevisionScope) Revision {
	return Revision{Scope: scope, Token: r.nonce + ":" + strconv.FormatUint(r.generation, 10) + ":" + CoordinationIndexKey(string(scope.Index), scope.Key)}
}
func (r *legacyRecordTx) record(key RecordKey, payload json.RawMessage) CoordinationRecord {
	return CoordinationRecord{Key: key, Payload: bytes.Clone(payload), Revision: r.revision(RevisionScope{CoordinationIndexRecord, CoordinationIndexKey(key.Kind, key.ID)})}
}
func validCoordinationKey(key RecordKey) error {
	if key.Kind == "" || key.ID == "" {
		return fmt.Errorf("%w: kind and ID required", ErrCoordinationRequest)
	}
	return nil
}
func (r *legacyRecordTx) Get(key RecordKey) (CoordinationRecord, bool, error) {
	if err := r.ready(); err != nil {
		return CoordinationRecord{}, false, err
	}
	if err := validCoordinationKey(key); err != nil {
		return CoordinationRecord{}, false, err
	}
	payload, ok := r.records[key.Kind][key.ID]
	if !ok {
		return CoordinationRecord{}, false, nil
	}
	if err := coordindex.CheckFormat(key.Kind, r.records["format"]["swarm"]); err != nil {
		return CoordinationRecord{}, false, err
	}
	if _, _, err := coordindex.Extract(key.Kind, key.ID, payload); err != nil {
		return CoordinationRecord{}, false, err
	}
	return r.record(key, payload), true, nil
}

func normalizeCoordinationScope(scope RevisionScope) (RevisionScope, error) {
	index, key, err := coordindex.Normalize(coordindex.Index(scope.Index), scope.Key)
	return RevisionScope{CoordinationIndex(index), key}, err
}

// selectorKind avoids interpreting unrelated kinds during a narrow read.
func selectorKind(scope RevisionScope) string {
	switch scope.Index {
	case CoordinationIndexKind:
		return scope.Key
	case CoordinationIndexRecord, CoordinationIndexAdmissionNews:
		parts, _ := coordindex.SplitKey(scope.Key)
		return parts[0]
	case CoordinationIndexMailRecipient, CoordinationIndexMailPending, CoordinationIndexMailFromCall, CoordinationIndexMailPendingStartRecipient:
		return "mail"
	case CoordinationIndexMemberNamePrefix, CoordinationIndexMemberName, CoordinationIndexMemberLifecycle:
		return "member"
	case CoordinationIndexRunStatus:
		return "run"
	case CoordinationIndexFollowupMember, CoordinationIndexFollowupPreparingMember:
		return "followup"
	case CoordinationIndexExecutionMember, CoordinationIndexExecutionRun, CoordinationIndexExecutionWorkflow, CoordinationIndexExecutionStatus, CoordinationIndexExecutionRunCall, CoordinationIndexExecutionCallID:
		return "execution"
	case CoordinationIndexTaskOwner, CoordinationIndexTaskExecution, CoordinationIndexTaskDependency, CoordinationIndexTaskStatus, CoordinationIndexTaskFollowupCall, CoordinationIndexTaskOpen:
		return "task"
	case CoordinationIndexApplyUncertain:
		return "apply"
	case CoordinationIndexSnapshotSource:
		return "snapshot"
	case CoordinationIndexContextOwner, CoordinationIndexContextRelease:
		return "context"
	case CoordinationIndexWorkflowSteps, CoordinationIndexWorkflowStepID:
		return "workflow_step"
	case CoordinationIndexPublicationAudience, CoordinationIndexPublicationClaim, CoordinationIndexPublicationSupersedes, CoordinationIndexPublicationReceipt, CoordinationIndexPublicationExactReceipt, CoordinationIndexPublicationSnapshot, CoordinationIndexPublicationActive, CoordinationIndexPublicationSurvivor, CoordinationIndexPublicationPending:
		return "publication"
	}
	return ""
}

func (r *legacyRecordTx) Revisions(scopes []RevisionScope) ([]Revision, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if len(scopes) > MaxCoordinationRevisionScopes {
		return nil, fmt.Errorf("%w: at most %d revision scopes", ErrCoordinationRequest, MaxCoordinationRevisionScopes)
	}
	out := make([]Revision, 0, len(scopes))
	for _, scope := range scopes {
		if err := r.ready(); err != nil {
			return nil, err
		}
		normalized, err := normalizeCoordinationScope(scope)
		if err != nil {
			return nil, err
		}
		kind := selectorKind(normalized)
		for k, records := range r.records {
			if (kind == "" || k == kind) && len(records) != 0 {
				if err := coordindex.CheckFormat(k, r.records["format"]["swarm"]); err != nil {
					return nil, err
				}
			}
		}
		if normalized.Index == CoordinationIndexAdmissionNews {
			parts, _ := coordindex.SplitKey(normalized.Key)
			projection, err := coordindex.NewsProjection(parts[0], r.records[parts[0]][parts[1]])
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(projection)
			out = append(out, Revision{Scope: normalized, Token: r.nonce + ":" + base64.RawURLEncoding.EncodeToString(digest[:])})
		} else {
			out = append(out, r.revision(normalized))
		}
	}
	return out, nil
}

// Cursor strings are opaque bytes, not necessarily UTF-8. Byte slices use
// JSON's base64 encoding so neither binding nor ordering loses byte identity.
// Version 2 rejects the previous lossy string representation.
type coordinationCursor struct {
	Version      int
	Parent, Kind []byte
	Index        CoordinationIndex
	Key          []byte
	Order        coordinationCursorOrder
}

type coordinationCursorOrder struct {
	Kind, ID, Workflow []byte
	Posted             time.Time
	Step               int64
}

func (r *legacyRecordTx) Query(query RecordQuery) (RecordPage, error) {
	if err := r.ready(); err != nil {
		return RecordPage{}, err
	}
	if err := validateCoordinationQuery(query); err != nil {
		return RecordPage{}, err
	}
	scope, err := normalizeCoordinationScope(RevisionScope{query.Index, query.Key})
	if err != nil {
		return RecordPage{}, err
	}
	if isRevisionOnlyCoordinationIndex(scope.Index) {
		return RecordPage{}, ErrCoordinationSelector
	}
	var derived *memoryDerivedStore
	if isDerivedSelector(scope.Index) {
		if err := coordindex.CheckFormat("publication", r.records["format"]["swarm"]); err != nil {
			return RecordPage{}, err
		}
		derived, err = buildDerivedMemory(r.ctx, r.records, r.identity.ParentID)
		if err != nil {
			return RecordPage{}, err
		}
	}
	kind := selectorKind(scope)
	if query.Kind != "" {
		if kind != "" && kind != query.Kind {
			return RecordPage{}, fmt.Errorf("%w: kind conflicts with selector", ErrCoordinationSelector)
		}
		kind = query.Kind
	}
	cursor := coordinationCursor{Version: 2, Parent: []byte(r.identity.ParentID), Kind: []byte(query.Kind), Index: scope.Index, Key: []byte(scope.Key)}
	var after *coordindex.Order
	if query.After != "" {
		data, err := base64.RawURLEncoding.DecodeString(query.After)
		var found coordinationCursor
		if err != nil || json.Unmarshal(data, &found) != nil || found.Version != cursor.Version || !bytes.Equal(found.Parent, cursor.Parent) || !bytes.Equal(found.Kind, cursor.Kind) || found.Index != cursor.Index || !bytes.Equal(found.Key, cursor.Key) || len(found.Order.Kind) == 0 || len(found.Order.ID) == 0 {
			return RecordPage{}, ErrCoordinationCursor
		}
		after = &coordindex.Order{
			Kind: string(found.Order.Kind), ID: string(found.Order.ID), Workflow: string(found.Order.Workflow),
			Posted: found.Order.Posted, Step: found.Order.Step,
		}
	}
	type selected struct {
		key     RecordKey
		order   coordindex.Order
		payload json.RawMessage
	}
	var rows []selected
	for k, group := range r.records {
		if kind != "" && k != kind {
			continue
		}
		for id, payload := range group {
			if err := r.ready(); err != nil {
				return RecordPage{}, err
			}
			if scope.Index == CoordinationIndexRecord && CoordinationIndexKey(k, id) != scope.Key {
				continue
			}
			if err := coordindex.CheckFormat(k, r.records["format"]["swarm"]); err != nil {
				return RecordPage{}, err
			}
			relations, order, err := coordindex.Extract(k, id, payload)
			if err != nil {
				return RecordPage{}, err
			}
			if derived != nil {
				relations = nil
				for relation := range derived.relations[RecordKey{k, id}] {
					relations = append(relations, relation)
				}
			}
			for _, relation := range relations {
				if relation.Index == coordindex.Index(scope.Index) && relation.Key == scope.Key {
					rows = append(rows, selected{RecordKey{k, id}, order, payload})
					break
				}
			}
		}
	}
	page := RecordPage{Total: -1}
	if query.IncludeTotal {
		page.Total = len(rows)
	}
	if query.CountOnly {
		if query.IncludeLast && len(rows) > 0 {
			last := rows[0]
			for _, row := range rows[1:] {
				if coordindex.Less(last.order, row.order) {
					last = row
				}
			}
			key := last.key
			page.Last = &key
		}
		return page, r.ready()
	}
	sort.Slice(rows, func(i, j int) bool { return coordindex.Less(rows[i].order, rows[j].order) })
	start := 0
	if after != nil {
		start = sort.Search(len(rows), func(i int) bool { return coordindex.Less(*after, rows[i].order) })
	}
	if query.Offset > len(rows)-start {
		start = len(rows)
	} else {
		start += query.Offset
	}
	bytesUsed, end := 0, start
	for end < len(rows) && len(page.Records) < query.Limit {
		row := rows[end]
		if query.MaxBytes > 0 && len(row.payload) > query.MaxBytes-bytesUsed {
			if len(page.Records) > 0 {
				break
			}
			page.Oversized = true
		}
		page.Records = append(page.Records, r.record(row.key, row.payload))
		bytesUsed += len(row.payload)
		end++
		if page.Oversized {
			break
		}
	}
	if end < len(rows) && end > start {
		order := rows[end-1].order
		cursor.Order = coordinationCursorOrder{
			Kind: []byte(order.Kind), ID: []byte(order.ID), Workflow: []byte(order.Workflow),
			Posted: order.Posted, Step: order.Step,
		}
		data, err := json.Marshal(cursor)
		if err != nil {
			return RecordPage{}, fmt.Errorf("encode coordination cursor: %w", err)
		}
		page.Next = base64.RawURLEncoding.EncodeToString(data)
	}
	if err := r.ready(); err != nil {
		return RecordPage{}, err
	}
	return page, nil
}

func (r *legacyRecordTx) Put(key RecordKey, payload json.RawMessage) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := validCoordinationKey(key); err != nil {
		return err
	}
	if _, _, err := coordindex.Extract(key.Kind, key.ID, payload); err != nil {
		return err
	}
	if bytes.Equal(r.records[key.Kind][key.ID], payload) {
		return nil
	}
	if r.records[key.Kind] == nil {
		r.records[key.Kind] = map[string]json.RawMessage{}
	}
	r.records[key.Kind][key.ID] = bytes.Clone(payload)
	r.generation++
	return nil
}
func (r *legacyRecordTx) Delete(key RecordKey) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := validCoordinationKey(key); err != nil {
		return err
	}
	if _, ok := r.records[key.Kind][key.ID]; ok {
		delete(r.records[key.Kind], key.ID)
		r.generation++
	}
	return nil
}
func (r *legacyRecordTx) Effects(effects CoordinationEffects) error {
	if err := r.ready(); err != nil {
		return err
	}
	old := r.effects
	if old.ExpectedSequence != nil && effects.ExpectedSequence != nil && *old.ExpectedSequence != *effects.ExpectedSequence || old.PinOwner != "" && effects.PinOwner != "" && old.PinOwner != effects.PinOwner || old.Transcript != nil && effects.Transcript != nil && (old.Transcript.Execution != effects.Transcript.Execution || old.Transcript.Generation != effects.Transcript.Generation) {
		return fmt.Errorf("%w: conflicting coordination effects", ErrCoordinationRequest)
	}
	if effects.Transcript != nil && (effects.Transcript.Execution == "" || effects.Transcript.Generation <= 0) {
		return fmt.Errorf("%w: transcript identity", ErrCoordinationRequest)
	}
	// JSON round-trip matches persistence and owns even nested message metadata.
	var appendCopy []messages.ChatMessage
	if len(effects.Append) != 0 {
		data, err := json.Marshal(effects.Append)
		if err != nil {
			return fmt.Errorf("copy coordination append: %w", err)
		}
		if err := json.Unmarshal(data, &appendCopy); err != nil {
			return fmt.Errorf("copy coordination append: %w", err)
		}
	}
	r.effects.Append = append(r.effects.Append, appendCopy...)
	r.effects.Pins = append(r.effects.Pins, effects.Pins...)
	r.effects.Members = append(r.effects.Members, effects.Members...)
	if effects.PinOwner != "" {
		r.effects.PinOwner = effects.PinOwner
	}
	if effects.ExpectedSequence != nil {
		value := *effects.ExpectedSequence
		r.effects.ExpectedSequence = &value
	}
	if effects.Transcript != nil {
		value := *effects.Transcript
		r.effects.Transcript = &value
	}
	return nil
}

func copyCoordinationRecords(records map[string]map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	out := make(map[string]map[string]json.RawMessage, len(records))
	for kind, group := range records {
		copy := make(map[string]json.RawMessage, len(group))
		for id, payload := range group {
			copy[id] = bytes.Clone(payload)
		}
		out[kind] = copy
	}
	return out
}

func changedCoordinationScopes(ctx context.Context, parent string, before, after map[string]map[string]json.RawMessage) ([]RevisionScope, error) {
	scopes := map[RevisionScope]bool{}
	visit := func(a, b map[string]map[string]json.RawMessage) error {
		for kind, group := range a {
			for id, payload := range group {
				if other, ok := b[kind][id]; ok && bytes.Equal(payload, other) {
					continue
				}
				relations, _, err := coordindex.Extract(kind, id, payload)
				if err != nil {
					return err
				}
				for _, relation := range relations {
					scopes[RevisionScope{CoordinationIndex(relation.Index), relation.Key}] = true
				}
				news, err := derivedNewsChanged(kind, payload, b[kind][id])
				if err != nil {
					return err
				}
				if news {
					scopes[RevisionScope{CoordinationIndexAdmissionNews, coordindex.Key(kind, id)}] = true
				}
			}
		}
		return nil
	}
	if err := visit(before, after); err != nil {
		return nil, err
	}
	if err := visit(after, before); err != nil {
		return nil, err
	}
	left, err := buildDerivedMemory(ctx, before, parent)
	if err != nil {
		return nil, err
	}
	right, err := buildDerivedMemory(ctx, after, parent)
	if err != nil {
		return nil, err
	}
	for _, pair := range [][2]*memoryDerivedStore{{left, right}, {right, left}} {
		for key, links := range pair[0].relations {
			for rel, order := range links {
				if !isDerivedSelector(CoordinationIndex(rel.Index)) {
					continue
				}
				other, exists := pair[1].relations[key][rel]
				if !exists || coordindex.Less(order, other) || coordindex.Less(other, order) || !bytes.Equal(before[key.Kind][key.ID], after[key.Kind][key.ID]) {
					scopes[RevisionScope{CoordinationIndex(rel.Index), rel.Key}] = true
				}
			}
		}
	}
	out := make([]RevisionScope, 0, len(scopes))
	for scope := range scopes {
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}
