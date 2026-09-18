package workflow

import (
	"fmt"
	"path"
	"strings"
)

// CheckSource rejects source that is a filesystem path rather than the
// JavaScript text of a workflow. Nothing else catches the mistake: a path is a
// valid expression, so the engine reports one as an undefined variable
// ("skills/a/b.js" becomes `skills is not defined`) and an absolute path as an
// invalid regular-expression flag naming a directory of the caller's home.
// Neither names what went wrong, and a caller that reads either literally
// looks for a bug in its script.
func CheckSource(source string) error {
	if !sourceIsPath(source) {
		return nil
	}
	return &Error{Code: "invalid_source", Message: fmt.Sprintf("workflow source must be the JavaScript text of the script, but %q is a file path; read the file and pass its contents as source", strings.TrimSpace(source))}
}

// sourceIsPath reports whether source names a file instead of defining a
// workflow. Every workflow calls polly.workflow or polly.defineWorkflow, so
// source holding no call at all, on one line, is not one; a script file's
// extension or a leading path root then settles what it is instead.
func sourceIsPath(source string) bool {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" || strings.ContainsAny(trimmed, "(){}[];=\n\r\"'`") {
		return false
	}
	switch strings.ToLower(path.Ext(trimmed)) {
	case ".js", ".mjs", ".cjs", ".ts":
		return true
	}
	for _, root := range []string{"/", "./", "../", "~/"} {
		if strings.HasPrefix(trimmed, root) {
			return true
		}
	}
	return false
}
