package main

import (
	"os/exec"
	"runtime"
)

// openExternal hands a URL or a local file to the desktop's default
// handler, detached from the TUI. The reap goroutine keeps the exited
// launcher from lingering as a zombie. It is a desktop convenience, not a
// tool: nothing the model runs goes through here.
func openExternal(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// openURL opens a page in the user's browser.
func openURL(url string) error { return openExternal(url) }
