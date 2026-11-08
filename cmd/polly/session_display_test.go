package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

// This functional host backend owns its history and provides replacement epochs.
// It deliberately has no native/indexed reporter: the tests prove selection and
// fencing, not SQLite work bounds or efficiency of compatibility adapters.
type displayPageStore struct {
	header                 sessions.SessionViewHeader
	history                []messages.ChatMessage
	fullReads, headerReads int
	queries                []sessions.TranscriptQuery
	beforePage             func(context.Context) error
	alterPage              func(*sessions.TranscriptPage)
	order                  []string
}

func newDisplayPageStore(count int) *displayPageStore {
	s := &displayPageStore{header: sessions.SessionViewHeader{ID: "member", Revision: "revision-1", Epoch: "epoch-1", Sequence: int64(count), Metadata: &sessions.Metadata{Name: "child", Title: "Original"}}}
	for n := range count {
		role := messages.MessageRoleAssistant
		if n%3 == 0 {
			role = messages.MessageRoleUser
		}
		s.history = append(s.history, messages.ChatMessage{Role: role, Content: fmt.Sprintf("message-%d", n), Metadata: map[string]any{"nested": map[string]any{"value": "owned"}}})
	}
	return s
}

func (s *displayPageStore) ReadView(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
	s.fullReads++
	h, err := s.ReadViewHeader(ctx, target, known)
	if err != nil {
		return nil, err
	}
	info := sessionViewFromHeader(h)
	if !info.Unchanged {
		for _, msg := range s.history {
			info.History = append(info.History, msg.Clone())
		}
	}
	return info, nil
}
func (s *displayPageStore) ReadViewHeader(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionViewHeader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.headerReads++
	s.order = append(s.order, "header")
	if target.ID != "" && target.ID != s.header.ID || target.ID == "" && target.Name != s.header.Metadata.Name {
		return nil, sessions.ErrSessionNotFound
	}
	h := s.header
	data, err := json.Marshal(s.header.Metadata)
	if err != nil {
		return nil, err
	}
	// Allocate metadata rather than decode into the stored pointer.
	h.Metadata = &sessions.Metadata{}
	if err := json.Unmarshal(data, h.Metadata); err != nil {
		return nil, err
	}
	h.Unchanged = known != "" && known == h.Revision
	return &h, nil
}
func (s *displayPageStore) ReadTranscriptPage(ctx context.Context, target sessions.ViewTarget, q sessions.TranscriptQuery) (sessions.TranscriptPage, error) {
	if err := ctx.Err(); err != nil {
		return sessions.TranscriptPage{}, err
	}
	s.queries = append(s.queries, q)
	s.order = append(s.order, "page")
	if s.beforePage != nil {
		if err := s.beforePage(ctx); err != nil {
			return sessions.TranscriptPage{}, err
		}
	}
	if target.ID != s.header.ID {
		return sessions.TranscriptPage{}, sessions.ErrSessionNotFound
	}
	if q.Epoch != "" && q.Epoch != s.header.Epoch {
		return sessions.TranscriptPage{}, sessions.ErrStaleTranscriptEpoch
	}
	through := q.Through
	if through == 0 {
		through = s.header.Sequence
	}
	if q.After < 0 || through > s.header.Sequence || q.After > through {
		return sessions.TranscriptPage{}, sessions.ErrInvalidTranscriptRange
	}
	page := sessions.TranscriptPage{ID: s.header.ID, Epoch: s.header.Epoch, First: q.After, Next: q.After, Through: through}
	if s.header.Metadata.Transcript != nil {
		identity := *s.header.Metadata.Transcript
		page.Transcript = &identity
	}
	used := 0
	for n := q.After; n < through && len(page.Messages) < q.Limit; n++ {
		data, err := json.Marshal(s.history[n])
		if err != nil {
			return sessions.TranscriptPage{}, err
		}
		if used+len(data) > q.MaxBytes && len(page.Messages) > 0 {
			break
		}
		var message messages.ChatMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return sessions.TranscriptPage{}, err
		}
		page.Messages = append(page.Messages, message)
		page.Next++
		used += len(data)
		if used > q.MaxBytes {
			page.Oversized = true
			break
		}
	}
	if s.alterPage != nil {
		s.alterPage(&page)
	}
	return page, nil
}

func TestHostDisplayPagingMetadataReuseAndReplacement(t *testing.T) {
	ctx := context.Background()
	store := newDisplayPageStore(2*hostTranscriptPageMessages + 3)
	var retained *sessions.SessionViewHeader
	info, err := readSessionDisplay(ctx, store, sessions.ViewTarget{ID: "member"}, "", &retained)
	if err != nil || len(info.History) != len(store.history) || store.fullReads != 0 || len(store.queries) != 3 {
		t.Fatalf("read=%+v queries=%d full=%d err=%v", info, len(store.queries), store.fullReads, err)
	}
	if !reflect.DeepEqual(info.History, store.history) {
		t.Fatal("paged hydration lost history")
	}
	for _, q := range store.queries {
		if q.Epoch != "epoch-1" || q.Through != store.header.Sequence || q.Limit != hostTranscriptPageMessages || q.MaxBytes != hostTranscriptPageBytes {
			t.Fatalf("query=%+v", q)
		}
	}
	model := prepareChildDisplay(info, &Config{}, 80)
	store.header.Metadata.Title = "Changed title"
	store.header.Revision = "revision-2"
	store.queries = nil
	next, err := readSessionDisplay(ctx, store, sessions.ViewTarget{ID: "member"}, info.Revision, &retained)
	if err != nil || !next.Unchanged || len(store.queries) != 0 || next.Metadata.Title != "Changed title" {
		t.Fatalf("metadata reread history: %+v %v queries=%v", next, err, store.queries)
	}
	fresh := refreshSavedDisplayMetadata(ctx, model, next, &Config{})
	if fresh.status.title != "Changed title" || model.status.title != "Original" || !reflect.DeepEqual(transcriptTexts(fresh), transcriptTexts(model)) {
		t.Fatal("metadata refresh changed history or mutated saved display")
	}
	store.header.Epoch = "epoch-2"
	store.header.Revision = "revision-3"
	store.history[0].Content = "replacement"
	replaced, err := readSessionDisplay(ctx, store, sessions.ViewTarget{ID: "member"}, next.Revision, &retained)
	if err != nil || replaced.Unchanged || replaced.History[0].Content != "replacement" || len(store.queries) != 3 {
		t.Fatalf("replacement fence=%+v err=%v", replaced, err)
	}
	replaced.Metadata.Title = "caller mutation"
	replaced.History[0].Metadata["nested"].(map[string]any)["value"] = "caller mutation"
	if store.header.Metadata.Title != "Changed title" || store.history[0].Metadata["nested"].(map[string]any)["value"] != "owned" {
		t.Fatal("owned display results alias supplied store")
	}
}

