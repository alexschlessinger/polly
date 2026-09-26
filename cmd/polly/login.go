package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/codex"
)

// A sign-in is the one credential polly keeps on disk. API keys never
// persist (see userconfig.go): they live in the environment, which is
// where the user keeps them. A sign-in's refresh token is minted for polly
// and replaced on every use, so there is nowhere else for it to live; it
// sits beside the configuration file, readable by its owner alone, and
// inside the sandbox's private root so sandboxed tools cannot read it.
const authFileName = "auth.json"

// codexProvider is the provider served on a sign-in today.
const codexProvider = "codex"

// authStorePath is ~/.pollytool/auth.json.
func authStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, userConfigDirName, authFileName), nil
}

// openAuthStore opens the sign-in store. Every store on the path reads the
// same file, so the router's and a /login's agree.
func openAuthStore() (*codex.Store, error) {
	path, err := authStorePath()
	if err != nil {
		return nil, err
	}
	return codex.NewStore(path), nil
}

// newLLMRouter builds the provider router on the environment's keys and
// the sign-ins on disk.
func newLLMRouter() *llm.MultiPass {
	var opts []llm.ClientOption
	if store, err := openAuthStore(); err == nil {
		opts = append(opts, llm.WithLogin(codexProvider, store))
	} else {
		slog.Debug("auth_store_unavailable", "error", err)
	}
	return llm.NewMultiPass(loadAPIKeys(), opts...)
}

// loginProvider names the provider a sign-in command is for: codex when
// none is given, and only a provider served on a sign-in.
func loginProvider(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return codexProvider, nil
	}
	if !slices.Contains(validModelProviders, name) {
		return "", fmt.Errorf("unknown provider '%s'. Valid providers: %s", name, strings.Join(validModelProviders, ", "))
	}
	if !llm.ProviderRequiresLogin(name + "/model") {
		return "", fmt.Errorf("provider '%s' takes an API key, not a sign-in: export %s, or set one with /keys", name, llm.ProviderKeyEnvVar(name))
	}
	return name, nil
}

// signedInNotice reports a sign-in without revealing anything but who.
func signedInNotice(provider string, acct llm.Account) string {
	who := acct.Email
	if who == "" {
		who = acct.ID
	}
	line := fmt.Sprintf("signed in to %s as %s", provider, who)
	if acct.Plan != "" {
		line += " (" + acct.Plan + " plan)"
	}
	return line
}
