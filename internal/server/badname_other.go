//go:build !windows

package server

import (
	"errors"
	"syscall"
)

// A name the filesystem cannot even represent, such as an over-long segment,
// names nothing that exists, so it is a plain miss rather than a server fault.
func isUnrepresentableName(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG)
}