func TestHostDisplayPageFailureFences(t *testing.T) {
	for _, mode := range []string{"id", "epoch", "first", "through", "gap", "empty", "replacement", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := newDisplayPageStore(hostTranscriptPageMessages + 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			old := &sessions.SessionViewHeader{ID: "member", Revision: "old", Epoch: "older", Sequence: 1}
			retained := old
			store.alterPage = func(page *sessions.TranscriptPage) {
				switch mode {
				case "id":
					page.ID = "wrong"
				case "epoch":
					page.Epoch = "wrong"
				case "first":
					page.First++
				case "through":
					page.Through++
				case "gap":
					page.Next++
				case "empty":
					page.Messages = nil
					page.Next = page.First
				case "replacement":
					store.header.Epoch = "new-epoch"
				case "canceled":
					cancel()
				}
			}
			got, err := readSessionDisplay(ctx, store, sessions.ViewTarget{ID: "member"}, "old", &retained)
			if err == nil || got != nil || retained != old || store.fullReads != 0 {
				t.Fatalf("partial result escaped: got=%+v err=%v retained=%+v", got, err, retained)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation=%v", err)
			}
			if mode == "replacement" && !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
				t.Fatalf("replacement=%v", err)
			}
		})
	}
}

func TestHostDisplayOversizedAndEmpty(t *testing.T) {
	for _, count := range []int{0, 3} {
		store := newDisplayPageStore(count)
		if count > 0 {
			store.history[1].Content = strings.Repeat("x", hostTranscriptPageBytes+1)
		}
		info, err := readSessionDisplay(context.Background(), store, sessions.ViewTarget{ID: "member"}, "", nil)
		if err != nil || !reflect.DeepEqual(info.History, store.history) {
			t.Fatalf("count=%d history lost: %v", count, err)
		}
		if count == 0 && len(store.queries) != 0 {
			t.Fatal("empty snapshot read open-ended transcript")
		}
	}
}

func TestHostDisplayRequiresBothSuppliedCapabilities(t *testing.T) {
	for _, mode := range []string{"legacy", "header only", "pages only"} {
		store := newDisplayPageStore(3)
		var supplied sessions.ViewStore
		switch mode {
		case "legacy":
			supplied = struct{ sessions.ViewStore }{store}
		case "header only":
			supplied = struct {
				sessions.ViewStore
				sessions.ViewHeaderStore
			}{store, store}
		case "pages only":
			supplied = struct {
				sessions.ViewStore
				sessions.TranscriptPageStore
			}{store, store}
		}
		info, err := readSessionDisplay(context.Background(), supplied, sessions.ViewTarget{ID: "member"}, "", nil)
		if err != nil || !reflect.DeepEqual(info.History, store.history) || store.fullReads != 1 || len(store.queries) != 0 {
			t.Fatalf("%s: full=%d pages=%d err=%v", mode, store.fullReads, len(store.queries), err)
		}
	}
}

func TestHostPagedInspectorExactPairing(t *testing.T) {
	for _, mode := range []string{"match", "wrong execution", "wrong generation", "wrong sequence", "invalid saved binding", "wrong live session"} {
		t.Run(mode, func(t *testing.T) {
			store := newDisplayPageStore(3)
			identity := inspectorLiveIdentity{execution: "exec", generation: 2}
			store.header.Metadata.Transcript = &sessions.TranscriptIdentity{Execution: "exec", Generation: 2, Sequence: 3}
			live := swarm.LiveTranscript{SessionID: "member", Execution: "exec", Generation: 2, BaseSequence: 3, Ready: true, Messages: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "provisional"}}}
			switch mode {
			case "wrong execution":
				live.Execution = "wrong"
			case "wrong generation":
				live.Generation++
			case "wrong sequence":
				live.BaseSequence++
			case "invalid saved binding":
				store.header.Metadata.Transcript.Sequence--
			case "wrong live session":
				live.SessionID = "wrong"
			}
			provider := inspectorLiveReadFunc(func(id string) (swarm.LiveTranscript, bool) {
				if id != "member" {
					t.Fatalf("unselected copy=%q", id)
				}
				store.order = append(store.order, "live")
				return live, true
			})
			_, tail, status, err := readInspectorConversation(context.Background(), store, sessions.ViewTarget{ID: "member"}, "", provider, identity)
			if err != nil || status.active != (mode == "match") || (len(tail.Messages) > 0) != (mode == "match") || strings.Join(store.order, ",") != "live,header,page" {
				t.Fatalf("status=%+v tail=%+v order=%v err=%v", status, tail, store.order, err)
			}
		})
	}
}
