# Captured evidence

`capture.py` opens the source with SQLite `mode=ro` and `query_only`, takes an
online backup into a new private directory, then evaluates `PRAGMA quick_check`
on the copy. An explicit `--out` directory must not already exist. It refuses an
unknown required schema rather than guessing or editing the source.

The capture contains:

- `sessions.db`: the complete consistent store, including artifact bytes. Use a
  read-only connection for subsequent queries.
- `manifest.json`: capture timestamps, selected IDs, inventory of recent roots,
  family membership, message counts, record counts, leases, and warnings.
- `sessions/ID/session.json`: settings and identity for each selected root/worker.
- `sessions/ID/messages.jsonl`: `{sequence, message}` records, preserving raw
  message fields and metadata without truncation.
- `coordination.jsonl`: `{parent_id, kind, id, payload}` records for selected
  families. Record kinds can evolve; the exporter preserves rather than filters
  them.

The helper follows both session ancestry and `swarm_members` relationships. A
named worker may be selected explicitly, but its capture does not imply that the
parent's coordination is included; select the root for a full audit. Latest-root
selection sorts by `updated_ns`, then `created_ns`, then ID. Report this basis if
an old session was recently resumed.

## Reading the snapshot

Inspect table columns before writing schema-dependent queries. The standard
store currently has:

| Evidence | Source |
|---|---|
| Names, ancestry, timestamps, settings | `sessions` |
| Ordered messages | `messages(session_id, sequence, payload_json)` |
| Tasks, members, executions, mail, publications, follow-ups, integrations | `swarm_records(parent_id, kind, id, payload_json)` |
| Family membership | `swarm_members` and `sessions.parent_id` |
| Session/family artifact ownership | `session_artifacts`, `swarm_artifacts` |
| Artifact metadata/bytes | `artifact_blobs`, ordered `artifact_chunks` |
| Lease observations | `session_leases` |

Messages use fields such as `role`, `content`, `text_blocks`, `tool_calls`,
`tool_call_id`, `tool_name`, `parts`, and `metadata`. Treat these as discovery
anchors rather than a frozen schema. Assistant text can live in phased
`text_blocks`; counting only `content` loses it. A saved `turn_audit` is internal
metadata, not user-visible text. Opaque provider state is usually irrelevant to
the report but remains in the private capture when needed for diagnosis.

To read a few exact messages without dumping a transcript:

```python
import json
from pathlib import Path

for line in Path("CAPTURE/sessions/ID/messages.jsonl").open():
    row = json.loads(line)
    if row["sequence"] in {170, 172, 173}:
        print(json.dumps(row, ensure_ascii=False))
```

For a large tool result, the saved preview may contain a durable artifact ID.
Locate its digest and ownership, join chunks in index order, and verify actual
bytes against recorded size and SHA-256 before claiming completeness. Prefer
reading only the referenced result, not extracting every stored blob.

## Interpretation traps

- `next_sequence` and the number of retained messages need not match after
  clearing or other supported history operations. Inspect the actual sequence.
- A queued message is not proof the recipient saw it. Delivery/admission proves
  persisted input, not comprehension, response, or use of every artifact byte.
- Some parent completion notices contain worker findings automatically; they
  should not be counted as deliberate peer collaboration.
- A publication can be coalesced or superseded while attribution and receipts
  remain. Count canonical evidence and confirmations separately when it matters.
- Worker scratch is disposable; persistence of the worker identity/transcript
  does not imply persistence of its former files.
- A tool timeout can cut off a still-progressing broad test command. Find its
  completed package output and subsequent runs before calling validation absent.
- Timing or failure counts alone do not establish a bottleneck, correctness bug,
  or need for a new feature. Trace the effect on work and outcome.
