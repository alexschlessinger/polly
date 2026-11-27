package control

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ProviderKeyVars are the environment variables that hold a provider's API
// key, by provider, as the backend reads them. Only these providers are
// served by the control backend.
var ProviderKeyVars = map[string]string{
	"anthropic":   "POLLYTOOL_ANTHROPICKEY",
	"openai":      "POLLYTOOL_OPENAIKEY",
	"openrouter":  "POLLYTOOL_OPENROUTERKEY",
	"deepseek":    "POLLYTOOL_DEEPSEEKKEY",
	"huggingface": "POLLYTOOL_HUGGINGFACEKEY",
}

// SupportedProviders lists the providers a control backend serves, sorted.
func SupportedProviders() []string {
	out := make([]string, 0, len(ProviderKeyVars))
	for name := range ProviderKeyVars {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// ProviderKeyVar is the variable that holds provider's key, or "" for a
// provider the backend does not serve.
func ProviderKeyVar(provider string) string {
	return ProviderKeyVars[strings.ToLower(provider)]
}

// KeylessWithBaseURL reports whether provider runs without a key when it is
// given a base URL (a local OpenAI-compatible server).
func KeylessWithBaseURL(provider string) bool { return strings.ToLower(provider) == "openai" }

// IsProviderKeyVar reports whether name holds any served provider's key.
func IsProviderKeyVar(name string) bool {
	for _, v := range ProviderKeyVars {
		if v == name {
			return true
		}
	}
	return false
}

// SetupEnvVar lists extra variables, comma-separated, that are pinned into a
// setup's key beside SetupEnv.
const SetupEnvVar = "POLLYTOOL_SETUP_ENV"

// setupEnv is the allowlist of variables that are part of a setup's identity:
// they split workers, and are never overridden per prompt.
var setupEnv = map[string]bool{}

func init() {
	for _, name := range []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
		"SSH_AUTH_SOCK", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
		"LANG", "LC_ALL", "TZ", "POLLYTOOL_RUNTIME_DIR", SetupEnvVar,
		"BASH_ENV", "ENV", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
		"DYLD_FRAMEWORK_PATH", "IFS", "ZDOTDIR", "PYTHONSTARTUP",
	} {
		setupEnv[name] = true
	}
}

// IsSetupEnv reports whether name is on the setup's allowlist.
func IsSetupEnv(name string) bool { return setupEnv[name] }

func isExtraSetupEnv(env map[string]string, name string) bool {
	raw, ok := env[SetupEnvVar]
	if !ok {
		return false
	}
	for _, token := range strings.Split(raw, ",") {
		if t := strings.Trim(token, " \t"); t != "" && t == name {
			return true
		}
	}
	return false
}

// clearablePromptEnv are a launch's terminal and session variables: each
// crosses with every prompt, as null where the launch has none, so a shared
// daemon never leaks the shell that started it into a later turn.
var clearablePromptEnv = []string{
	"_", "SHLVL", "OLDPWD", "PWD", "TERM", "TERMINFO", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION",
	"TERM_SESSION_ID", "ITERM_SESSION_ID", "ITERM_PROFILE", "TMUX", "TMUX_PANE", "WEZTERM_PANE",
	"WEZTERM_UNIX_SOCKET", "KITTY_WINDOW_ID", "KITTY_PID", "KITTY_LISTEN_ON", "ALACRITTY_WINDOW_ID",
	"ALACRITTY_SOCKET", "WT_SESSION", "WINDOWID", "STY", "WINDOW", "COLUMNS", "LINES", "GPG_TTY", "SSH_TTY",
	"SSH_CLIENT", "SSH_CONNECTION", "VIRTUAL_ENV", "CONDA_DEFAULT_ENV", "CONDA_PREFIX",
}

// isBlockedPromptEnv reports a variable a prompt may not carry: one pinned to
// the setup, a provider's key, or a launcher switch.
func isBlockedPromptEnv(name string) bool {
	if setupEnv[name] {
		return true
	}
	if strings.HasPrefix(name, "POLLYTOOL_") && strings.HasSuffix(name, "KEY") {
		return true
	}
	if strings.HasPrefix(name, "POLLY_ZIG_") || name == "POLLYTOOL_IDLE" {
		return true
	}
	return false
}

// EnvOver is one variable a prompt sets for its turn's tools; a nil Value
// clears it.
type EnvOver struct {
	Name  string
	Value *string
}

// MarshalJSON writes the pair the contract takes: [name, value-or-null].
func (o EnvOver) MarshalJSON() ([]byte, error) {
	name, _ := jsonString(o.Name)
	if o.Value == nil {
		return []byte("[" + name + ",null]"), nil
	}
	value, _ := jsonString(*o.Value)
	return []byte("[" + name + "," + value + "]"), nil
}

// UnmarshalJSON reads the pair the contract writes.
func (o *EnvOver) UnmarshalJSON(data []byte) error {
	var pair []*string
	if err := json.Unmarshal(data, &pair); err != nil {
		return err
	}
	if len(pair) != 2 || pair[0] == nil {
		return fmt.Errorf("an env pair is [name, value or null], got %s", data)
	}
	o.Name, o.Value = *pair[0], pair[1]
	return nil
}

// PromptEnv is a launch's own variables, as each prompt carries them: the
// clearable session variables, with null where absent, and then every other
// variable that is not the setup's, not a key and not a launcher switch, by
// name.
func PromptEnv(environ []string) []EnvOver {
	env := environMap(environ)
	var out []EnvOver
	seen := map[string]bool{}
	for _, name := range clearablePromptEnv {
		if isExtraSetupEnv(env, name) {
			continue
		}
		seen[name] = true
		over := EnvOver{Name: name}
		if value, ok := env[name]; ok {
			over.Value = &value
		}
		out = append(out, over)
	}
	var extra []string
	for name, value := range env {
		if seen[name] || isBlockedPromptEnv(name) || IsProviderKeyVar(name) || isExtraSetupEnv(env, name) {
			continue
		}
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
			continue
		}
		extra = append(extra, name)
	}
	slices.Sort(extra)
	for _, name := range extra {
		value := env[name]
		out = append(out, EnvOver{Name: name, Value: &value})
	}
	return out
}

// withoutProviderKeys is environ less every provider's key variable.
func withoutProviderKeys(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if IsProviderKeyVar(name) || (strings.HasPrefix(name, "POLLYTOOL_") && strings.HasSuffix(name, "KEY")) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
