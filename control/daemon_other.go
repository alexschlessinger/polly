//go:build !darwin && !linux

package control

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("the control backend needs Unix domain sockets: it runs on macOS and Linux")

// UserSocketDir is not known on this system.
func UserSocketDir() (string, error) { return "", errUnsupported }

// SocketPath is the socket of build key's daemon in dir.
func SocketPath(dir, buildKey string) string { return "" }

func dialExisting(context.Context, Bootstrap) (*Client, error) { return nil, errUnsupported }

func connectOrStart(context.Context, Bootstrap) (*Client, bool, error) {
	return nil, false, errUnsupported
}
