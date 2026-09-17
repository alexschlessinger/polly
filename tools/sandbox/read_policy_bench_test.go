package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchPolicyFixture builds a tree of files at depth four beneath a temp
// directory with a policy of grants and masks, half of which do not exist,
// and a modeled private root, so a query meets every kind of route.
func benchPolicyFixture(b *testing.B) (Config, []string) {
	b.Helper()
	dir, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	var cfg Config
	var paths []string
	for g := 0; g < 20; g++ {
		grant := filepath.Join(dir, fmt.Sprintf("grant%02d", g))
		cfg.ReadPaths = append(cfg.ReadPaths, grant)
		cfg.DenyPaths = append(cfg.DenyPaths, filepath.Join(grant, "secret"))
		if g%2 == 1 {
			continue // half the grants and masks do not exist
		}
		for d := 0; d < 10; d++ {
			leaf := filepath.Join(grant, "a", "b", fmt.Sprintf("c%d", d))
			mustMkdirAll(b, leaf)
			for f := 0; f < 10; f++ {
				path := filepath.Join(leaf, fmt.Sprintf("f%d", f))
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					b.Fatal(err)
				}
				paths = append(paths, path)
			}
		}
	}
	modelPrivateRoots(b, filepath.Join(dir, "home"))
	return cfg, paths
}

func BenchmarkReadAllowed(b *testing.B) {
	cfg, paths := benchPolicyFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ReadAllowed(cfg, paths[i%len(paths)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadPolicyAllowed(b *testing.B) {
	cfg, paths := benchPolicyFixture(b)
	policy, err := CompileReadPolicy(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := policy.Allowed(paths[i%len(paths)]); err != nil {
			b.Fatal(err)
		}
	}
}
