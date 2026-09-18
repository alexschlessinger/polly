package sandbox

import "github.com/alexschlessinger/pollytool/internal/scratch"

// traversablePrivateRoots lists private roots whose own directory entry stays
// readable while everything beneath it needs a grant. Only the runtime scratch
// root qualifies. A command reaches its own scratch by opening each component
// of the path in turn — the hardening internal/safefile applies, and what any
// careful tool may do — and a wholly denied ancestor refuses that open however
// the leaf is granted. Listing the root reveals polly's own slot names and
// nothing belonging to the user, while every slot inside it stays unreadable
// without a grant, as under the private home.
func traversablePrivateRoots() []string { return []string{scratch.Root()} }
