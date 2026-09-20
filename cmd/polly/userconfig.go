package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/llm"
)

// The user configuration file holds the defaults the setup form saves: one
// POLLYTOOL_* variable per line, exactly the names the flags read from the
// environment. Each flag reads it through fileDefaultSource after the
// environment, so precedence is flag, then environment, then this file,
// then the built-in default. Provider keys never live here: the reader
// skips POLLYTOOL_*KEY lines, and the form does not write them.
const (
	userConfigDirName  = ".pollytool"
	userConfigFileName = "config"
)

// userConfigHeader opens every file this process writes. A header-only file
// is a valid, empty configuration: it records that setup ran (or was
// skipped) so the first-run form does not reopen.
const userConfigHeader = "# polly defaults · one POLLYTOOL_* variable per line · flags and the environment override\n"

// userConfigDisplayPath is the path as notices spell it.
const userConfigDisplayPath = "~/" + userConfigDirName + "/" + userConfigFileName

var userConfigKeyPattern = regexp.MustCompile(`^POLLYTOOL_[A-Z0-9_]+$`)

// The configuration variables the setup form and set_theme persist: exactly
// the names their flags read through envDefault, declared once so the
// writer and the reader cannot drift.
const (
	envVarModel     = "POLLYTOOL_MODEL"
	envVarModelHost = "POLLYTOOL_MODELHOST"
	envVarBaseURL   = "POLLYTOOL_BASEURL"
	envVarEffort    = "POLLYTOOL_EFFORT"
	// envVarThinking is the former spelling of envVarEffort, still read so
	// an existing configuration file keeps working. Saving replaces it.
	envVarThinking  = "POLLYTOOL_THINKING"
	envVarTheme     = "POLLYTOOL_THEME"
	envVarNoSandbox = "POLLYTOOL_NOSANDBOX"

	// The variables that carry a sandbox policy. --nosandbox refuses to
	// coexist with any of them, so they are named once and paired with their
	// flags in sandboxPolicySources.
	envVarSandbox    = "POLLYTOOL_SANDBOX"
	envVarDenyPaths  = "POLLYTOOL_DENYPATHS"
	envVarWritePaths = "POLLYTOOL_WRITEPATHS"
	envVarReadPaths  = "POLLYTOOL_READPATHS"
	envVarAllowNet   = "POLLYTOOL_ALLOWNET"
)

// userConfigPath is ~/.pollytool/config.
func userConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, userConfigDirName, userConfigFileName), nil
}

// userConfigExists reports whether the file is present, whatever it holds.
func userConfigExists() bool {
	path, err := userConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// isProviderKeyVar reports whether name is a provider credential variable.
func isProviderKeyVar(name string) bool {
	return strings.HasPrefix(name, "POLLYTOOL_") && strings.HasSuffix(name, "KEY")
}

// readUserConfig parses the file into its variables. A missing file is an
// empty configuration. Malformed and key lines are skipped and reported in
// problems so one bad line cannot take every default with it.
func readUserConfig(path string) (values map[string]string, problems []string, err error) {
	values = map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return values, nil, nil
		}
		return nil, nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !userConfigKeyPattern.MatchString(key) {
			problems = append(problems, fmt.Sprintf("line %d: expected POLLYTOOL_NAME=value", n))
			continue
		}
		if isProviderKeyVar(key) {
			problems = append(problems, fmt.Sprintf("line %d: %s ignored; keys are read from the environment only", n, key))
			continue
		}
		values[key] = unquoteUserConfigValue(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, problems, err
	}
	return values, problems, nil
}

// unquoteUserConfigValue strips one pair of matching quotes so a value
// pasted from a shell export reads the same here.
func unquoteUserConfigValue(value string) string {
	if len(value) >= 2 {
		if q := value[0]; (q == '"' || q == '\'') && value[len(value)-1] == q {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// userConfigCache holds the parsed file for the flag sources, read once per
// path and dropped when this process rewrites the file. Problems are
// reported on stderr the first time and never stop a run.
var userConfigCache struct {
	sync.Mutex
	path   string
	loaded bool
	values map[string]string
}

func userConfigValue(key string) (string, bool) {
	path, err := userConfigPath()
	if err != nil {
		return "", false
	}
	userConfigCache.Lock()
	defer userConfigCache.Unlock()
	if !userConfigCache.loaded || userConfigCache.path != path {
		values, problems, err := readUserConfig(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "polly: %s: %v\n", userConfigDisplayPath, err)
			values = map[string]string{}
		}
		for _, problem := range problems {
			fmt.Fprintf(os.Stderr, "polly: %s: %s\n", userConfigDisplayPath, problem)
		}
		userConfigCache.path, userConfigCache.loaded, userConfigCache.values = path, true, values
	}
	value, ok := userConfigCache.values[key]
	return value, ok
}

// writeUserConfig merges updates into the file and rewrites it: a non-empty
// value replaces the key, an empty value removes it, and keys not mentioned
// keep their stored value.
func writeUserConfig(path string, updates map[string]string) error {
	values, _, err := readUserConfig(path)
	if err != nil {
		return err
	}
	for key, value := range updates {
		if value == "" {
			delete(values, key)
		} else {
			values[key] = value
		}
	}
	var b strings.Builder
	b.WriteString(userConfigHeader)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		fmt.Fprintf(&b, "%s=%s\n", key, values[key])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	userConfigCache.Lock()
	userConfigCache.loaded = false
	userConfigCache.Unlock()
	return nil
}

// shadowedByEnvironment lists the saved keys the environment overrides on
// every launch, since it is read before the file.
func shadowedByEnvironment(updates map[string]string) []string {
	var keys []string
	for key := range updates {
		if _, ok := os.LookupEnv(key); ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// firstRunPending reports whether this launch should open the setup form
// unasked: no configuration file records an earlier setup. Flags and
// environment values still apply; the form opens with them as its draft.
// Keys play no part; a missing key is a separate startup gate
// (missingKeyError).
func firstRunPending() bool { return !userConfigExists() }

// missingKeyError is the startup refusal when the provider of the model the
// session will run on needs a credential and none is configured. Keys come
// only from the environment, so every frontend fails the same way; the
// check runs after the session opens so a resumed session is judged by its
// own stored model rather than the launch default.
func missingKeyError(client *llm.MultiPass, model, baseURL string) error {
	if client == nil {
		return nil
	}
	envVar, missing := client.MissingAPIKey(model, baseURL)
	if !missing {
		return nil
	}
	provider, _, _ := strings.Cut(model, "/")
	return fmt.Errorf("no API key configured for provider '%s': export %s, or pick another provider with polly --setup", provider, envVar)
}
