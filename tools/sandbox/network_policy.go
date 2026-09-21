package sandbox

import (
	"fmt"
	"net"
)

// NetworkAllowed reports whether the policy lets an in-process fetch reach
// host, judged as the OS backends judge a sandboxed process: nothing without
// allowNetwork, and under denyDNS no name resolution, so only an IP literal
// is reachable. It is the network counterpart of ReadAllowed for tools that
// fetch in-process instead of through a wrapped command.
func NetworkAllowed(cfg Config, host string) error {
	if !cfg.AllowNetwork {
		return fmt.Errorf("host %q is blocked: the sandbox policy denies network access", host)
	}
	if cfg.DenyDNS && net.ParseIP(host) == nil {
		return fmt.Errorf("host %q is blocked: the sandbox policy denies DNS, so only an IP literal is reachable", host)
	}
	return nil
}
