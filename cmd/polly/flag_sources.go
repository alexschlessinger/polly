package main

import (
	"slices"
	"sync"

	"github.com/urfave/cli/v3"
)

// Flags have three tiers: a value given on the command line, a default from
// the environment or from ~/.pollytool/config, and the built-in default.
// Only the first tier overrides what a session has stored; the other two
// seed new sessions. urfave folds the first two into IsSet, but consults a
// flag's Sources only when the command line left it unset, so a source that
// records being used tells the tiers apart again (flagGiven).

// defaultSourcesUsed holds the environment keys whose value came from a
// default source in the current parse. The flag table resets it when built.
var defaultSourcesUsed = struct {
	sync.Mutex
	keys map[string]bool
}{keys: map[string]bool{}}

func resetDefaultSources() {
	defaultSourcesUsed.Lock()
	defaultSourcesUsed.keys = map[string]bool{}
	defaultSourcesUsed.Unlock()
}

func markDefaultUsed(key string) {
	defaultSourcesUsed.Lock()
	defaultSourcesUsed.keys[key] = true
	defaultSourcesUsed.Unlock()
}

func defaultUsed(key string) bool {
	defaultSourcesUsed.Lock()
	defer defaultSourcesUsed.Unlock()
	return defaultSourcesUsed.keys[key]
}

// envDefaultSource is cli.EnvVar with a memory of having been used: Lookup
// records the key, and String, GoString, Key, and IsFromEnv delegate to the
// real cli.EnvVar through the embedded interfaces, so help text cannot drift
// from urfave's own.
type envDefaultSource struct {
	cli.ValueSource
	cli.EnvValueSource
}

func newEnvDefaultSource(key string) *envDefaultSource {
	env := cli.EnvVar(key)
	return &envDefaultSource{ValueSource: env, EnvValueSource: env.(cli.EnvValueSource)}
}

func (e *envDefaultSource) Lookup() (string, bool) {
	value, ok := e.ValueSource.Lookup()
	if ok {
		markDefaultUsed(e.EnvValueSource.Key())
	}
	return value, ok
}

// fileDefaultSource reads the same variable from ~/.pollytool/config.
type fileDefaultSource struct{ key string }

func (f *fileDefaultSource) Lookup() (string, bool) {
	value, ok := userConfigValue(f.key)
	if ok {
		markDefaultUsed(f.key)
	}
	return value, ok
}

func (f *fileDefaultSource) String() string   { return userConfigDisplayPath + " \"" + f.key + "\"" }
func (f *fileDefaultSource) GoString() string { return "&fileDefaultSource{key:\"" + f.key + "\"}" }

// envDefault is the Sources value for a flag whose variable is a default
// rather than an override: the environment first, then the file, for each key
// in turn. Keys after the first are former spellings, still read so an
// existing environment or configuration file keeps working.
func envDefault(keys ...string) cli.ValueSourceChain {
	chain := cli.ValueSourceChain{}
	for _, key := range keys {
		chain.Chain = append(chain.Chain, newEnvDefaultSource(key), &fileDefaultSource{key: key})
	}
	return chain
}

// flagGiven reports whether the flag was set on the command line, as
// opposed to defaulted from the environment or file or left at its built-in
// value.
func flagGiven(cmd *cli.Command, name string) bool {
	if !cmd.IsSet(name) {
		return false
	}
	for _, flag := range cmd.Root().Flags {
		if !slices.Contains(flag.Names(), name) {
			continue
		}
		if doc, ok := flag.(cli.DocGenerationFlag); ok {
			return !slices.ContainsFunc(doc.GetEnvVars(), defaultUsed)
		}
		return true
	}
	return true
}
