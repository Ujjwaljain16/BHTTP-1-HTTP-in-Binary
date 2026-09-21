package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

var (
	// errBadPath means the requested path is not something we are willing to
	// look at, whatever is on disk. It maps to a 400.
	errBadPath = errors.New("path not allowed")

	// errNotFound covers everything that should look like an ordinary miss,
	// including paths that lead outside the root, so a probe learns nothing.
	errNotFound = errors.New("not found")
)

// resolver maps request paths onto files under one document root.
type resolver struct {
	root string // absolute, symlinks resolved
}

func newResolver(root string) (*resolver, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New(root + " is not a directory")
	}
	return &resolver{root: real}, nil
}

// checkPath rejects anything that could mean something different to the
// filesystem than it does to us. The same rules apply on every platform so a
// path behaves identically wherever the server runs; several of them exist
// only because Windows treats those forms specially.
func checkPath(p string) error {
	if !strings.HasPrefix(p, "/") || !utf8.ValidString(p) || strings.Contains(p, "//") {
		return errBadPath
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7F || strings.IndexByte(`\:*?"<>|`, c) >= 0 {
			return errBadPath
		}
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" {
			continue // the root itself, or a trailing slash
		}
		if seg == "." || seg == ".." {
			return errBadPath
		}
		// Windows silently drops trailing dots and spaces, so "a." and "a"
		// would be the same file.
		if last := seg[len(seg)-1]; last == '.' || last == ' ' {
			return errBadPath
		}
		if reservedDeviceName(seg) {
			return errBadPath
		}
	}
	return nil
}

// Windows treats these as devices no matter the extension, so "con.txt" opens
// the console rather than a file.
func reservedDeviceName(seg string) bool {
	stem := seg
	if i := strings.IndexByte(seg, '.'); i >= 0 {
		stem = seg[:i]
	}
	stem = strings.ToUpper(stem)
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		return stem[3] >= '1' && stem[3] <= '9'
	}
	return false
}

// open finds the regular file a path refers to and returns it opened, along
// with its info taken from the open handle so the size we announce is the
// size of the file we will actually read.
func (r *resolver) open(urlPath string) (*os.File, fs.FileInfo, error) {
	if err := checkPath(urlPath); err != nil {
		return nil, nil, err
	}

	trailingSlash := strings.HasSuffix(urlPath, "/")
	rel := filepath.FromSlash(strings.Trim(urlPath, "/"))

	target, err := r.within(filepath.Join(r.root, rel))
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, nil, missingOr(err)
	}

	if info.IsDir() {
		target, err = r.within(filepath.Join(target, "index.html"))
		if err != nil {
			return nil, nil, err
		}
	} else if trailingSlash {
		return nil, nil, errNotFound
	}

	f, err := os.Open(target)
	if err != nil {
		return nil, nil, missingOr(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, errNotFound
	}
	return f, fi, nil
}

// within resolves links and confirms the result still lives under the root.
// String checks alone cannot see a symlink or junction that points outward.
func (r *resolver) within(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", missingOr(err)
	}
	rel, err := filepath.Rel(r.root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errNotFound
	}
	return resolved, nil
}

// missingOr turns "does not exist" style failures into errNotFound and leaves
// real problems, such as permission errors, for the caller to report as such.
func missingOr(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || isUnrepresentableName(err) {
		return errNotFound
	}
	return err
}
