//go:build !windows

package server

import (
	"os"
	"testing"
)

// lockFile removes all permissions from the file. Privileged users ignore
// permissions, so the test is skipped for them rather than passing vacuously.
func lockFile(t *testing.T, path string) (unlock func()) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	unlock = func() { os.Chmod(path, info.Mode()) }
	t.Cleanup(unlock)

	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("permissions are not enforced for this user")
	}
	return unlock
}
