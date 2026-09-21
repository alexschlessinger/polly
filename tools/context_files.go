package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	gitignore "github.com/denormal/go-gitignore"
)

// ReadContextFile reads a complete regular file under the registry's read
// policy. It fails rather than truncating when the file exceeds maxBytes.
func (r *ToolRegistry) ReadContextFile(ctx context.Context, path string, maxBytes int64) (string, []byte, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if maxBytes < 1 || maxBytes == 1<<63-1 {
		return "", nil, fmt.Errorf("file byte limit must be positive and below MaxInt64")
	}
	abs, f, info, err := openLocalRead(r, "attach", path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	data, tooLarge, err := readBoundedRegular(f, info, maxBytes)
	if err != nil {
		return "", nil, err
	}
	if tooLarge {
		return "", nil, fmt.Errorf("%s exceeds %d bytes", abs, maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	return abs, data, nil
}

// ContextFilePaths walks the workspace in-process, applying .gitignore files
// from outer to inner directories. Explicit reads are independent of ignores.
func (r *ToolRegistry) ContextFilePaths(ctx context.Context, root string) ([]string, error) {
	root, err := r.ResolvePath(root)
	if err != nil {
		return nil, err
	}
	routes, resolved := localRoutes(root)
	// One compiled policy serves the whole walk.
	policy, err := compileReadPolicy(r)
	if err != nil {
		return nil, err
	}
	if err := readRoutesAllowed(policy, routes...); err != nil {
		return nil, err
	}
	tree, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	// os.Root confines traversal even if directories change during discovery.
	scopes := map[string][]gitignore.GitIgnore{}
	var paths []string
	count, pathBytes := 0, 0
	err = fs.WalkDir(tree.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > 100000 {
			return fmt.Errorf("file completion exceeds 100000 entries")
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Directory links are never followed; explicit references can still name links.
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		// The walk never follows links, so an entry's resolved route is its
		// path under the resolved root; no per-entry symlink resolution.
		abs := filepath.Join(root, filepath.FromSlash(rel))
		canonical := filepath.Join(resolved, filepath.FromSlash(rel))
		if readRoutesAllowed(policy, pathRoutes(abs, canonical)...) != nil {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		inherited := scopes[path.Dir(rel)]
		if rel != "." {
			for i := len(inherited) - 1; i >= 0; i-- {
				local, err := filepath.Rel(inherited[i].Base(), abs)
				if err != nil {
					return err
				}
				if match := inherited[i].Relative(filepath.ToSlash(local), entry.IsDir()); match != nil {
					if match.Ignore() {
						if entry.IsDir() {
							return fs.SkipDir
						}
						return nil
					}
					break
				}
			}
		}
		if entry.IsDir() {
			rules := inherited
			data, err := readIgnoreRules(policy, filepath.Join(abs, ".gitignore"))
			if err == nil {
				rules = append(append([]gitignore.GitIgnore(nil), inherited...), gitignore.New(bytes.NewReader(data), abs, nil))
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("read ignore rules: %w", err)
			}
			scopes[rel] = rules
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		pathBytes += len(rel) + 1
		if pathBytes > 8<<20 {
			return fmt.Errorf("file completion exceeds 8 MiB")
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("file completion: %w", err)
	}
	return paths, nil
}

// readIgnoreRules reads a directory's .gitignore under the walk's compiled
// policy rather than recompiling the policy for every directory.
func readIgnoreRules(policy *sandbox.ReadPolicy, abs string) ([]byte, error) {
	const maxBytes = 1 << 20
	routes, resolved := localRoutes(abs)
	if err := readRoutesAllowed(policy, routes...); err != nil {
		return nil, err
	}
	f, info, err := openLocalRegular(resolved, os.O_RDONLY, 0)
	if err != nil {
		return nil, describeOpenError("read", abs, err)
	}
	defer f.Close()
	data, tooLarge, err := readBoundedRegular(f, info, maxBytes)
	if err != nil {
		return nil, err
	}
	if tooLarge {
		return nil, fmt.Errorf("%s exceeds %d bytes", abs, maxBytes)
	}
	return data, nil
}
