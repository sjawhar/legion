// Command natsd runs one nats-server in this process, in the foreground, until it is signalled.
// It is the bus of packages/envoy/scripts/e2e-local.sh, which builds it beside the listener it
// builds: an agent's machine has no container runtime, so the script cannot run the NATS image.
//
// It is a test fixture, not a product. Nothing deployed runs it: it listens on loopback only, has
// no users, keeps its JetStream store wherever the caller says and never cleans it up, and lives
// under internal/testnats so nothing outside this module can import it. The server the tests run
// is the one testnats starts (nats.go); this is the same server for a shell script's sake.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// readyTimeout bounds the wait for the server to accept connections, so a port another process
// holds ends this command instead of leaving its caller waiting.
const readyTimeout = 10 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "natsd: %v\n", err)
		os.Exit(1)
	}
}

// run parses args as nats-server parses its own command line (-p for the client port, -store_dir
// for the JetStream store), starts the server with JetStream on 127.0.0.1 and its log on stdout,
// and returns once a signal has shut it down.
func run(args []string) error {
	flags := flag.NewFlagSet("natsd", flag.ContinueOnError)
	help := func() { flags.PrintDefaults() }
	opts, err := natsserver.ConfigureOptions(flags, args, help, help, help)
	if err != nil {
		return err
	}
	if opts == nil {
		// -v, -h or -help_tls: ConfigureOptions has printed what was asked for.
		return nil
	}
	opts.Host = "127.0.0.1"
	opts.JetStream = true
	opts.NoSigs = true
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return err
	}
	srv.ConfigureLogger()
	go srv.Start()
	if !srv.ReadyForConnections(readyTimeout) {
		srv.Shutdown()
		srv.WaitForShutdown()
		return fmt.Errorf("no connections accepted on %s:%d within %s; is the port taken?", opts.Host, opts.Port, readyTimeout)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	srv.Shutdown()
	srv.WaitForShutdown()
	return nil
}
