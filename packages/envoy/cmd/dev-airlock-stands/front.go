// packages/envoy/cmd/dev-airlock-stands/front.go
// The `front` subcommand: one TCP address in front of several, so a client reaches whichever
// process answers. The airlock's local stack (scripts/dev-airlock.sh) runs two brokers, and a
// check that drives "two broker processes behind one address" needs one address for their API
// ports and one for their proxy ports. socat and nginx are not in a Legion issue pod, and nginx
// would not do for the proxy port anyway: it passes no CONNECT. This is a few lines of Go
// instead.
//
// It is a byte-for-byte TCP relay, so it carries HTTP/1.1, a CONNECT tunnel and the TLS inside
// one alike: nothing here reads a request. A connection is handed to one backend and stays
// there for its life, chosen round-robin - the next backend in the list for each new connection.
// Round-robin per connection is enough for what the checks ask of it: they ask that both
// processes serve, not that the load is even, and the two brokers share one database, so either
// answers any call.
//
// The listening socket is bound before anything else and its address printed, so a caller may
// name the front in a broker's own BROKER_PUBLIC_URL and BROKER_AIRLOCK_PROXY_URL before that
// broker exists. The backends are therefore allowed to arrive later: --backends-file names a
// file of one address per line, read again whenever it has changed, so the stack writes the two
// brokers' bound addresses into it once they report them. A connection that arrives while no
// backend is known is closed, with one line saying so.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// frontReport is the JSON one front writes to standard output once it is bound.
type frontReport struct {
	// Address is the host:port it listens on, with the port the system picked resolved.
	Address string `json:"address"`
	// URL is that address as a caller names it: the value BROKER_PUBLIC_URL or
	// BROKER_AIRLOCK_PROXY_URL takes.
	URL string `json:"url"`
}

// runFront is `dev-airlock-stands front`.
func runFront(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-airlock-stands front", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "bind `<host:port>`, port 0 for one the system picks")
	file := fs.String("backends-file", "", "read the backends from `<path>`, one host:port a line, again whenever the file changes")
	var static addressFlags
	fs.Var(&static, "backend", "send connections to `<host:port>` (repeatable), beside any --backends-file names")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsageError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dev-airlock-stands front: unexpected argument %q\n", fs.Arg(0))
		return exitUsageError
	}
	if len(static.values) == 0 && *file == "" {
		fmt.Fprintln(stderr, "dev-airlock-stands front: name at least one --backend or a --backends-file")
		return exitUsageError
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands front: %v\n", err)
		return 1
	}
	defer func() { _ = listener.Close() }()
	addr := listener.Addr().String()
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(frontReport{Address: addr, URL: "http://" + addr}); err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands front: write the report: %v\n", err)
		return 1
	}
	if flusher, ok := stdout.(interface{ Sync() error }); ok {
		_ = flusher.Sync()
	}
	fmt.Fprintf(stderr, "dev-airlock-stands: front listening addr=%s\n", addr)
	backends := &backendList{static: static.values, file: *file}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			fmt.Fprintf(stderr, "dev-airlock-stands front: accept: %v\n", err)
			return 1
		}
		backend, err := backends.next()
		if err != nil {
			fmt.Fprintf(stderr, "dev-airlock-stands front: %v; closing the connection\n", err)
			_ = client.Close()
			continue
		}
		go relay(client, backend, stderr)
	}
}

// addressFlags is a repeated address flag: --backend.
type addressFlags struct {
	values []string
}

func (f *addressFlags) String() string { return "" }

func (f *addressFlags) Set(value string) error {
	if _, _, err := net.SplitHostPort(value); err != nil {
		return fmt.Errorf("want <host:port>: %w", err)
	}
	f.values = append(f.values, value)
	return nil
}

// backendList is the addresses one front sends connections to and whose turn it is. The file's
// addresses are read again whenever its size or modification time has changed, so a stack that
// binds its backends after the front may write them in.
type backendList struct {
	mu       sync.Mutex
	static   []string
	file     string
	fromFile []string
	// stamp is the file as it was when fromFile was read: its size and modification time, which
	// is enough to find a rewrite of a file this one process is the only reader of.
	stamp string
	turn  int
}

// next is the backend this connection goes to: the one after the last, over the static
// addresses and the file's, in that order.
func (b *backendList) next() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.reread(); err != nil {
		return "", err
	}
	all := append(append([]string{}, b.static...), b.fromFile...)
	if len(all) == 0 {
		return "", fmt.Errorf("no backend is known yet (%s names none)", b.file)
	}
	backend := all[b.turn%len(all)]
	b.turn++
	return backend, nil
}

// reread reads the backends file again when it has changed. A file that is not there yet is no
// error: the stack writes it once its brokers have bound.
func (b *backendList) reread() error {
	if b.file == "" {
		return nil
	}
	info, err := os.Stat(b.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b.fromFile, b.stamp = nil, ""
			return nil
		}
		return fmt.Errorf("read the backends file %s: %w", b.file, err)
	}
	stamp := fmt.Sprintf("%d@%d", info.Size(), info.ModTime().UnixNano())
	if stamp == b.stamp {
		return nil
	}
	data, err := os.ReadFile(b.file)
	if err != nil {
		return fmt.Errorf("read the backends file %s: %w", b.file, err)
	}
	var addresses []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, _, err := net.SplitHostPort(line); err != nil {
			return fmt.Errorf("the backends file %s holds %q, which is no host:port", b.file, line)
		}
		addresses = append(addresses, line)
	}
	b.fromFile, b.stamp = addresses, stamp
	return nil
}

// relay joins client to one backend and copies bytes until either side is done. Each direction
// is closed for writing as its source ends, so a backend reading to end of file - a CONNECT
// tunnel's far side, an HTTP body - sees that end.
func relay(client net.Conn, backend string, stderr io.Writer) {
	defer func() { _ = client.Close() }()
	server, err := net.DialTimeout("tcp", backend, 10*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands front: dial %s: %v\n", backend, err)
		return
	}
	defer func() { _ = server.Close() }()
	var wg sync.WaitGroup
	wg.Add(2)
	go copyHalf(&wg, server, client)
	go copyHalf(&wg, client, server)
	wg.Wait()
}

// copyHalf copies one direction and half-closes the far side's write half when it ends.
func copyHalf(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if half, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
		return
	}
	_ = dst.Close()
}
