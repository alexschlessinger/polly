//go:build !unix

package sandbox

import "errors"

// EnterHelperMode has no effect where the container sandbox is unavailable.
func EnterHelperMode() {}

// ErrNotHelper reports a container sandbox requested outside helper mode.
var ErrNotHelper = errors.New("container sandbox is only constructible in helper mode")

// NewContainerSandboxFactory is unavailable: the helper runs on Unix only.
func NewContainerSandboxFactory([]string) (func(Config) (Sandbox, error), error) {
	return nil, errors.New("container sandbox requires a Unix helper")
}
