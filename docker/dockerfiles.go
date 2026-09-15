// Package dockerfiles ships the reference container images the docker tool
// backend runs tools in: a base with git, bash, coreutils and polly, and
// variants adding a toolchain. polly sandbox build writes one out and builds
// it; nothing builds an image implicitly.
package dockerfiles

import (
	"embed"
	"fmt"
	"slices"
)

//go:embed base/Dockerfile go/Dockerfile node/Dockerfile python/Dockerfile
var files embed.FS

var variants = []string{"base", "go", "node", "python"}

// Variants lists the reference image variants, base first.
func Variants() []string { return slices.Clone(variants) }

// Dockerfile returns a variant's Dockerfile.
func Dockerfile(variant string) ([]byte, error) {
	if !slices.Contains(variants, variant) {
		return nil, fmt.Errorf("unknown image variant %q (choose one of %v)", variant, variants)
	}
	return files.ReadFile(variant + "/Dockerfile")
}

// Parent names the variant a variant builds on, or "" for base.
func Parent(variant string) string {
	if variant == "base" {
		return ""
	}
	return "base"
}

// Tag is the image reference polly sandbox build gives a variant.
func Tag(variant string) string { return "polly/" + variant + ":latest" }
