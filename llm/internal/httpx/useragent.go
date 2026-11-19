package httpx

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// UserAgent identifies the harness build in provider HTTP logs.
var UserAgent = sync.OnceValue(func() string {
	return fmt.Sprintf("polly/%s (%s %s)", buildVersion(), runtime.GOOS, runtime.GOARCH)
})

// buildVersion is the module version when the binary was built from a
// tagged release, else the commit it was built from, else "dev".
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return setting.Value[:min(8, len(setting.Value))]
		}
	}
	return "dev"
}
