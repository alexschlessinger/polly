package docker

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
)

// Container-side fixed paths.
const (
	containerHome = "/run/polly/home"
	tmpfsTmp      = "/tmp"
	tmpfsRun      = "/run/polly"
)

// containerSpec is everything that determines a container's identity and
// its create request.
type containerSpec struct {
	session  string
	root     string
	scratch  string
	readOnly bool
	mode     Mode
	imageID  string
	protocol int
	network  NetworkPolicy
	memory   int64
	nanoCPUs int64
	pids     int
	user     string
	mounts   []mount
	labels   map[string]string
	name     string
}

type engineMount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type hostConfig struct {
	Mounts         []engineMount     `json:"Mounts"`
	Tmpfs          map[string]string `json:"Tmpfs"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	CapDrop        []string          `json:"CapDrop"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	NetworkMode    string            `json:"NetworkMode"`
	PidsLimit      int64             `json:"PidsLimit,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`
	NanoCPUs       int64             `json:"NanoCpus,omitempty"`
	Init           bool              `json:"Init"`
}

// createBody is the container create request. The environment carries only
// HOME; host-selected values travel sealed over the exec stream.
type createBody struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env"`
	WorkingDir string            `json:"WorkingDir"`
	User       string            `json:"User"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
}

// createRequest builds the create request: a sleeping init under
// docker-init, a read-only root with private tmpfs mounts, dropped
// capabilities, the host user, and the derived mounts.
func createRequest(spec containerSpec) createBody {
	mounts := make([]engineMount, 0, len(spec.mounts))
	for _, m := range spec.mounts {
		mounts = append(mounts, engineMount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	networkMode := "none"
	if spec.network.Allow {
		networkMode = "bridge"
	}
	return createBody{
		Image:      spec.imageID,
		Cmd:        []string{"sleep", "infinity"},
		Env:        []string{"HOME=" + containerHome},
		WorkingDir: spec.root,
		User:       spec.user,
		Labels:     spec.labels,
		HostConfig: hostConfig{
			Mounts:         mounts,
			Tmpfs:          map[string]string{tmpfsTmp: "rw,nosuid,nodev,exec,size=1g", tmpfsRun: "rw,nosuid,nodev,exec,size=256m"},
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			NetworkMode:    networkMode,
			PidsLimit:      int64(spec.pids),
			Memory:         spec.memory,
			NanoCPUs:       spec.nanoCPUs,
			Init:           true,
		},
	}
}

// helperExec is the exec that starts the helper.
func helperExec(spec containerSpec, helperPath string) execBody {
	if helperPath == "" {
		helperPath = "polly"
	}
	return execBody{
		AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: false,
		Cmd:        []string{helperPath, "sandbox", "helper"},
		Env:        []string{"HOME=" + containerHome},
		WorkingDir: spec.root,
		User:       spec.user,
	}
}

// specMismatch explains why an existing container is not this spec.
func specMismatch(have map[string]string, want map[string]string) string {
	for _, key := range []string{labelImage, labelProtocol, labelMode, labelRoot, labelReadOnly, labelScratch, labelNetwork, labelMemory, labelCPUs, labelPIDs, labelUser, labelMounts} {
		if have[key] != want[key] {
			return fmt.Sprintf("%s changed", key)
		}
	}
	return "labels changed"
}

var _ = protocol.Version
