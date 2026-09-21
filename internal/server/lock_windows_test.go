package server

import (
	"syscall"
	"testing"
)

// lockFile makes a file impossible to open for reading, in a way that is
// neither "missing" nor "outside the root", so the server has to treat it as
// its own failure. The returned function releases the lock.
func lockFile(t *testing.T, path string) (unlock func()) {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// A share mode of zero refuses every other opener, readers included.
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("locking %s: %v", path, err)
	}
	var once bool
	unlock = func() {
		if !once {
			once = true
			syscall.CloseHandle(h)
		}
	}
	t.Cleanup(unlock)
	return unlock
}
