package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"bhttp/internal/frame"
	"bhttp/internal/protocol"
	"bhttp/internal/server"
)

var binPath string

// The tests run the real executable so exit codes and the raw bytes on stdout
// are checked exactly as a shell would see them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bcurl-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "bcurl.exe")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type outcome struct {
	stdout []byte
	stderr string
	code   int
}

func runBcurl(t *testing.T, args ...string) outcome {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return outcome{stdout.Bytes(), stderr.String(), code}
}

func allBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func startServer(t *testing.T) (addr string, binary []byte) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	binary = append(allBytes(300000), '\r', '\n', 0, '\r', '\n')
	for name, data := range map[string][]byte{
		"index.html": []byte("<h1>hello</h1>\n"),
		"binary.bin": binary,
		"empty.txt":  nil,
	} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := server.New(server.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String(), binary
}

func TestBadUsage(t *testing.T) {
	tests := map[string][]string{
		"no arguments":           {},
		"two targets":            {"localhost:1/a", "localhost:1/b"},
		"unknown flag":           {"-x", "localhost:1/"},
		"flag only":              {"-v"},
		"target without port":    {"localhost/index.html"},
		"empty host":             {":9000/x"},
		"empty port":             {"localhost:/x"},
		"empty target":           {""},
		"header without colon":   {"-H", "nocolon", "localhost:1/"},
		"header with bad name":   {"-H", "bad name: v", "localhost:1/"},
		"header with empty name": {"-H", ": v", "localhost:1/"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			got := runBcurl(t, args...)
			if got.code != 2 {
				t.Errorf("exit code %d, want 2 (stderr: %q)", got.code, got.stderr)
			}
			if len(got.stdout) != 0 || got.stderr == "" {
				t.Errorf("stdout %q, stderr %q; want an explanation on stderr only", got.stdout, got.stderr)
			}
		})
	}
}

func TestUnreachableServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	got := runBcurl(t, addr+"/")
	if got.code != 3 || len(got.stdout) != 0 || !strings.Contains(got.stderr, "bcurl:") {
		t.Errorf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
}

func TestSuccessWritesTheBodyAndExitsZero(t *testing.T) {
	addr, _ := startServer(t)
	for _, target := range []string{addr + "/index.html", addr + "/", addr} {
		got := runBcurl(t, target)
		if got.code != 0 || string(got.stdout) != "<h1>hello</h1>\n" || got.stderr != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", target, got.code, got.stdout, got.stderr)
		}
	}
	if got := runBcurl(t, addr+"/empty.txt"); got.code != 0 || len(got.stdout) != 0 {
		t.Errorf("empty file: exit %d, stdout %d bytes", got.code, len(got.stdout))
	}
}

func TestBinaryBodyReachesStdoutUntouched(t *testing.T) {
	addr, want := startServer(t)
	got := runBcurl(t, addr+"/binary.bin")
	if got.code != 0 || !bytes.Equal(got.stdout, want) {
		t.Fatalf("exit %d, stdout %d bytes (want %d) equal=%v", got.code, len(got.stdout), len(want), bytes.Equal(got.stdout, want))
	}
}

func TestErrorStatusesGiveNonZeroExit(t *testing.T) {
	addr, _ := startServer(t)
	tests := map[string]string{
		"404": addr + "/missing.html",
		"400": addr + "/a:b",
	}
	for status, target := range tests {
		got := runBcurl(t, target)
		if got.code != 1 || !strings.Contains(got.stderr, status) {
			t.Errorf("%s: exit %d, stderr %q", status, got.code, got.stderr)
		}
	}
}

