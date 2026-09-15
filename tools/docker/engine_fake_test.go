package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/docker/helper"
)

// fakeEngine is an Engine API daemon in an httptest server: it records
// every request body, keeps a container table, and answers an exec start
// by hijacking the connection and running the real helper over it, so the
// whole host path is exercised without Docker.
type fakeEngine struct {
	t      *testing.T
	server *httptest.Server
	opts   helper.Options

	mu         sync.Mutex
	images     map[string]string
	containers map[string]*fakeContainer
	creates    []createBody
	execs      []execBody
	starts     []string
	removed    []string
	streams    []*bytes.Buffer // what the host sent on each exec stream
	rootless   bool
	nextID     int
}

type fakeContainer struct {
	id      string
	name    string
	labels  map[string]string
	running bool
}

func newFakeEngine(t *testing.T, opts helper.Options) *fakeEngine {
	t.Helper()
	if opts.Home == "" {
		opts.Home = filepath.Join(t.TempDir(), "home")
	}
	f := &fakeEngine{t: t, opts: opts, images: map[string]string{"test:image": "sha256:image-one"}, containers: map[string]*fakeContainer{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("OK")) })
	mux.HandleFunc("GET /v1.41/version", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Version": "29.4.0", "ApiVersion": "1.54", "Os": "linux", "Arch": "arm64"})
	})
	mux.HandleFunc("GET /v1.41/info", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		rootless := f.rootless
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"Rootless": rootless})
	})
	mux.HandleFunc("GET /v1.41/images/{ref}/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		id, ok := f.images[r.PathValue("ref")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Id": id})
	})
	mux.HandleFunc("GET /v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		var filters struct {
			Label []string `json:"label"`
		}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []containerSummary
		for _, c := range f.containers {
			if !hasLabels(c.labels, filters.Label) {
				continue
			}
			state := "exited"
			if c.running {
				state = "running"
			}
			out = append(out, containerSummary{ID: c.id, Names: []string{"/" + c.name}, State: state, Labels: c.labels})
		}
		if out == nil {
			out = []containerSummary{}
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /v1.41/containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body createBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := r.URL.Query().Get("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.containers {
			if c.name == name {
				http.Error(w, `{"message":"Conflict. The container name is already in use"}`, http.StatusConflict)
				return
			}
		}
		f.nextID++
		id := fmt.Sprintf("container-%d", f.nextID)
		f.containers[id] = &fakeContainer{id: id, name: name, labels: body.Labels}
		f.creates = append(f.creates, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"Id": id, "Warnings": []string{}})
	})
	mux.HandleFunc("GET /v1.41/containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		c := f.containers[r.PathValue("id")]
		f.mu.Unlock()
		if c == nil {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		status := "exited"
		if c.running {
			status = "running"
		}
		json.NewEncoder(w).Encode(map[string]any{"Id": c.id, "State": map[string]any{"Running": c.running, "Status": status}, "Config": map[string]any{"Labels": c.labels}})
	})
	mux.HandleFunc("POST /v1.41/containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.containers[r.PathValue("id")]
		if c == nil {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		f.starts = append(f.starts, c.id)
		c.running = true
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1.41/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		if f.containers[id] == nil {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		delete(f.containers, id)
		f.removed = append(f.removed, id)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1.41/containers/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var body execBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.execs = append(f.execs, body)
		id := fmt.Sprintf("exec-%d", len(f.execs))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"Id": id})
	})
	mux.HandleFunc("GET /v1.41/exec/{id}/json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": 0})
	})
	mux.HandleFunc("POST /v1.41/exec/{id}/start", f.startExec)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// startExec upgrades the connection and serves the helper over it.
func (f *fakeEngine) startExec(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	rw.Flush()
	received := &bytes.Buffer{}
	f.mu.Lock()
	f.streams = append(f.streams, received)
	f.mu.Unlock()
	stdin := io.TeeReader(rw.Reader, received)
	_ = helper.Serve(context.Background(), stdin, stdcopyWriter{writer: conn, stream: streamStdout}, f.opts)
}

func (f *fakeEngine) host() string {
	address := strings.TrimPrefix(f.server.URL, "http://")
	return "tcp://" + address
}

func (f *fakeEngine) snapshot() (creates []createBody, execs []execBody, starts, removed []string, containers map[string]*fakeContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	containers = map[string]*fakeContainer{}
	for id, c := range f.containers {
		copy := *c
		containers[id] = &copy
	}
	return append([]createBody(nil), f.creates...), append([]execBody(nil), f.execs...), append([]string(nil), f.starts...), append([]string(nil), f.removed...), containers
}

func (f *fakeEngine) setImage(ref, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = id
}

func (f *fakeEngine) stopAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		c.running = false
	}
}

func (f *fakeEngine) received() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all strings.Builder
	for _, s := range f.streams {
		all.Write(s.Bytes())
	}
	return all.String()
}

func hasLabels(have map[string]string, filters []string) bool {
	for _, filter := range filters {
		key, value, withValue := strings.Cut(filter, "=")
		got, ok := have[key]
		if !ok || withValue && got != value {
			return false
		}
	}
	return true
}

// Ensure the fake's hijack path matches what the client expects.
var _ net.Conn = (*net.TCPConn)(nil)
var _ = bufio.NewReader
