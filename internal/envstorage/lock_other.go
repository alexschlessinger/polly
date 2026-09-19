//go:build !unix

package envstorage

import (
	"errors"
	"io/fs"
)

var errUnsupported = errors.New("managed sandbox environments require a native Unix sandbox")

func owned(fs.FileInfo) error     { return errUnsupported }
func identity(fs.FileInfo) string { return "" }
func Lock(string) (func(), error) { return nil, errUnsupported }

type Lease struct{}

func OpenLease(string) (*Lease, error)      { return nil, errUnsupported }
func (*Lease) Exclusive(func() error) error { return errUnsupported }
func (*Lease) Close() error                 { return nil }
