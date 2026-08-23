//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func rejectPlatformBroadWorkspace(dir string) error {
	tempRoots, runRoots := privateLinuxRoots()
	privateRoots := append(append([]string(nil), tempRoots...), runRoots...)
	if pathEqualsAny(dir, privateRoots) {
		return broadWorkspaceError(dir, "private Linux sandbox root")
	}

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("inspect Linux mount table for workspace %q: %w", dir, err)
	}
	mounts, err := linuxMountPoints(data)
	if err != nil {
		return fmt.Errorf("inspect Linux mount table for workspace %q: %w", dir, err)
	}
	if mounts[filepath.Clean(dir)] {
		return broadWorkspaceError(dir, "mounted volume root")
	}
	return nil
}

func linuxMountPoints(data []byte) (map[string]bool, error) {
	mounts := make(map[string]bool)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, fmt.Errorf("mount table is empty")
	}
	for lineNumber, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			return nil, fmt.Errorf("mount table line %d has %d fields, want at least 10", lineNumber+1, len(fields))
		}
		separator := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || len(fields)-separator-1 < 3 {
			return nil, fmt.Errorf("mount table line %d is missing its filesystem separator", lineNumber+1)
		}
		mountPoint, err := decodeLinuxMountInfoPath(fields[4])
		if err != nil {
			return nil, fmt.Errorf("mount table line %d mount point: %w", lineNumber+1, err)
		}
		if !filepath.IsAbs(mountPoint) {
			return nil, fmt.Errorf("mount table line %d mount point %q is not absolute", lineNumber+1, mountPoint)
		}
		mounts[filepath.Clean(mountPoint)] = true
	}
	return mounts, nil
}

func decodeLinuxMountInfoPath(value string) (string, error) {
	var decoded strings.Builder
	decoded.Grow(len(value))
	for i := 0; i < len(value); {
		if value[i] != '\\' {
			decoded.WriteByte(value[i])
			i++
			continue
		}
		if i+4 > len(value) {
			return "", fmt.Errorf("truncated escape in %q", value)
		}
		switch value[i : i+4] {
		case `\040`:
			decoded.WriteByte(' ')
		case `\011`:
			decoded.WriteByte('\t')
		case `\012`:
			decoded.WriteByte('\n')
		case `\134`:
			decoded.WriteByte('\\')
		default:
			return "", fmt.Errorf("unsupported escape %q in %q", value[i:i+4], value)
		}
		i += 4
	}
	return decoded.String(), nil
}

// gitHostWritablePath mirrors bubblewrap's private-root precedence. Exact
// /tmp, runtime temp, and /run grants stay private tmpfs mounts; a configured
// descendant or wider ancestor is bound later and therefore re-exposes host
// content that the Git policy must audit.
func gitHostWritablePath(path string) bool {
	tempRoots, runRoots := privateLinuxRoots()
	privateRoots := append(append([]string(nil), tempRoots...), runRoots...)
	return !pathEqualsAny(path, privateRoots)
}