func TestVerboseDumpsTheBytesThatCrossedTheConnection(t *testing.T) {
	addr, _ := startServer(t)
	got := runBcurl(t, "-v", addr+"/index.html")
	if got.code != 0 || string(got.stdout) != "<h1>hello</h1>\n" {
		t.Fatalf("exit %d, stdout %q: verbose must not disturb the body", got.code, got.stdout)
	}

	// Built independently of the dump, so the test fails if the tap ever
	// stops showing what was actually written.
	payload, err := protocol.Request{Method: protocol.MethodGet, Path: "/index.html"}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := frame.Encode(frame.Frame{Type: frame.TypeRequest, Flags: frame.FlagEndStream, StreamID: 1, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--> frame 1  REQUEST  stream=1  flags=END_STREAM",
		fmt.Sprintf("header   % X", wire[:frame.HeaderSize]),
		"<-- frame 2  RESPONSE  stream=1",
		"<-- frame 3  DATA  stream=1  flags=END_STREAM  length=15",
		"3C 68 31 3E", // "<h1>" inside the body
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, got.stderr)
		}
	}

	quiet := runBcurl(t, addr+"/index.html")
	if quiet.stderr != "" {
		t.Errorf("without -v nothing should reach stderr, got %q", quiet.stderr)
	}
}

func TestRequestHeadersAreSent(t *testing.T) {
	addr, _ := startServer(t)
	got := runBcurl(t, "-v", "-H", "X-Demo: hello", "-H", "content-type:text/plain", addr+"/index.html")
	if got.code != 0 || string(got.stdout) != "<h1>hello</h1>\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	// A dump wraps every sixteen bytes, so compare the bytes themselves.
	sent := hexBytes(got.stderr)
	for name, want := range map[string]string{
		"x-demo (lowercased, sent by name)": "782D64656D6F",
		"hello":                             "68656C6C6F",
		"text/plain (sent by table id)":     "746578742F706C61696E",
	} {
		if !strings.Contains(sent, want) {
			t.Errorf("the request bytes do not contain %s:\n%s", name, got.stderr)
		}
	}
}

// hexBytes joins the hex columns of every dump row into one continuous string.
func hexBytes(dump string) string {
	var b strings.Builder
	for _, line := range strings.Split(dump, "\n") {
		if m := dumpRow.FindStringSubmatch(line); m != nil {
			b.WriteString(strings.ReplaceAll(m[1], " ", ""))
		}
	}
	return b.String()
}

var dumpRow = regexp.MustCompile(`^\s+[0-9A-F]{8}  ([0-9A-F ]+?)\s+\|`)

func TestVerboseOnAnErrorResponse(t *testing.T) {
	addr, _ := startServer(t)
	got := runBcurl(t, "-v", addr+"/nope")
	if got.code != 1 || !strings.Contains(got.stderr, "RESPONSE  stream=1  flags=END_STREAM") {
		t.Errorf("exit %d, stderr:\n%s", got.code, got.stderr)
	}
}

func TestServerThatIsNotSpeakingTheProtocol(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi"))
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 4096)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	got := runBcurl(t, l.Addr().String()+"/")
	if got.code != 3 || len(got.stdout) != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in, addr, path string
		ok             bool
	}{
		{"localhost:9000/index.html", "localhost:9000", "/index.html", true},
		{"localhost:9000", "localhost:9000", "/", true},
		{"localhost:9000/", "localhost:9000", "/", true},
		{"127.0.0.1:80/a/b/c.txt", "127.0.0.1:80", "/a/b/c.txt", true},
		{"[::1]:9000/x", "[::1]:9000", "/x", true},
		{"host:1/a b", "host:1", "/a b", true},
		{"host:1//double", "host:1", "//double", true},
		{"localhost/x", "", "", false},
		{"localhost", "", "", false},
		{":9000/x", "", "", false},
		{"", "", "", false},
		{"/x", "", "", false},
	}
	for _, tc := range tests {
		addr, path, err := parseTarget(tc.in)
		if (err == nil) != tc.ok || addr != tc.addr || path != tc.path {
			t.Errorf("%q: got %q %q %v", tc.in, addr, path, err)
		}
	}
}
