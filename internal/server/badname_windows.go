package server

import (
	"errors"
	"syscall"
)

// The syscall package does not export these, so the raw Win32 codes are used:
// ERROR_INVALID_NAME, ERROR_BAD_PATHNAME and ERROR_FILENAME_EXCED_RANGE.
const (
	errInvalidName syscall.Errno = 123
	errBadPathname syscall.Errno = 161
	errNameTooLong syscall.Errno = 206
)

// A name the filesystem cannot even represent, such as an over-long segment,
// names nothing that exists, so it is a plain miss rather than a server fault.
func isUnrepresentableName(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == errInvalidName || errno == errBadPathname || errno == errNameTooLong
}
