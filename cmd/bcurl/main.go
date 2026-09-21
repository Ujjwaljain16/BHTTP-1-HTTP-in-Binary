// Command bcurl fetches one path from a server speaking the binary protocol
// and writes the body to standard output.
//
//	bcurl [-v] [-H "name: value"]... <host>:<port>[/path]
//
// Exit status: 0 on success, 1 if the server answered 4xx or 5xx, 2 for bad
// usage, 3 if the exchange itself failed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"bhttp/internal/client"
	"bhttp/internal/protocol"
	"bhttp/internal/wire"
)

const dialTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bcurl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, `usage: bcurl [-v] [-H "name: value"]... <host>:<port>[/path]`) }
	verbose := fs.Bool("v", false, "hexdump every frame that crosses the connection (to stderr)")
	var headers headerFlags
	fs.Var(&headers, "H", `send a request header, e.g. -H "x-demo: hello" (repeatable)`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	addr, path, err := parseTarget(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
		return 2
	}

	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
		return 3
	}
	if *verbose {
		tap := wire.NewTap(stderr)
		defer tap.Flush()
		conn = tap.Wrap(conn)
	}

	c := client.New(conn)
	defer c.Close()

	res, err := c.Get(path, stdout, headers...)
	if err != nil {
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
		return 3
	}
	if res.Status >= 400 {
		fmt.Fprintf(stderr, "bcurl: server answered %d\n", res.Status)
		return 1
	}
	return 0
}

// headerFlags collects repeated -H options. Names are lowercased because the
// protocol only carries lowercase names, and header names are case-insensitive
// wherever people are used to writing them.
type headerFlags []protocol.Header

func (h *headerFlags) String() string { return "" }

func (h *headerFlags) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	name = strings.ToLower(strings.TrimSpace(name))
	if !ok || name == "" {
		return fmt.Errorf("header %q must look like name: value", v)
	}
	hdr := protocol.NewHeader(name, strings.TrimLeft(value, " \t"))
	if _, err := (protocol.Request{Method: protocol.MethodGet, Path: "/", Headers: []protocol.Header{hdr}}).Encode(); err != nil {
		return fmt.Errorf("header %q: %w", v, err)
	}
	*h = append(*h, hdr)
	return nil
}

// parseTarget splits "host:port/path" into a dial address and a request path.
// The port is mandatory because the protocol has no well-known one.
func parseTarget(target string) (addr, path string, err error) {
	authority, path := target, "/"
	if i := strings.IndexByte(target, '/'); i >= 0 {
		authority, path = target[:i], target[i:]
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host == "" || port == "" {
		return "", "", errors.New("target must look like host:port/path")
	}
	return net.JoinHostPort(host, port), path, nil
}
