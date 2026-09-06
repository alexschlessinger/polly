# SEARCH.md

How Polly searches. No hedging.

## One tool

`zvec_grep_search`: zg's agent search surface, loaded when `zg` (zvec-grep) is on `PATH`. Exact lookups are bash's job: `grep`, `rg`. Without zg, bash is the whole story. That is what zg's own default agent toolset does too.

| tool | input | backend | default limit | max |
|------|-------|---------|---------------|-----|
| `zvec_grep_search` | zg's agent request: `query`, `queries`, `fts`, `vector`, `fuse`, `globs`, … | `zg` + local embedding model | 7 per group | 50 |

Indexed results are **ranked samples, not exhaustive matches**. If the snippets don't answer the question, follow up with `read_file`, or `grep`/`rg` for every occurrence. Do not treat "no hits" as "not in the codebase."

## The request

Same fields, names, and meanings as zg's own MCP `zvec_grep_search`, so zg's agent guidance applies verbatim. Every list field also accepts a single string.

| field | meaning |
|-------|---------|
| `query` | one primary hybrid (FTS + vector) group |
| `queries` | more primary hybrid groups; each searched separately with its own ranks unless `fuse` |
| `fts` | supplemental lexical-only groups: symbols, flags, error strings |
| `vector` | supplemental semantic-only groups |
| `fuse` | collapse every group into one ranked list |
| `limit` | per group, or for the fused plan; 7 default, 50 max |
| `globs`, `insensitiveGlobs` | ordered rg-style rules, `!` excludes, later rules win; relative to `path` |
| `fileTypes`, `excludedFileTypes` | `rg --type-list` names |
| `preferSymbol`, `symbolTypes` | symbol-aware ranking; types: module, class, interface, function, value, alias |
| `modifiedAfter`, `modifiedBefore` | a date, or epoch milliseconds |
| `path` | Polly's stand-in for zg's `root`: a directory to narrow to. The workspace is decided by [Index](#index), not by the model |

Not offered, on purpose:

- `root`: the sandbox decides the workspace.
- `apiKey`, `device`, `endpoint`: the local CPU model is pinned; never remote embeddings.
- `hidden`, `noIgnore`, `ignoreFiles`, `maxDepth`, `maxFileSizeBytes`, `follow`, `embeddingConcurrency`: index file-selection. An existing index keeps its settings.
- `freshness`, `autoUpdate`: every query waits for a fresh index.
- `trace`: debug output.

Output is zg's agent text, filtered: group lines (`Q1 [primary]: …`, `hits: N`), then one block per hit with a `#n matchedBy=… path:range` header, metadata, and numbered source lines. Hits outside `path`, outside the read policy, deleted, or reached through symlinks are removed. Output over the page cap drops the last, partial hit and says so.

## Dependency

Requires `zg` on `PATH`. Resolved at tool load, every time. Not cached.

- `zg` present → tool loads.
- `zg` absent → tool does not load. Not degraded. Not replaced. Gone.
- `zg` removed mid-session → next call errors. No silent downgrade.

## Install

`zg` is [Zvec-Grep](https://zvec.org/en/docs/zvec-grep/). Node.js ≥ 22 required.

```bash
npm install -g @zvec/zvec-grep
zg version
```

That is the whole dependency. The embedding model is separate: Polly uses `potion-code-16m-v2`, downloaded on the first query, cached under `.zvec-grep/polly/`. Verify the live path with the test in [Verification](#verification).

## When zg is absent

| situation | behavior |
|-----------|----------|
| fresh default context | `zvec_grep_search` omitted from seed metadata |
| default load at turn time | skipped; other tools load; no warning |
| session saved with zg, restored without | saved preference kept; tool not registered; session works |
| session saved by an older Polly naming `search_files` | preference kept; tool not registered; session works |
| `--tool zvec_grep_search` explicitly | **fails**: `zvec_grep_search requires zvec-grep (zg) on PATH` |
| zg returns | preference loads again on next restore |

No fallback tool. Absence is honest; a lying fallback makes the model trust semantic results that are literal greps. Omit, don't imitate.

Model fallback is the model's job: `bash` grep/`rg`, `read_file`, `list_dir`. Tool descriptions steer toward `zvec_grep_search` only when it is loaded.

## Index

- Location: `<workspace>/.zvec-grep/`. Index, runtime state, model cache all live there.
- Created on first query. Refreshed inside every later query (`--refresh wait`): one zg launch scans, embeds what changed, and answers. Nothing changed costs a scan; anything changed also loads the model, about 2.5s in a fresh process however many files changed. Model: `potion-code-16m-v2`, downloaded on first query.
- Search root: nearest ancestor with `.zvec-grep/manifest.json` or a Git checkout, else the requested directory. Discovery stops short of the home directory and the filesystem root; zg's own `~/.zvec-grep` runtime home is not an index. Subdirectory searches stay scoped.

## Policy

- `zg` runs through the process sandbox, `--mode direct`. No daemon delegation. No extra write or network grants. Automatic indexing is local-model only — never remote embeddings.
- Cached hits are re-filtered through the current read policy, even if another app built the index.
- Read-only policy: bash grep/`rg` work; index creation/refresh does not.
- Ancestor index or Git root whose `.zvec-grep` the write policy cannot reach → skipped; the index lives at the requested (writable) directory. The default CLI sandbox writes only under cwd, so a launch from a subdirectory indexes that subdirectory. `--writepath <repo-root>` shares one index across launches.
- Another daemon owns the write lease → index untouched; you get a snapshot marked `possibly_stale`. Verify with `read_file`.

## Verification

Live test (builds an index, downloads the model — slow, network):

```bash
POLLYTOOL_REQUIRE_ZG_TESTS=1 go test ./tools -run TestZvecGrepSearchLive -count=1
```

Unit tests run without `zg`; they assert omission, not degradation.
