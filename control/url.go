package control

import (
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

// Transport schemes of a backend URL.
const (
	// SchemeUnix dials a daemon's socket that is there already: local, or
	// forwarded from another machine.
	SchemeUnix = "unix"
	// SchemeExec finds or starts the daemon of a local backend binary.
	SchemeExec = "exec"
)

// Target is a parsed backend URL.
type Target struct {
	Scheme string
	// Socket is the socket a unix:// target dials.
	Socket string
	// Binary is the absolute path of an exec:// target's program.
	Binary string
	// Runtime, Dir and Home override, for a unix:// daemon on another
	// machine, where its store is, where its setup works and its home
	// (query parameters runtime=, dir=, home=).
	Runtime, Dir, Home string
	raw                string
}

// String is the URL as it was given.
func (t Target) String() string { return t.raw }

// ParseTarget reads a backend URL:
//
//	unix:///path/to/serve.sock[?runtime=/abs&dir=/abs&home=/abs]
//	exec:///abs/path/to/binary
//	exec://name            (name looked up on PATH)
func ParseTarget(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, fmt.Errorf("backend %q is no URL: %w", raw, err)
	}
	t := Target{Scheme: strings.ToLower(u.Scheme), raw: raw}
	q := u.Query()
	for _, name := range []string{"runtime", "dir", "home"} {
		if v := q.Get(name); v != "" && !filepath.IsAbs(v) {
			return Target{}, fmt.Errorf("backend %q: %s= must be an absolute path", raw, name)
		}
	}
	switch t.Scheme {
	case SchemeUnix:
		if u.Host != "" {
			return Target{}, fmt.Errorf("backend %q: a unix:// URL names a socket path, as unix:///path/to/serve.sock", raw)
		}
		if u.Path == "" || !filepath.IsAbs(u.Path) {
			return Target{}, fmt.Errorf("backend %q: a unix:// URL names an absolute socket path", raw)
		}
		t.Socket = filepath.Clean(u.Path)
		t.Runtime, t.Dir, t.Home = q.Get("runtime"), q.Get("dir"), q.Get("home")
	case SchemeExec:
		if len(q) > 0 {
			return Target{}, fmt.Errorf("backend %q: an exec:// URL takes no parameters", raw)
		}
		switch {
		case u.Host != "" && (u.Path == "" || u.Path == "/"):
			found, err := exec.LookPath(u.Host)
			if err != nil {
				return Target{}, fmt.Errorf("backend %q: %s is not on PATH: %w", raw, u.Host, err)
			}
			if t.Binary, err = filepath.Abs(found); err != nil {
				return Target{}, err
			}
		case u.Host == "" && filepath.IsAbs(u.Path):
			t.Binary = filepath.Clean(u.Path)
		default:
			return Target{}, fmt.Errorf("backend %q: an exec:// URL is exec:///abs/path/to/binary or exec://name", raw)
		}
	case "":
		return Target{}, fmt.Errorf("backend %q has no scheme: use unix:///path/to/serve.sock or exec:///path/to/binary", raw)
	default:
		return Target{}, fmt.Errorf("backend scheme %q is not supported: use unix:// or exec://", u.Scheme)
	}
	return t, nil
}
