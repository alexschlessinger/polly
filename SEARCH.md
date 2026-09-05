# SEARCH.md

How `search_files` works. No hedging.

## Modes

Two modes. Exactly one per call. Both rejected if you pass both.

| mode | input | backend | default limit | max |
|------|-------|---------|---------------|-----|
| indexed | `query` | `zg` (zvec-grep) + local embedding model | 7 | 50 |
| exact | `pattern` (+ optional `regex`) | in-process Go walk | 100 | 500 |

Indexed results are **ranked samples, not exhaustive matches**. If the snippets don't answer the question, follow up with `read_file`. Do not treat "no hits" as "not in the codebase."

Exact mode reports `path:line: text`. Never paste that prefix into `edit_file`'s `old_string`.

## Dependency

Indexed mode requires `zg` on `PATH`. Resolved at tool load, every time. Not cached.

- `zg` present → tool loads, `query` works.
- `zg` absent → tool does not load. Not degraded. Not replaced. Gone.
- `zg` removed mid-session → next call errors. No silent downgrade.

## Install

`zg` is [Zvec-Grep](https://zvec.org/en/docs/zvec-grep/). Node.js ≥ 22 required.

```bash
npm install -g @zvec/zvec-grep
zg version
```

That is the whole dependency. The embedding model is separate: Polly uses `potion-code-16m-v2`, downloaded on the first `query`, cached under `.zvec-grep/polly/`. Verify the live path with the test in [Verification](#verification).

## When zg is absent

| situation | behavior |
|-----------|----------|
| fresh default context | `search_files` omitted from seed metadata |
| default load at turn time | skipped; other tools load; no warning |
| session saved with zg, restored without | saved preference kept; tool not registered; session works |
| `--tool search_files` explicitly | **fails**: `search_files requires zvec-grep (zg) on PATH` |
| zg returns | preference loads again on next restore |

No fallback to exact mode. Absence is honest; a lying fallback makes the model trust semantic results that are literal greps. Omit, don't imitate.

Model fallback is the model's job: `bash` grep/`rg`, `read_file`, `list_dir`. Tool descriptions already steer this way when `search_files` is loaded; they don't mention it when it isn't.

## Index

- Location: `<workspace>/.zvec-grep/`. Index, runtime state, model cache all live there.
- Created on first `query`. Refreshed incrementally after. Model: `potion-code-16m-v2`, downloaded on first query.
- Search root: nearest ancestor with `.zvec-grep/manifest.json` or a Git checkout, else the requested directory. Discovery stops short of the home directory and the filesystem root; zg's own `~/.zvec-grep` runtime home is not an index. Subdirectory searches stay scoped.
- Exact mode skips `.git` and `.zvec-grep` too.

## Policy

- `zg` runs through the process sandbox, `--mode direct`. No daemon delegation. No extra write or network grants. Automatic indexing is local-model only — never remote embeddings.
- Cached hits are re-filtered through the current read policy, even if another app built the index.
- Read-only policy: exact mode works; index creation/refresh does not.
- Another daemon owns the write lease → index untouched; you get a snapshot marked `possibly_stale`. Verify with `read_file`.

## Verification

Live test (builds an index, downloads the model — slow, network):

```bash
POLLYTOOL_REQUIRE_ZG_TESTS=1 go test ./tools -run TestIndexedSearchLive -count=1
```

Unit tests run without `zg`; they assert omission, not degradation.
