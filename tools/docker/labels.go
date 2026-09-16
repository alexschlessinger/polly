package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Container labels are the only record of a container: nothing about it is
// persisted in session records. Every polly.* label is part of the identity
// an open compares; a mismatch destroys and recreates the container.
const (
	labelPrefix   = "polly."
	labelSession  = "polly.session"
	labelRoot     = "polly.root"
	labelMode     = "polly.mode"
	labelImage    = "polly.image"
	labelProtocol = "polly.protocol"
	labelReadOnly = "polly.readonly"
	labelScratch  = "polly.scratch"
	labelNetwork  = "polly.network"
	labelMemory   = "polly.memory"
	labelCPUs     = "polly.cpus"
	labelPIDs     = "polly.pids"
	labelUser     = "polly.user"
	labelMounts   = "polly.mounts"
)

// containerName derives a stable name from the session and root.
func containerName(session, root string) string {
	sum := sha256.Sum256([]byte(session + "|" + root))
	return "polly-" + hex.EncodeToString(sum[:])[:12]
}

// mountsDigest summarises a mount set so a changed set recreates the
// container.
func mountsDigest(mounts []mount) string {
	lines := make([]string, 0, len(mounts))
	for _, m := range mounts {
		access := "rw"
		if m.ReadOnly {
			access = "ro"
		}
		lines = append(lines, strings.Join([]string{m.Type, m.Source, m.Target, access}, "\x00"))
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// computeLabels builds the container's labels from its spec, over any
// caller labels.
func computeLabels(extra map[string]string, spec containerSpec) map[string]string {
	labels := maps.Clone(extra)
	if labels == nil {
		labels = map[string]string{}
	}
	network := "none"
	if spec.network.Allow {
		network = "bridge"
		if spec.network.DenyDNS {
			network = "bridge-nodns"
		}
	}
	labels[labelSession] = spec.session
	labels[labelRoot] = spec.root
	labels[labelMode] = string(spec.mode)
	labels[labelImage] = spec.imageID
	labels[labelProtocol] = fmt.Sprint(spec.protocol)
	labels[labelReadOnly] = fmt.Sprint(spec.readOnly)
	labels[labelScratch] = spec.scratch
	labels[labelNetwork] = network
	labels[labelMemory] = fmt.Sprint(spec.memory)
	labels[labelCPUs] = fmt.Sprint(spec.nanoCPUs)
	labels[labelPIDs] = fmt.Sprint(spec.pids)
	labels[labelUser] = spec.user
	labels[labelMounts] = mountsDigest(spec.mounts)
	return labels
}

// labelsMatch reports whether every polly.* label of want is present in
// have with the same value.
func labelsMatch(have, want map[string]string) bool {
	for key, value := range want {
		if !strings.HasPrefix(key, labelPrefix) {
			continue
		}
		if have[key] != value {
			return false
		}
	}
	return true
}
