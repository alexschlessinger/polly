package codex

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// loginResult is what ends a browser sign-in: the authorization code the
// authority redirected back with, or its refusal.
type loginResult struct {
	code string
	err  error
}

// callbackServer receives the authority's redirect on a loopback port. It
// listens on 127.0.0.1 only: the browser on this machine is the only
// client, and nothing off the machine can reach it.
type callbackServer struct {
	srv     *http.Server
	port    int
	state   string
	results chan<- loginResult
}

// listenCallback binds the first of ports that is free, delivering the
// code it receives on results. Port 0 asks for an ephemeral port.
func listenCallback(ports []int, state string, results chan<- loginResult) (*callbackServer, error) {
	var lastErr error
	for _, port := range ports {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			lastErr = err
			continue
		}
		s := &callbackServer{port: ln.Addr().(*net.TCPAddr).Port, state: state, results: results}
		mux := http.NewServeMux()
		mux.HandleFunc(callbackPath, s.handle)
		s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = s.srv.Serve(ln) }()
		return s, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no ports to try")
	}
	return nil, fmt.Errorf("codex: no sign-in callback port is free: %w", lastErr)
}

// redirectURI is where the authority sends the browser. The allow-list
// spells the host "localhost".
func (s *callbackServer) redirectURI() string {
	return fmt.Sprintf("http://localhost:%d%s", s.port, callbackPath)
}

func (s *callbackServer) handle(w http.ResponseWriter, r *http.Request) {
	code, err := redirectCode(r.URL.Query(), s.state)
	if err != nil {
		http.Error(w, "polly sign-in: "+err.Error(), http.StatusBadRequest)
		if !errors.Is(err, errStateMismatch) {
			s.deliver(loginResult{err: err})
		}
		return
	}
	s.deliver(loginResult{code: code})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, callbackPage)
}

// deliver hands the first result over; later ones are dropped.
func (s *callbackServer) deliver(r loginResult) {
	select {
	case s.results <- r:
	default:
	}
}

func (s *callbackServer) Close() error { return s.srv.Close() }

const callbackPage = `<!doctype html><html><head><meta charset="utf-8"><title>polly</title></head>
<body style="font-family: system-ui, sans-serif; margin: 3em; color: #222"><h1>Signed in to polly</h1>
<p>You can close this tab and return to the terminal.</p></body></html>
`

var errStateMismatch = errors.New("the sign-in did not start here (state mismatch)")

// redirectCode reads the authorization code out of a redirect's query,
// refusing a state that is not this sign-in's and reporting the
// authority's refusal when it sent one instead of a code.
func redirectCode(q url.Values, state string) (string, error) {
	if e := q.Get("error"); e != "" {
		if d := q.Get("error_description"); d != "" {
			return "", fmt.Errorf("the sign-in was refused: %s (%s)", d, e)
		}
		return "", fmt.Errorf("the sign-in was refused: %s", e)
	}
	if got := q.Get("state"); got != "" && got != state {
		return "", errStateMismatch
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("the redirect carries no authorization code")
	}
	return code, nil
}

// parseRedirect reads an authorization code out of what the user pasted:
// the whole redirect URL from the browser's address bar, its query string,
// a "code#state" pair, or the bare code.
func parseRedirect(raw, state string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("nothing was pasted")
	}
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("the pasted text is not a URL: %w", err)
		}
		return redirectCode(u.Query(), state)
	case strings.HasPrefix(raw, "?"), strings.Contains(raw, "="):
		q, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil {
			return "", fmt.Errorf("the pasted text is not a redirect query: %w", err)
		}
		return redirectCode(q, state)
	case strings.Contains(raw, "#"):
		code, got, _ := strings.Cut(raw, "#")
		return redirectCode(url.Values{"code": {code}, "state": {got}}, state)
	}
	if strings.ContainsAny(raw, " \t\n?&/") {
		return "", errors.New("the pasted text is not an authorization code")
	}
	return raw, nil
}
