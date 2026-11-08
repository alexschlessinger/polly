package sessions

import (
	"encoding/json"
	"testing"
)

func TestCoordinationExtentEmptyAndStagedPartitionMoves(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				nativeMust(t, tx.Delete(RecordKey{"format", "swarm"}))
				checkExtent(t, tx, RecordQuery{}, 0, nil)
				checkExtent(t, tx, RecordQuery{Index: CoordinationIndexKind, Key: "custom"}, 0, nil)
				return tx.Put(RecordKey{"format", "swarm"}, json.RawMessage(`{"version":2}`))
			})
			nativeMust(t, err)
			key := RecordKey{"mail", "m"}
			_, err = access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				a := RecordQuery{Index: CoordinationIndexMailRecipient, Key: "a"}
				b := RecordQuery{Index: CoordinationIndexMailRecipient, Key: "b"}
				pending := RecordQuery{Index: CoordinationIndexMailPending, Key: "b"}
				nativeMust(t, tx.Put(key, json.RawMessage(`{"to":"a"}`)))
				checkExtent(t, tx, a, 1, &key)
				checkExtent(t, tx, b, 0, nil)
				nativeMust(t, tx.Put(key, json.RawMessage(`{"to":"b"}`)))
				checkExtent(t, tx, a, 0, nil)
				checkExtent(t, tx, b, 1, &key)
				checkExtent(t, tx, pending, 1, &key)
				nativeMust(t, tx.Put(key, json.RawMessage(`{"to":"b","delivered":true}`)))
				checkExtent(t, tx, b, 1, &key)
				checkExtent(t, tx, pending, 0, nil)
				return nil
			})
			nativeMust(t, err)
		})
	}
}
