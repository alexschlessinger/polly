package docker

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsupportedHost reports a daemon address this package cannot dial.
var ErrUnsupportedHost = errors.New("unsupported docker host")

// endpoint is how the Engine API is reached: a Unix socket or a TCP
// address, with TLS material for the latter when configured.
type endpoint struct {
	network string
	address string
	tls     *tls.Config
	display string
}

// resolveEndpoint picks the daemon: an explicit override, DOCKER_HOST, the
// active docker context (DOCKER_CONTEXT or the configuration file's current
// context), or the default socket. ssh:// hosts are refused: nothing here
// spawns an ssh process.
func resolveEndpoint(override string) (endpoint, error) {
	if override != "" {
		return parseHost(override, tlsFromEnv)
	}
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return parseHost(host, tlsFromEnv)
	}
	if host, tlsDir, skipVerify, ok, err := contextHost(); err != nil {
		return endpoint{}, err
	} else if ok {
		return parseHost(host, func() (*tls.Config, error) { return tlsFromContext(tlsDir, skipVerify) })
	}
	return parseHost("unix:///var/run/docker.sock", nil)
}

func parseHost(host string, loadTLS func() (*tls.Config, error)) (endpoint, error) {
	parsed, err := url.Parse(host)
	if err != nil {
		return endpoint{}, fmt.Errorf("%w: %q: %v", ErrUnsupportedHost, host, err)
	}
	switch parsed.Scheme {
	case "unix":
		path := parsed.Path
		if path == "" {
			path = parsed.Host
		}
		return endpoint{network: "unix", address: path, display: host}, nil
	case "tcp":
		if parsed.Host == "" {
			return endpoint{}, fmt.Errorf("%w: %q names no address", ErrUnsupportedHost, host)
		}
		ep := endpoint{network: "tcp", address: parsed.Host, display: host}
		if loadTLS != nil {
			ep.tls, err = loadTLS()
			if err != nil {
				return endpoint{}, err
			}
		}
		return ep, nil
	}
	return endpoint{}, fmt.Errorf("%w: %q (unix:// and tcp:// are supported)", ErrUnsupportedHost, host)
}

// dockerConfigDir is ~/.docker or DOCKER_CONFIG.
func dockerConfigDir() (string, error) {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker"), nil
}

// contextHost reads the active docker context's endpoint. The context name
// comes from DOCKER_CONTEXT or the configuration file's currentContext; the
// endpoint from the context's metadata file, keyed by the SHA-256 of the
// name. The default context has no file and reports ok=false.
func contextHost() (host, tlsDir string, skipVerify, ok bool, err error) {
	dir, err := dockerConfigDir()
	if err != nil {
		return "", "", false, false, nil
	}
	name := os.Getenv("DOCKER_CONTEXT")
	if name == "" {
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			return "", "", false, false, nil
		}
		var config struct {
			CurrentContext string `json:"currentContext"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return "", "", false, false, fmt.Errorf("read docker config: %w", err)
		}
		name = config.CurrentContext
	}
	if name == "" || name == "default" {
		return "", "", false, false, nil
	}
	sum := sha256.Sum256([]byte(name))
	id := hex.EncodeToString(sum[:])
	data, err := os.ReadFile(filepath.Join(dir, "contexts", "meta", id, "meta.json"))
	if err != nil {
		return "", "", false, false, fmt.Errorf("docker context %q: %w", name, err)
	}
	var meta struct {
		Endpoints struct {
			Docker struct {
				Host          string `json:"Host"`
				SkipTLSVerify bool   `json:"SkipTLSVerify"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", "", false, false, fmt.Errorf("docker context %q: %w", name, err)
	}
	if meta.Endpoints.Docker.Host == "" {
		return "", "", false, false, fmt.Errorf("docker context %q names no docker endpoint", name)
	}
	return meta.Endpoints.Docker.Host, filepath.Join(dir, "contexts", "tls", id, "docker"), meta.Endpoints.Docker.SkipTLSVerify, true, nil
}

// tlsFromEnv builds the client TLS configuration DOCKER_TLS_VERIFY asks
// for, from DOCKER_CERT_PATH or the docker configuration directory.
func tlsFromEnv() (*tls.Config, error) {
	if os.Getenv("DOCKER_TLS_VERIFY") == "" {
		return nil, nil
	}
	dir := os.Getenv("DOCKER_CERT_PATH")
	if dir == "" {
		var err error
		if dir, err = dockerConfigDir(); err != nil {
			return nil, err
		}
	}
	return loadTLS(dir, false)
}

func tlsFromContext(dir string, skipVerify bool) (*tls.Config, error) {
	if _, err := os.Stat(filepath.Join(dir, "cert.pem")); err != nil {
		if skipVerify {
			return &tls.Config{InsecureSkipVerify: true}, nil
		}
		return nil, nil
	}
	return loadTLS(dir, skipVerify)
}

func loadTLS(dir string, skipVerify bool) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: skipVerify}
	if ca, err := os.ReadFile(filepath.Join(dir, "ca.pem")); err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("docker TLS: %s holds no certificate", filepath.Join(dir, "ca.pem"))
		}
		config.RootCAs = pool
	} else if !skipVerify {
		return nil, fmt.Errorf("docker TLS: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, fmt.Errorf("docker TLS: %w", err)
	}
	config.Certificates = []tls.Certificate{cert}
	return config, nil
}

// String names the endpoint for messages.
func (e endpoint) String() string {
	if e.display != "" {
		return e.display
	}
	return e.network + "://" + strings.TrimPrefix(e.address, "/")
}
