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

// setupDefaults is what a completed setup persists: the model settings, the
// theme, and the sandbox default. The form's Apply, the line questionnaire,
// and a command line naming every default all write through here.
type setupDefaults struct {
	model, host, endpoint, thinking string
	// theme is the name to save; empty leaves the saved theme alone.
	theme string
	// sandboxPreset is the policy later launches sandbox under; empty runs
	// them unsandboxed.
	sandboxPreset string
}

// check refuses a sandbox default the environment contradicts. Polly
// refuses to start with a sandbox policy and no sandbox, so the pair is
// refused here rather than at the next launch, which could not be talked out
// of it. Only the environment can hold the other half: a saved line is one
// write overwrites.
func (d setupDefaults) check() error {
	if d.sandboxPreset != "" && noSandboxExported() {
		return fmt.Errorf("%s is set in your environment, and a launch refuses to start with a sandbox policy and no sandbox; unset it or choose none", envVarNoSandbox)
	}
	return nil
}

// write saves the defaults to ~/.pollytool/config in one write and returns
// the notices a completed setup reports.
func (d setupDefaults) write() ([]string, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	// Empty values drop the line; the built-in defaults need none. The effort
	// is always written: every one of its words is a choice, and off is not
	// what polly does without being told. The sandbox is the reverse — none
	// is what polly does untold, so only a policy is written, and the former
	// spelling of its off state goes with it.
	updates := map[string]string{
		envVarModel:     d.model,
		envVarModelHost: d.host,
		envVarBaseURL:   strings.TrimSpace(d.endpoint),
		envVarEffort:    d.thinking,
		envVarSandbox:   d.sandboxPreset,
		envVarNoSandbox: "",
		// The former spelling of the effort line would outlive the line that
		// replaces it, so saving removes it.
		envVarThinking: "",
	}
	if d.theme != "" {
		updates[envVarTheme] = d.theme
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
	// variable shadows nothing and stays out of the report. The theme has
	// its own warning, worded for /theme as well.
	delete(updates, envVarThinking)
	delete(updates, envVarTheme)
	if shadowed := shadowedByEnvironment(updates); len(shadowed) > 0 {
		notices = append(notices, "set in your environment and overriding the file on the next launch: "+strings.Join(shadowed, ", "))
	}
	if d.theme != "" {
		if warning := themeShadowedWarning(d.theme); warning != "" {
			notices = append(notices, "Warning: "+warning)
		}
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
		model:    config.Launch.Model,
		host:     config.Launch.ModelHost,
		endpoint: config.BaseURL,
		thinking: config.Launch.ThinkingEffort,
		theme:    config.Theme,
	}
	if !config.NoSandbox {
		defaults.sandboxPreset = config.SandboxPreset
	}
	notices, err := defaults.write()
	if err != nil {
		return err
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
	q := &setupQuestions{runner: r, reader: bufio.NewReader(in), in: in, out: out}
	err := q.run()
	if errors.Is(err, errSetupDismissed) {
		if notice := recordSetupSkip(); notice != "" {
			fmt.Fprintln(out, notice)
		}
		return nil
	}
	return err
}

// setupQuestions asks the questionnaire on one reader and writer.
type setupQuestions struct {
	runner *commandRunner
	reader *bufio.Reader
	in     io.Reader
	out    io.Writer
}

// ask shows the answer Enter keeps and reads one line; end of input
// dismisses setup.
func (q *setupQuestions) ask(label, current string) (string, error) {
	fmt.Fprintf(q.out, "%s [%s]: ", label, current)
	line, err := readLine(q.reader)
	if err != nil {
		fmt.Fprintln(q.out)
		return "", errSetupDismissed
	}
	return cmp.Or(strings.TrimSpace(line), current), nil
}

// askValid asks again after a wrong answer rather than ending setup.
func (q *setupQuestions) askValid(label, current string, check func(string) error) (string, error) {
	for {
		answer, err := q.ask(label, current)
		if err != nil {
			return "", err
		}
		if err := check(answer); err != nil {
			fmt.Fprintln(q.out, err)
			continue
		}
		return answer, nil
	}
}

func (q *setupQuestions) run() error {
	config, client := q.runner.config, q.runner.llmClient
	provider, modelName, _ := strings.Cut(config.Launch.Model, "/")
	provider, err := q.askValid("Provider ("+strings.Join(validModelProviders, ", ")+")", provider, func(v string) error {
		return validateModel(v + "/model")
	})
	if err != nil {
		return err
	}
	modelName, err = q.askValid("Model", modelName, func(v string) error {
		if v == "" || strings.ContainsAny(v, " \t") {
			return errors.New("enter a model name without spaces")
		}
		return nil
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
	endpoint, err := q.ask("Endpoint (none for the provider's own)", cmp.Or(config.BaseURL, "none"))
	if err != nil {
		return err
	}
	if endpoint == "none" {
		endpoint = ""
	}
	if client != nil {
		if envVar, missing := client.MissingAPIKey(model, endpoint); missing {
			key, err := q.readKey(provider, envVar)
			if err != nil {
				return err
			}
			client.SetAPIKey(provider, key)
		}
	}
	thinking, err := q.askValid("Effort ("+strings.Join(llm.ThinkingEffortWords(), ", ")+" or a token budget)", cmp.Or(config.Launch.ThinkingEffort, defaultThinkingEffort), func(v string) error {
		_, err := llm.ParseThinkingEffort(v)
		return err
	})
	if err != nil {
		return err
	}
	theme, err := q.askValid("Theme ("+strings.Join(allThemeNames(), ", ")+")", themeFollowFlag(config), func(v string) error {
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
	preset, err := q.askValid("Sandbox preset (none to run unsandboxed)", currentPreset, func(v string) error {
		if v == "none" {
			return nil
		}
		return validateSandboxPresetSpec(v)
	})
	if err != nil {
		return err
	}
	defaults := setupDefaults{model: model, host: host, endpoint: endpoint, thinking: thinking, theme: theme}
	if preset != "none" {
		defaults.sandboxPreset = expandSandboxPreset(preset)
	}
	notices, err := defaults.write()
	if err != nil {
		return err
	}
	// This launch has opened nothing yet, so the defaults are its settings
	// too.
	config.Launch.Model, config.Launch.ModelHost, config.Launch.ThinkingEffort = model, host, thinking
	config.BaseURL, config.Theme = endpoint, theme
	config.NoSandbox, config.SandboxPreset = defaults.sandboxPreset == "", defaults.sandboxPreset
	for _, notice := range notices {
		fmt.Fprintln(q.out, notice)
	}
	return nil
}

// readKey asks for a provider key, without echo when the input is a
// terminal. The key is a process override like /keys; the next launch needs
// it in the environment. An empty answer ends setup the way Apply refuses a
// keyless provider.
func (q *setupQuestions) readKey(provider, envVar string) (string, error) {
	fmt.Fprintf(q.out, "Key for %s (kept for this process only; export %s for the next launch): ", provider, envVar)
	var key string
	var err error
	if file, ok := q.in.(*os.File); ok && terminalFD(int(file.Fd())) {
		var raw []byte
		raw, err = term.ReadPassword(int(file.Fd()))
		key = string(raw)
		fmt.Fprintln(q.out)
	} else {
		key, err = readLine(q.reader)
	}
	if err != nil {
		fmt.Fprintln(q.out)
		return "", errSetupDismissed
	}
	if key = strings.TrimSpace(key); key == "" {
		return "", fmt.Errorf("no API key for provider '%s': enter one, or export %s", provider, envVar)
	}
	return key, nil
}
