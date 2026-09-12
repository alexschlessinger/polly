package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

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
	abs, err := r.ResolvePath(path)
	if err != nil {
		return "", nil, err
	}
	routes, resolved := localRoutes(abs)
	if err := checkReadPolicy(r, routes...); err != nil {
		return "", nil, err
	}
	f, info, err := openLocalRegular(resolved, os.O_RDONLY, 0)
	if err != nil {
		return "", nil, describeOpenError("attach", abs, err)
	}
	defer f.Close()
	if info.Size() > maxBytes {
		return "", nil, fmt.Errorf("%s exceeds %d bytes", abs, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", nil, err
	}
	if int64(len(data)) > maxBytes {
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
	if err := checkReadPolicy(r, routes...); err != nil {
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
		abs := filepath.Join(root, filepath.FromSlash(rel))
		routes, _ := localRoutes(abs)
		if checkReadPolicy(r, routes...) != nil {
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
			ignorePath := filepath.Join(abs, ".gitignore")
			_, data, err := r.ReadContextFile(ctx, ignorePath, 1<<20)
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
