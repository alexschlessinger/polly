package main

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// setupDefaults is what a completed setup persists: the model settings and
// the sandbox default. The form's Apply, the line questionnaire, and a
// command line naming every default all write through here.
type setupDefaults struct {
	model, host, endpoint, thinking string
	sandbox                         bool
	sandboxPreset                   string
}

// check refuses a sandbox default the environment contradicts. Polly
// refuses to start with a sandbox policy and no sandbox, so the pair is
// refused here rather than at the next launch, which could not be talked out
// of it. Only the environment can hold the other half: a saved line is one
// write overwrites.
func (d setupDefaults) check() error {
	if d.sandbox && noSandboxExported() {
		return fmt.Errorf("%s is set in your environment, and a launch refuses to start with a sandbox policy and no sandbox; unset it or choose none", envVarNoSandbox)
	}
	return nil
}

// write saves the defaults to ~/.pollytool/config and returns the notices a
// completed setup reports.
func (d setupDefaults) write() ([]string, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	// Empty values drop the line; the built-in defaults need none. The effort
	// is always written: every one of its words is a choice, and off is not
	// what polly does without being told. The sandbox is the reverse — none
	// is what polly does untold, so only a policy is written, and the former
	// spelling of its off state goes with it.
	sandboxPreset := ""
	if d.sandbox {
		sandboxPreset = d.sandboxPreset
	}
	updates := map[string]string{
		envVarModel:     d.model,
		envVarModelHost: d.host,
		envVarBaseURL:   strings.TrimSpace(d.endpoint),
		envVarEffort:    d.thinking,
		envVarSandbox:   sandboxPreset,
		envVarNoSandbox: "",
		// The former spelling of the effort line would outlive the line that
		// replaces it, so saving removes it.
		envVarThinking: "",
	}
	path, err := userConfigPath()
	if err != nil {
		return nil, err
	}
	if err := writeUserConfig(path, updates); err != nil {
		return nil, err
	}
	notices := []string{"defaults saved to " + userConfigDisplayPath}
	// The saved effort line outranks an exported former spelling, so that
	// variable shadows nothing and stays out of the report.
	delete(updates, envVarThinking)
	if shadowed := shadowedByEnvironment(updates); len(shadowed) > 0 {
		notices = append(notices, "set in your environment and overriding the file on the next launch: "+strings.Join(shadowed, ", "))
	}
	return notices, nil
}

// recordSetupSkip records a dismissed setup as an empty configuration when
// none exists yet, so the next launch starts straight into the conversation.
// It returns the notice to show.
func recordSetupSkip() string {
	if userConfigExists() {
		return ""
	}
	path, err := userConfigPath()
	if err == nil {
		err = writeUserConfig(path, nil)
	}
	if err != nil {
		return "Setup skipped · could not record it · " + err.Error()
	}
	return "Setup skipped · /setup or polly --setup reopens it"
}

// setupFlagsComplete reports whether the command line names every default
// setup asks about: the model, the effort, the theme, and a sandbox preset
// or none. The endpoint and model host are extras, saved as resolved.
func setupFlagsComplete(cmd *cli.Command) bool {
	if cmd == nil {
		return false
	}
	return flagGiven(cmd, "model") && flagGiven(cmd, "effort") && flagGiven(cmd, "theme") &&
		(flagGiven(cmd, "sandbox") || flagGiven(cmd, "nosandbox"))
}

// saveSetupFromFlags is setup without a form: the launch's resolved settings
// are saved as the defaults, judged the way Apply judges a draft. The
// provider needs its key, and the theme has to load.
func (r *commandRunner) saveSetupFromFlags(w io.Writer) error {
	config := r.config
	if err := missingKeyError(r.llmClient, config.Launch.Model, config.BaseURL); err != nil {
		return err
	}
	if _, err := resolveThemeSelection(config.Theme); err != nil {
		return fmt.Errorf("theme %s: %w", themeDisplayPath(config.Theme), err)
	}
	defaults := setupDefaults{
		model:         config.Launch.Model,
		host:          config.Launch.ModelHost,
		endpoint:      config.BaseURL,
		thinking:      config.Launch.ThinkingEffort,
		sandbox:       !config.NoSandbox,
		sandboxPreset: config.SandboxPreset,
	}
	notices, err := defaults.write()
	if err != nil {
		return err
	}
	if err := saveThemeSelection(config.Theme); err != nil {
		return fmt.Errorf("theme not saved: %w", err)
	}
	if warning := themeShadowedWarning(config.Theme); warning != "" {
		notices = append(notices, "Warning: "+warning)
	}
	for _, notice := range notices {
		fmt.Fprintln(w, notice)
	}
	return nil
}

// errSetupDismissed is the questionnaire's answer to end of input: setup
// was skipped, the way Escape skips the form.
var errSetupDismissed = errors.New("setup dismissed")

