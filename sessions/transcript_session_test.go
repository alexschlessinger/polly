package sessions

import (
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestSessionTranscriptReaderStableTargetAndReadOnly(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := nativeHandle(t, store, "original")
	nativeMust(t, session.AddMessages(t.Context(), messages.User("first")))
	var reader SessionTranscriptReader = session
	first, err := reader.ReadTranscriptPage(t.Context(), TranscriptQuery{Limit: 1})
	nativeMust(t, err)
	before := lastUsed(t, session)
	metric := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), metric)
	empty, err := reader.ReadTranscriptPage(ctx, TranscriptQuery{Epoch: first.Epoch, After: first.Next, Limit: 1})
	nativeMust(t, err)
	if empty.Next != 1 || empty.Through != 1 || len(empty.Messages) != 0 || !lastUsed(t, session).Equal(before) {
		t.Fatalf("empty tail changed state: %+v", empty)
	}
	count := metric.Snapshot()
	if count.Count(runtimemetrics.TranscriptMessagesRead) != 0 || count.Count(runtimemetrics.PayloadBytesRead) != 0 || count.Count(runtimemetrics.WriteTransactions) != 0 {
		t.Fatalf("unchanged session paging read payload/wrote: %+v", count)
	}
	nativeMust(t, session.Rename(t.Context(), "renamed"))
	other := nativeHandle(t, store, "original")
	nativeMust(t, other.AddMessages(t.Context(), messages.User("different identity")))
	again, err := reader.ReadTranscriptPage(t.Context(), TranscriptQuery{Epoch: first.Epoch, Limit: 1})
	nativeMust(t, err)
	if again.ID != first.ID || again.Messages[0].Content != "first" {
		t.Fatalf("session paging followed a reused name: %+v", again)
	}
	nativeMust(t, session.Clear(t.Context()))
	nativeMust(t, session.AddMessages(t.Context(), messages.User("replacement")))
	_, err = reader.ReadTranscriptPage(t.Context(), TranscriptQuery{Epoch: first.Epoch, Limit: 1})
	if !errors.Is(err, ErrStaleTranscriptEpoch) {
		t.Fatalf("same-length replacement kept old epoch: %v", err)
	}
}
