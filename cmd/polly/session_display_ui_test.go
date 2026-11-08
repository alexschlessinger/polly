package main

import (
	"context"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

type suppliedDisplayStore struct {
	sessions.SessionStore
	*displayPageStore
}

func TestHostPagedInspectorMetadataRefresh(t *testing.T) {
	base := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, base, "root")
	pages := newDisplayPageStore(3)
	r.state.sessionStore = &suppliedDisplayStore{SessionStore: base, displayPageStore: pages}
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: "member"}})
	v := waitInspector(t, r, 140)
	before, oldSaved := len(pages.queries), v.saved
	if v.savedHeader == nil || v.savedHeader.Epoch != "epoch-1" || pages.fullReads != 0 {
		t.Fatalf("missing paged cache: header=%+v full=%d", v.savedHeader, pages.fullReads)
	}
	pages.header.Metadata.Title = "New title"
	pages.header.Revision = "title-revision"
	r.inspectorRefreshAt = time.Time{}
	r.refreshInspector(140)
	settleInspectorWork(r)
	if len(pages.queries) != before || v.info.Metadata.Title != "New title" || v.saved == oldSaved || v.saved.status.title != "New title" || oldSaved.status.title != "Original" {
		t.Fatalf("metadata did not reuse immutable history: queries=%d before=%d info=%+v", len(pages.queries), before, v.info)
	}
	if v.bytes < sessionHeaderBytes(v.savedHeader) {
		t.Fatal("header retention omitted from cache budget")
	}
	// Retirement retains the same bounded cache entry and header; returning
	// cannot turn a metadata-only refresh back into a full history read.
	r.closeInspector()
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: "member"}})
	waitInspector(t, r, 140)
	if len(pages.queries) != before {
		t.Fatal("cache reopen discarded saved header")
	}
}

func TestHostPagedInspectorLateAndCanceledReads(t *testing.T) {
	for _, mode := range []string{"late selection", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			base := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, base, "root")
			pages := newDisplayPageStore(3)
			entered, release := make(chan struct{}), make(chan struct{})
			pages.beforePage = func(ctx context.Context) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			r.state.sessionStore = &suppliedDisplayStore{SessionStore: base, displayPageStore: pages}
			r.inspect(viewTarget{session: sessions.ViewTarget{ID: "member"}})
			r.refreshInspector(140) // Returns while the off-loop page is held.
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("page was not started off loop")
			}
			stale := r.workspace().inspector.current
			if mode == "cancel" {
				r.work.cancel()
			} else {
				r.closeInspector()
			}
			close(release)
			settleInspectorWork(r)
			if stale.saved != nil || stale.savedHeader != nil || pages.fullReads != 0 {
				t.Fatalf("late result landed: saved=%p header=%+v full=%d", stale.saved, stale.savedHeader, pages.fullReads)
			}
			if mode == "late selection" && r.workspace().inspector.current != nil {
				t.Fatal("late read revived inspector")
			}
		})
	}
}