// runTextSetup is the setup form for a terminal without the TUI: the same
// questions, one line each, with the launch's resolved value as the answer
// Enter keeps. The answers are saved as the defaults and applied to this
// launch; end of input skips setup and records the skip.
func (r *commandRunner) runTextSetup(in io.Reader, out io.Writer) error {
	config := r.config
	reader := bufio.NewReader(in)
	ask := func(label, current string) (string, error) {
		fmt.Fprintf(out, "%s [%s]: ", label, current)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out)
			return "", errSetupDismissed
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return current, nil
		}
		return line, nil
	}
	// A wrong answer asks again rather than ending the questionnaire.
	askValid := func(label, current string, check func(string) error) (string, error) {
		for {
			answer, err := ask(label, current)
			if err != nil {
				return "", err
			}
			if err := check(answer); err != nil {
				fmt.Fprintln(out, err)
				continue
			}
			return answer, nil
		}
	}

	err := func() error {
		provider, modelName, _ := strings.Cut(config.Launch.Model, "/")
		provider, err := askValid("Provider ("+strings.Join(validModelProviders, ", ")+")", provider, func(v string) error {
			return validateModel(v + "/model")
		})
		if err != nil {
			return err
		}
		modelName, err = askValid("Model", modelName, func(v string) error {
			return validateModel(provider + "/" + strings.TrimPrefix(v, provider+"/"))
		})
		if err != nil {
			return err
		}
		model := provider + "/" + strings.TrimPrefix(modelName, provider+"/")
		// The host is a route the picker discovered for one model; another
		// model has none.
		host := ""
		if model == config.Launch.Model {
			host = config.Launch.ModelHost
		}
		endpoint, err := ask("Endpoint (none for the provider's own)", cmp.Or(config.BaseURL, "none"))
		if err != nil {
			return err
		}
		if endpoint == "none" {
			endpoint = ""
		}
		if llm.ProviderRequiresKey(model, endpoint) && r.llmClient != nil && r.llmClient.APIKeySource(provider) == "" {
			key, err := r.readSetupKey(reader, in, out, provider)
			if err != nil {
				return err
			}
			r.llmClient.SetAPIKey(provider, key)
		}
		thinking, err := askValid("Effort ("+strings.Join(llm.ThinkingEffortWords(), ", ")+" or a token budget)", cmp.Or(config.Launch.ThinkingEffort, defaultThinkingEffort), func(v string) error {
			_, err := llm.ParseThinkingEffort(v)
			return err
		})
		if err != nil {
			return err
		}
		theme, err := askValid("Theme ("+strings.Join(allThemeNames(), ", ")+")", themeFollowFlag(config), func(v string) error {
			_, err := resolveThemeSelection(v)
			return err
		})
		if err != nil {
			return err
		}
		currentPreset := "none"
		if !config.NoSandbox {
			currentPreset = cmp.Or(config.SandboxPreset, defaultSandboxPreset)
		}
		preset, err := askValid("Sandbox preset (none to run unsandboxed)", currentPreset, func(v string) error {
			if v == "none" {
				return nil
			}
			return validateSandboxPresetSpec(v)
		})
		if err != nil {
			return err
		}
		defaults := setupDefaults{model: model, host: host, endpoint: endpoint, thinking: thinking, sandbox: preset != "none", sandboxPreset: expandSandboxPreset(preset)}
		notices, err := defaults.write()
		if err != nil {
			return err
		}
		if err := saveThemeSelection(theme); err != nil {
			return fmt.Errorf("theme not saved: %w", err)
		}
		if warning := themeShadowedWarning(theme); warning != "" {
			notices = append(notices, "Warning: "+warning)
		}
		// This launch has opened nothing yet, so the defaults are its
		// settings too.
		config.Launch.Model, config.Launch.ModelHost, config.Launch.ThinkingEffort = model, host, thinking
		config.BaseURL, config.Theme = endpoint, theme
		config.NoSandbox, config.SandboxPreset = !defaults.sandbox, defaults.sandboxPreset
		for _, notice := range notices {
			fmt.Fprintln(out, notice)
		}
		return nil
	}()
	if errors.Is(err, errSetupDismissed) {
		if notice := recordSetupSkip(); notice != "" {
			fmt.Fprintln(out, notice)
		}
		return nil
	}
	return err
}

// readSetupKey asks for a provider key, without echo when the input is a
// terminal. The key is a process override like /keys; the next launch needs
// it in the environment. An empty answer ends setup the way Apply refuses a
// keyless provider.
func (r *commandRunner) readSetupKey(reader *bufio.Reader, in io.Reader, out io.Writer, provider string) (string, error) {
	envVar := llm.ProviderKeyEnvVar(provider)
	fmt.Fprintf(out, "Key for %s (kept for this process only; export %s for the next launch): ", provider, envVar)
	var key string
	if file, ok := in.(*os.File); ok && terminalFD(int(file.Fd())) {
		raw, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", errSetupDismissed
		}
		key = string(raw)
	} else {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out)
			return "", errSetupDismissed
		}
		key = line
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("no API key for provider '%s': enter one, or export %s", provider, envVar)
	}
	return key, nil
}
