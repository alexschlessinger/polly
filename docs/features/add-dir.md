# Extra project directories

Use `--add-dir` to give a session read-only project context outside its workspace.
The directory list persists with the session and passes to child contexts.

```sh
polly --sandbox default --add-dir ../shared --add-dir ../data
```

## Contents

- [Add and resume](#add-and-resume)
- [Allowed paths](#allowed-paths)
- [Policy changes](#policy-changes)
- [Library APIs](#library-apis)

[Documentation index](../README.md) · [Sandbox guide](../SANDBOX.md#extra-project-directories)

## Add and resume

Repeat the flag for multiple directories. In the REPL, `/add-dir <path>` adds one;
`/add-dir` lists the saved entries. Flags merge with saved directories on resume,
and also work when creating a context. Resetting or clearing a session preserves
the list.

Paths expand `~`, resolve relative to Polly's working directory, and store the
canonical absolute target after symlink resolution. Exact duplicates and new
paths already covered by an existing ancestor are ignored. Adding an ancestor
later does not remove previously listed descendants.

## Allowed paths

An added path must exist and be a directory. These are refused:

- Filesystem root, home, or an ancestor of home.
- The workspace itself or a path inside it.
- OS temporary directories and their descendants.
- Directories containing a known credential mask, or inside one.

An ancestor of the workspace is allowed; that also exposes its siblings.
Use a deliberate `--readpath` or preset for credential access.

A saved directory that disappears stays in the session record. Its grant is
inactive for that sandbox's lifetime; recreate it and resume the session to make
it available again.

## Policy changes

`--add-dir` alone does **not** enable sandboxing. With sandboxing disabled, the
list supplies context but cannot restrict an unsandboxed process. Unsupported
platforms can run unsandboxed; a requested sandbox fails closed.

With a sandbox, adding a directory widens reads, never workspace writes. File
tools and rebuilt Bash/shell tools use the updated policy. Running stdio MCP
servers retain their previous policy; `/tools restart <server>` applies the new
base policy. A failed restart leaves the current server running.

## Library APIs

| API | Use |
|---|---|
| `sandbox.ValidateExtraReadDir(workspace, path)` | Validate a new directory |
| `sandbox.CanonicalizeExtraReadDir(path)` | Revalidate a saved entry without requiring existence |
| `sandbox.MergeExtraReadDirs(existing, added)` | Merge canonical paths without modifying the inputs |
| `registry.AppendBaseReadPaths(paths...)` | Extend sandbox reads and rebuild dependent tools |
| `sessions.Metadata.ExtraReadDirs` | Persist the session list (`extraReadDirs` in JSON) |

Sources: [path validation](../../tools/sandbox/extra_read_dir.go),
[registry policy](../../tools/registry.go), [session metadata](../../sessions/interface.go).
