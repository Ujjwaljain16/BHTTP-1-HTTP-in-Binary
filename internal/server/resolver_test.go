package server

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPath(t *testing.T) {
	good := []string{
		"/", "/index.html", "/css/style.css", "/docs/", "/a/b/c.txt",
		"/naïve/日本語.txt", "/%2e%2e/x", "/a#b", "/a b/c", "/a..b", "/.hidden",
		"/console", "/com", "/com10", "/lpt", "/nullable.txt", "/x.con",
	}
	for _, p := range good {
		if err := checkPath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}

	bad := []string{
		"", "index.html", "a/b",
		"/..", "/../secret", "/a/../../secret", "/./index.html", "/a/./b", "/a/..",
		`/a\b`, `/a\..\b`, `\index.html`,
		"/C:/Windows/win.ini", "/index.html:stream", "/a:b", "/c:",
		"//index.html", "/a//b", "///",
		"/a\x00b", "/a\x01b", "/a\x1fb", "/a\x7fb", "/a\nb",
		"/a?b", "/a*b", `/a"b`, "/a<b", "/a>b", "/a|b",
		"/index.html.", "/index.html ", "/a./b", "/a /b", "/...",
		"/con", "/CON", "/Con.txt", "/nul", "/NUL.tar.gz", "/aux", "/prn.x",
		"/com1", "/COM9", "/lpt1", "/LPT9.txt", "/dir/con",
		"/\xff", "/\xc0\x80",
	}
	for _, p := range bad {
		if err := checkPath(p); !errors.Is(err, errBadPath) {
			t.Errorf("%q accepted", p)
		}
	}
}

func newTestResolver(t *testing.T) (*resolver, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "www")
	writeFile(t, filepath.Join(root, "index.html"), []byte("home"))
	writeFile(t, filepath.Join(root, "css", "style.css"), []byte("body{}"))
	writeFile(t, filepath.Join(root, "docs", "index.html"), []byte("docs"))
	writeFile(t, filepath.Join(root, "empty.txt"), nil)
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "secret.txt"), []byte("outside"))
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	return r, base
}

func readOpened(t *testing.T, r *resolver, path string) (string, error) {
	t.Helper()
	f, info, err := r.open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) != info.Size() {
		t.Fatalf("%s: info says %d bytes, read %d", path, info.Size(), len(b))
	}
	return string(b), nil
}

func TestOpenMapping(t *testing.T) {
	r, _ := newTestResolver(t)
	tests := []struct {
		path string
		want string
	}{
		{"/", "home"},
		{"/index.html", "home"},
		{"/css/style.css", "body{}"},
		{"/docs/", "docs"},
		{"/docs", "docs"},
		{"/empty.txt", ""},
	}
	for _, tc := range tests {
		got, err := readOpened(t, r, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
}

func TestOpenMisses(t *testing.T) {
	r, _ := newTestResolver(t)
	for _, p := range []string{
		"/missing.html", "/emptydir/", "/emptydir", "/index.html/", "/css/style.css/",
		"/index.html/deeper", "/css/missing.css", "/docs/missing", "/%2e%2e/secret.txt",
		"/" + strings.Repeat("a", 4000),
		"/" + strings.Repeat("a/", 2000),
		"/" + strings.Repeat("d", 300) + "/x.txt",
	} {
		if _, err := readOpened(t, r, p); !errors.Is(err, errNotFound) {
			t.Errorf("%.40q: got %v, want not found", p, err)
		}
	}
}

func TestOpenRejectsEscapes(t *testing.T) {
	r, _ := newTestResolver(t)
	for _, p := range []string{"/../secret.txt", "/a/../../secret.txt", `/..\secret.txt`, "/./index.html"} {
		if _, err := readOpened(t, r, p); !errors.Is(err, errBadPath) {
			t.Errorf("%q: got %v, want bad path", p, err)
		}
	}
}

func TestOpenRefusesLinksOutOfTheRoot(t *testing.T) {
	r, base := newTestResolver(t)
	outside := filepath.Join(base, "outside")
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("outside"))
	writeFile(t, filepath.Join(outside, "index.html"), []byte("outside index"))

	link := filepath.Join(r.root, "escape")
	made := false
	if err := os.Symlink(outside, link); err == nil {
		made = true
	} else if runtime.GOOS == "windows" {
		// Junctions need no special privilege, unlike symlinks.
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err == nil {
			made = true
		} else {
			t.Logf("could not create a junction: %v: %s", err, out)
		}
	}
	if !made {
		t.Skip("cannot create a symlink or junction here")
	}

	for _, p := range []string{"/escape/secret.txt", "/escape/", "/escape"} {
		if got, err := readOpened(t, r, p); !errors.Is(err, errNotFound) {
			t.Errorf("%q: got %q, %v; want not found", p, got, err)
		}
	}
}

func TestOpenRefusesFileLinkOutOfTheRoot(t *testing.T) {
	r, base := newTestResolver(t)
	link := filepath.Join(r.root, "leak.txt")
	if err := os.Symlink(filepath.Join(base, "secret.txt"), link); err != nil {
		t.Skip("cannot create a symlink here")
	}
	if got, err := readOpened(t, r, "/leak.txt"); !errors.Is(err, errNotFound) {
		t.Fatalf("got %q, %v; want not found", got, err)
	}
}

func TestOpenAllowsLinksThatStayInside(t *testing.T) {
	r, _ := newTestResolver(t)
	if err := os.Symlink(filepath.Join(r.root, "docs"), filepath.Join(r.root, "alias")); err != nil {
		t.Skip("cannot create a symlink here")
	}
	if got, err := readOpened(t, r, "/alias/"); err != nil || got != "docs" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestOpenIsCaseInsensitiveOnlyWhereTheFilesystemIs(t *testing.T) {
	r, _ := newTestResolver(t)
	got, err := readOpened(t, r, "/INDEX.HTML")
	if runtime.GOOS == "windows" {
		if err != nil || got != "home" {
			t.Fatalf("got %q, %v", got, err)
		}
		return
	}
	if !errors.Is(err, errNotFound) && err != nil {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestNewResolverRejectsNonDirectories(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	writeFile(t, file, []byte("x"))
	if _, err := newResolver(file); err == nil {
		t.Error("a file was accepted as a document root")
	}
	if _, err := newResolver(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing directory was accepted as a document root")
	}
}
