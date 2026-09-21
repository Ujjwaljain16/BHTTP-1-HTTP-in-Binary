package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bhttp/internal/frame"
)

// The conformance folder is what other people test their clients against, so
// its checksums and the way bserve frames each file are checked here. If this
// test passes, the tables in the interoperability guide are true.
func TestConformanceFixtureMatchesItsChecksumsAndIsFramedAsDocumented(t *testing.T) {
	sums, err := os.Open(filepath.Join("..", "..", "conformance", "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	defer sums.Close()

	srv, err := New(Config{Root: filepath.Join("..", "..", "conformance", "www")})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()
	h := &harness{srv: srv, lis: &countingListener{Listener: l}}
	c := dial(t, h)

	checked := 0
	scan := bufio.NewScanner(sums)
	for scan.Scan() {
		want, name, ok := strings.Cut(scan.Text(), "  ")
		if !ok {
			t.Fatalf("malformed checksum line %q", scan.Text())
		}
		rel := strings.TrimPrefix(name, "www/")

		onDisk, err := os.ReadFile(filepath.Join("..", "..", "conformance", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if got := sha256Hex(onDisk); got != want {
			t.Errorf("%s: the file on disk hashes to %s, SHA256SUMS says %s", rel, got, want)
		}

		r := c.get("/" + rel)
		if r.status != 200 {
			t.Errorf("%s: status %d", rel, r.status)
			continue
		}
		if got := sha256Hex(r.body); got != want {
			t.Errorf("%s: the served body hashes to %s, want %s", rel, got, want)
		}

		// One RESPONSE, then ceil(size/16384) DATA frames, or none for an empty file.
		wantData := (len(onDisk) + frame.MaxPayload - 1) / frame.MaxPayload
		if got := len(r.frames) - 1; got != wantData {
			t.Errorf("%s: %d DATA frames, want %d", rel, got, wantData)
		}
		checked++
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if checked < 10 {
		t.Fatalf("only %d fixture files were checked", checked)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
