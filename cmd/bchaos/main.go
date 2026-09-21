// Command bchaos sits between a client and a server and makes the server's
// answers awkward in ways a correct client must tolerate: delivered in tiny
// pieces, and padded with frames of types it has never heard of.
//
//	bchaos -target host:port [-listen addr] [-chunk n] [-delay d] [-noise] [-seed n]
//
// Point the client under test at the -listen address instead of the server.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"bhttp/internal/chaos"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bchaos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bchaos -target host:port [-listen addr] [-chunk n] [-delay d] [-noise] [-seed n]")
		fs.PrintDefaults()
	}
	target := fs.String("target", "", "server to relay to, host:port (required)")
	listen := fs.String("listen", "127.0.0.1:9100", "address the client under test connects to")
	chunk := fs.Int("chunk", 0, "write at most this many bytes at a time toward the client, in random pieces of 1 to n (0 = off, 1 = one byte at a time)")
	delay := fs.Duration("delay", 0, "pause after each piece, e.g. 1ms (keeps tiny pieces from being merged again)")
	noise := fs.Bool("noise", false, "insert frames of unknown types around every server frame")
	seed := fs.Int64("seed", 1, "seed for the randomness, so a failing run can be repeated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *target == "" || *chunk < 0 || *delay < 0 {
		fs.Usage()
		return 2
	}
	if _, _, err := net.SplitHostPort(*target); err != nil {
		fmt.Fprintf(stderr, "bchaos: -target must look like host:port: %v\n", err)
		return 2
	}

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "bchaos: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "listening on %s, relaying to %s (chunk=%d delay=%v noise=%v seed=%d)\n",
		l.Addr(), *target, *chunk, *delay, *noise, *seed)

	logger := log.New(stderr, "", log.LstdFlags)
	err = chaos.Serve(l, *target, chaos.Options{
		Chunk: *chunk, Delay: *delay, Noise: *noise, Seed: *seed,
		Logf: logger.Printf,
	})
	if err != nil {
		fmt.Fprintf(stderr, "bchaos: %v\n", err)
		return 1
	}
	return 0
}
