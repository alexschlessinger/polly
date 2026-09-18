//go:build !linux && !darwin

package sandbox

// platformPrivatePolicyRoots lists the directories the in-process policy
// treats as private where no backend wraps commands: the runtime scratch
// root, so one member's file tools cannot read a sibling's scratch. It is a
// variable so tests can model a private root.
var platformPrivatePolicyRoots = func() []string { return traversablePrivateRoots() }
