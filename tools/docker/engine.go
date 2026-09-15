package docker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiVersion is the Engine API version requested; every daemon since 2020
// answers it.
const apiVersion = "v1.41"

// ErrUnavailable reports a daemon that did not answer.
var ErrUnavailable = errors.New("docker daemon unavailable")

// ErrImageMissing reports an image the daemon does not hold. Polly never
// pulls.
var ErrImageMissing = errors.New("docker image not present on the daemon")

// apiError is a non-success reply from the daemon.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("docker API: %s (HTTP %d)", e.Message, e.Status)
}

// engine is a minimal Engine API client over one endpoint: no host process,
// nothing in argv, JSON in and out.
type engine struct {
	endpoint endpoint
	client   *http.Client
	base     string
}

func newEngine(ep endpoint) *engine {
	e := &engine{endpoint: ep, base: "http://docker"}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return e.dial(ctx) }}
	if ep.network == "tcp" {
		e.base = "http://" + ep.address
		if ep.tls != nil {
			e.base = "https://" + ep.address
			transport.TLSClientConfig = ep.tls
			transport.DialTLSContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return e.dial(ctx) }
		}
	}
	e.client = &http.Client{Transport: transport}
	return e
}

// dial opens one connection to the daemon.
func (e *engine) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, e.endpoint.network, e.endpoint.address)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, e.endpoint, err)
	}
	if e.endpoint.tls != nil {
		host, _, _ := net.SplitHostPort(e.endpoint.address)
		config := e.endpoint.tls.Clone()
		config.ServerName = host
		return tls.Client(conn, config), nil
	}
	return conn, nil
}

// do performs one request; a non-2xx reply becomes an apiError.
func (e *engine) do(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(encoded)
	}
	target := e.base + "/" + apiVersion + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return 0, err
	}
	req.Host = "docker"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %s: %v", ErrUnavailable, e.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return resp.StatusCode, readAPIError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return resp.StatusCode, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func readAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var message struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &message) != nil || message.Message == "" {
		message.Message = strings.TrimSpace(string(data))
	}
	if message.Message == "" {
		message.Message = resp.Status
	}
	return &apiError{Status: resp.StatusCode, Message: message.Message}
}

func isStatus(err error, status int) bool {
	var failure *apiError
	return errors.As(err, &failure) && failure.Status == status
}

func (e *engine) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base+"/_ping", nil)
	if err != nil {
		return err
	}
	req.Host = "docker"
	resp, err := e.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return err
		}
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, e.endpoint, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s answered ping with HTTP %d", ErrUnavailable, e.endpoint, resp.StatusCode)
	}
	return nil
}

type versionInfo struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	OS         string `json:"Os"`
	Arch       string `json:"Arch"`
}

func (e *engine) version(ctx context.Context) (versionInfo, error) {
	var info versionInfo
	_, err := e.do(ctx, http.MethodGet, "/version", nil, nil, &info)
	return info, err
}

type daemonInfo struct {
	Rootless bool `json:"Rootless"`
}

func (e *engine) info(ctx context.Context) (daemonInfo, error) {
	var info daemonInfo
	_, err := e.do(ctx, http.MethodGet, "/info", nil, nil, &info)
	return info, err
}

type imageInfo struct {
	ID string `json:"Id"`
}

func (e *engine) imageInspect(ctx context.Context, ref string) (imageInfo, error) {
	var info imageInfo
	_, err := e.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &info)
	if isStatus(err, http.StatusNotFound) {
		return info, fmt.Errorf("%w: %s", ErrImageMissing, ref)
	}
	return info, err
}

type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// containerList lists every container, running or not, carrying all of the
// given label filters (NAME=VALUE, or NAME for presence).
func (e *engine) containerList(ctx context.Context, labels []string) ([]containerSummary, error) {
	filters, err := json.Marshal(map[string][]string{"label": labels})
	if err != nil {
		return nil, err
	}
	query := url.Values{"all": {"1"}, "filters": {string(filters)}}
	var containers []containerSummary
	_, err = e.do(ctx, http.MethodGet, "/containers/json", query, nil, &containers)
	return containers, err
}

type containerDetail struct {
	ID    string `json:"Id"`
	State struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Image  string            `json:"Image"`
	} `json:"Config"`
}

func (e *engine) containerInspect(ctx context.Context, id string) (containerDetail, error) {
	var detail containerDetail
	_, err := e.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &detail)
	return detail, err
}

// containerCreate creates a named container; 409 means another container
// already holds the name.
func (e *engine) containerCreate(ctx context.Context, name string, body createBody) (string, error) {
	var created struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	_, err := e.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &created)
	if isStatus(err, http.StatusConflict) {
		return "", fmt.Errorf("container name %s is taken by a container polly does not own: %w", name, err)
	}
	return created.ID, err
}

func (e *engine) containerStart(ctx context.Context, id string) error {
	status, err := e.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
	if status == http.StatusNotModified {
		return nil
	}
	return err
}

// containerRemove removes a container and its anonymous volumes, stopping
// it first; a missing container is already removed.
func (e *engine) containerRemove(ctx context.Context, id string) error {
	_, err := e.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

type execBody struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
	User         string   `json:"User,omitempty"`
}

func (e *engine) execCreate(ctx context.Context, id string, body execBody) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	_, err := e.do(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, body, &created)
	return created.ID, err
}

type execDetail struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

func (e *engine) execInspect(ctx context.Context, id string) (execDetail, error) {
	var detail execDetail
	_, err := e.do(ctx, http.MethodGet, "/exec/"+id+"/json", nil, nil, &detail)
	return detail, err
}

// hijacked is an exec's attached stream: raw bytes in, multiplexed frames out.
type hijacked struct {
	conn   net.Conn
	reader *bufio.Reader
}

// closeWrite signals end of input to the exec; the helper reads EOF.
func (h *hijacked) closeWrite() error {
	if closer, ok := h.conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return h.conn.Close()
}

// execStart starts an exec and upgrades the connection to its stream. The
// request is written by hand: the client keeps the raw connection.
func (e *engine) execStart(ctx context.Context, id string) (*hijacked, error) {
	conn, err := e.dial(ctx)
	if err != nil {
		return nil, err
	}
	body := `{"Detach":false,"Tty":false}`
	req, err := http.NewRequest(http.MethodPost, e.base+"/"+apiVersion+"/exec/"+id+"/start", strings.NewReader(body))
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Host = "docker"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("start exec: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start exec: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		err := readAPIError(resp)
		conn.Close()
		return nil, fmt.Errorf("start exec: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return &hijacked{conn: conn, reader: reader}, nil
}

// putArchive extracts a tar stream into the container at path.
func (e *engine) putArchive(ctx context.Context, id, path string, archive io.Reader) error {
	target := e.base + "/" + apiVersion + "/containers/" + id + "/archive?" + url.Values{"path": {path}, "copyUIDGID": {"1"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, archive)
	if err != nil {
		return err
	}
	req.Host = "docker"
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, e.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readAPIError(resp)
	}
	return nil
}

// getArchive fetches a tar of the container path; the caller closes it.
func (e *engine) getArchive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	target := e.base + "/" + apiVersion + "/containers/" + id + "/archive?" + url.Values{"path": {path}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Host = "docker"
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, e.endpoint, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readAPIError(resp)
	}
	return resp.Body, nil
}
