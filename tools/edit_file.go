package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
)

const (
	// editFileMaxBytes bounds how large a file edit_file will load; larger
	// files are better handled by wrapped commands.
	editFileMaxBytes = 16 << 20
	// editSnippetContext and editSnippetMaxLines bound the numbered
	// verification snippet returned after a successful edit.
	editSnippetContext  = 2
	editSnippetMaxLines = 20
)

// editFileTool performs an exact literal string replacement in a local text
// file. Reads honor the registry's base sandbox read policy and writes its
// write policy, so the tool cannot change what a sandboxed command could not.
type editFileTool struct {
	NativeTool
	registry *ToolRegistry
}

// NewEditFileTool creates the edit_file tool bound to registry's sandbox
// policy.
func NewEditFileTool(registry *ToolRegistry) Tool {
	return &editFileTool{registry: registry}
}

func (t *editFileTool) GetName() string { return "edit_file" }

// BindExecutionContext rebuilds the tool from the bound registry's own
// factory, under the context's root and policy.
func (t *editFileTool) BindExecutionContext(bound *ToolRegistry, _ ExecutionContext) (Tool, error) {
	return rebindNative(bound, t.GetName())
}

func (t *editFileTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"edit_file",
		"Replace an exact literal string in a local text file. old_string must match the file byte-for-byte (indentation and line endings included) and occur exactly once unless replace_all is set; never include the \"N: \" line-number prefix that read_file displays. Use write_file to create or fully replace a file.",
		schema.Params{
			"path":        schema.S("Filesystem path of the file to edit"),
			"old_string":  schema.S("Exact literal text to replace"),
			"new_string":  schema.S("Replacement text (may be empty to delete old_string)"),
			"replace_all": schema.Bool("Replace every occurrence instead of requiring old_string to be unique (default false)"),
		},
		"path", "old_string", "new_string",
	)
}

func (t *editFileTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, raw)
	return out.Text, err
}

// ExecuteOutput edits the file and reports the change as FileChanges in
// Data. Text is what the model sees and does not include the diff.
func (t *editFileTool) ExecuteOutput(ctx context.Context, raw map[string]any) (ToolOutput, error) {
	text, change, err := t.edit(ctx, raw)
	if err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{Text: text, Data: change}, nil
}

func (t *editFileTool) edit(ctx context.Context, raw map[string]any) (string, FileChanges, error) {
	var none FileChanges
	if err := ctx.Err(); err != nil {
		return "", none, err
	}
	args := Args(raw)
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		return "", none, fmt.Errorf("path is required")
	}
	oldString := args.String("old_string")
	if oldString == "" {
		return "", none, fmt.Errorf("old_string is required; use write_file to create a new file")
	}
	newString := args.String("new_string")
	if oldString == newString {
		return "", none, fmt.Errorf("old_string and new_string are identical")
	}
	abs, routes, resolved, err := resolveLocalRoutes(t.registry, path)
	if err != nil {
		return "", none, err
	}
	if err := checkReadPolicy(t.registry, routes...); err != nil {
		return "", none, err
	}
	if err := checkWritePolicy(t.registry, routes...); err != nil {
		return "", none, err
	}
	// The read and rewrite run under one lock and through one descriptor:
	// the text read is exactly the text replaced, and a concurrent edit_file
	// or write_file of the same file waits instead of racing this one.
	unlock := lockLocalFiles()
	defer unlock()
	f, info, err := openLocalRegular(resolved, os.O_RDWR, 0)
	if err != nil {
		return "", none, describeOpenError("edit", abs, err)
	}
	defer f.Close()
	data, tooLarge, err := readBoundedRegular(f, info, editFileMaxBytes)
	if err != nil {
		return "", none, fmt.Errorf("edit %s: %w", abs, err)
	}
	if tooLarge {
		return "", none, fmt.Errorf("%s is larger than %d bytes; edit_file handles files up to %d bytes", abs, editFileMaxBytes, editFileMaxBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", none, fmt.Errorf("%s looks like binary data; edit_file only edits text", abs)
	}
	content := string(data)
	replaceAll := args.Bool("replace_all")
	count := strings.Count(content, oldString)
	switch {
	case count == 0:
		return "", none, fmt.Errorf("old_string was not found in %s. Matching is exact, including whitespace and line endings; re-read the file with read_file and copy the text precisely, without the \"N: \" line-number prefix", abs)
	case count > 1 && !replaceAll:
		return "", none, fmt.Errorf("old_string occurs %d times in %s; provide a longer string that is unique, or set replace_all to replace every occurrence", count, abs)
	}
	replacements := 1
	if replaceAll {
		replacements = count
	}
	updated := strings.Replace(content, oldString, newString, replacements)
	if err := rewriteFile(f, updated); err != nil {
		return "", none, fmt.Errorf("edit %s: %w", abs, err)
	}
	unlock()
	if err := f.Sync(); err != nil {
		return "", none, fmt.Errorf("edit %s: %w", abs, err)
	}

	result := fmt.Sprintf("Edited %s: %d replacement(s).", abs, replacements)
	if snippet, err := editSnippet(ctx, updated, strings.Index(content, oldString), newString); err == nil && snippet != "" {
		result += "\n" + snippet
	}
	root := t.registry.changeRoot()
	change := FileChanges{Root: root, Tracked: true, Changes: []FileChange{
		DiffFileChange(changePath(root, abs), content, updated, true, true),
	}}
	return result, change, nil
}

// rewriteFile replaces the contents of the open file with content through
// the same descriptor the content was read from. The caller syncs the file
// once it has released localFileMu.
func rewriteFile(f *os.File, content string) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := f.WriteString(content)
	return err
}

// editSnippet renders numbered lines around the first replacement so the
// model can verify the edit without a follow-up read. firstIdx is the byte
// index of the first replacement, which the unchanged prefix keeps identical
// between the old and new content.
func editSnippet(ctx context.Context, updated string, firstIdx int, newString string) (string, error) {
	if firstIdx < 0 {
		return "", nil
	}
	startLine := 1 + strings.Count(updated[:firstIdx], "\n")
	offset := max(1, startLine-editSnippetContext)
	limit := min(startLine-offset+strings.Count(newString, "\n")+1+editSnippetContext, editSnippetMaxLines)
	return PageLines(ctx, strings.NewReader(updated), "file", offset, limit, "")
}
