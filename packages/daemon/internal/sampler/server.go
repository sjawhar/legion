package sampler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// This file is the sampler's process: the HTTP server that hands a collector out of the registry,
// the collector of whichever mode the command named, and the memory guard that ends the sampler
// before the pod's kernel does. `legion sampler` is this and the flags that build it
// (cmd/legion/sampler.go).
//
// It is built in the shape of internal/repocache's own server: the options are checked before
// anything is bound, the listener is taken before the collector starts, the server serves on a
// goroutine into a channel, a deferred Shutdown is bounded, and one select joins the context, the
// server and the collector. Nothing here reads a path of its own either — the root every read is
// given is the collector's — so a test drives the whole process over a directory it wrote.

// Port is the port the sampler binds: the issue pod's `roles` container declares it as `sampler`
// and node mode's DaemonSet serves on it too (docs/sampler-metrics.md, "The port"). The bind
// address is every address and not loopback, since the collector that scrapes it runs outside
// the pod.
const Port = 9464

// The two numbers the sampler's own memory rule is written with. Both are about anonymous memory
// and neither about resident memory: under gVisor a process's resident memory counts the mapped
// `legion` binary, which is 101,978,204 bytes, and a live `legion` of this pod reads VmRSS
// 102,552 kB against 34,956 kB of anonymous memory (INFRA-1208, measured 2026-11-05). A resident
// rule at 64 MiB would end the sampler before it served one byte, and the launcher's five-second
// restart would make that permanent.
const (
	// memoryLimit is what debug.SetMemoryLimit is given at start. It bounds the Go heap, which
	// is the part of the sampler's anonymous memory the runtime can do something about: as the
	// heap nears the number the collector runs harder rather than letting it grow. It sits
	// below memoryExit so that the runtime's own answer comes first and the exit below is the
	// backstop, for memory no collection takes back.
	memoryLimit = 60 << 20
	// memoryExit is the anonymous memory the sampler ends on (dispatch://INFRA-1208/spec#b-a-rates:
	// "exits when its own anonymous memory passes 64 MiB").
	memoryExit = 64 << 20
)

// guardInterval is how often the guard reads the sampler's own anonymous memory, and a package
// var rather than a constant so that a test drives the exit without waiting a second for it, as
// the collectors' own rates are.
var guardInterval = time.Second

// shutdownGrace bounds the deferred Shutdown: a scrape being written when the sampler ends is
// given this long to finish, and the process leaves whether or not it does.
const shutdownGrace = 5 * time.Second

// Options is everything the sampler's process runs with: where it serves, which of the two
// collectors it runs — exactly one of Pod and Node, which is the mode — and where its own lines
// go. The collector's options carry the registry this serves and the root every read is given.
type Options struct {
	// Listen is the address the HTTP server binds, "0.0.0.0:9464" in a pod and a loopback
	// port of its own in a test.
	Listen string
	// Pod and Node are the two collectors, exactly one of them set: an issue pod's roles, or
	// every pod of one node.
	Pod  *PodOptions
	Node *NodeOptions
	// Log takes one line about the server itself: the address it serves on, a reading it
	// could not take, an exposition it could not write.
	Log func(string)
}

// check refuses options the sampler could not run with, in the shape PodOptions.check uses: the
// field, lower case, and what it must be. The collector's own options are checked here too, so
// nothing is bound for a sampler that would refuse its collector a moment later.
func (opts Options) check() error {
	switch {
	case opts.Listen == "":
		return errors.New("sampler: listen address is required")
	case opts.Pod == nil && opts.Node == nil:
		return errors.New("sampler: an issue pod's collector or a node's is required")
	case opts.Pod != nil && opts.Node != nil:
		return errors.New("sampler: one sampler reads an issue pod or a node, never both")
	case opts.Log == nil:
		return errors.New("sampler: log is required")
	case opts.Pod != nil:
		return opts.Pod.check()
	default:
		return opts.Node.check()
	}
}

// mode is the word /healthz carries and the one the log line names: which of the two collectors
// this sampler runs.
func (opts Options) mode() string {
	if opts.Node != nil {
		return "node"
	}
	return "pod"
}

// root is the root every read is given, the collector's own: "/" in a pod and on a node, a
// fixture directory in a test. The guard reads the sampler's own memory under it.
func (opts Options) root() string {
	if opts.Node != nil {
		return opts.Node.Root
	}
	return opts.Pod.Root
}

// registry is where the collector's readings land and what GET /metrics writes.
func (opts Options) registry() *Registry {
	if opts.Node != nil {
		return opts.Node.Registry
	}
	return opts.Pod.Registry
}

// Run is the sampler's process: it serves GET /metrics and GET /healthz on Listen and reads the
// pod or the node into the registry behind them, until the context ends or one of the two does.
//
//	GET /metrics  the registry's exposition, Content-Type ContentType, which resets every peak
//	              to the reading beside it (Registry.Write).
//	GET /healthz  200 {"mode":"pod","pass":true}: the mode this sampler runs in, and whether its
//	              collector has finished a pass, which is what it has read rather than how long
//	              it has run.
//
// A port it cannot bind ends it with one line naming the address and never a panic, and so does
// its own anonymous memory passing 64 MiB: both are failures the launcher answers by starting
// the sampler again five seconds later (dispatch://INFRA-1208/spec#b-a-safe), and neither is
// worth a stack trace in a pod's log. Nothing here calls os.Exit: the exit is the error this
// returns, which the command prints on one line and exits non-zero with, so a test drives it
// without ending the test binary.
//
// A context that ends is no failure and returns nil, as internal/repocache's Serve does. The
// collector is joined before this returns, so a sampler that has ended has stopped writing to
// the registry and to the log.
func Run(ctx context.Context, opts Options) error {
	if err := opts.check(); err != nil {
		return err
	}
	debug.SetMemoryLimit(memoryLimit)
	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("the sampler could not listen on %s: %w", opts.Listen, err)
	}
	s := &service{opts: opts, registry: opts.registry()}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	opts.Log(fmt.Sprintf("sampler: serving %s mode on %s", opts.mode(), listener.Addr()))

	collecting, stop := context.WithCancel(ctx)
	collected, ended := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(ended)
		collected <- s.collect(collecting)
	}()
	defer func() {
		stop()
		<-ended
	}()

	guard := time.NewTicker(guardInterval)
	defer guard.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			return fmt.Errorf("the sampler stopped serving %s: %w", listener.Addr(), err)
		case err := <-collected:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-guard.C:
			if err := s.overLimit(); err != nil {
				return err
			}
		}
	}
}

// service is one Run: what it was given, the registry it serves, whether a pass has been made,
// and the last line the guard logged, so a reading that fails every second is reported once and
// not sixty times a minute, as the collectors report their own.
//
// The flag is read by the HTTP handler and written by Run's own loop, which is why it is atomic;
// the logged line belongs to the guard alone.
type service struct {
	opts     Options
	registry *Registry
	pass     atomic.Bool
	logged   string
}

// health is what GET /healthz answers: a small JSON body with the mode and the pass above. It is
// 200 whatever it carries, since a sampler that is starting is not a sampler that has failed,
// and a probe that reads this has its answer either way.
type health struct {
	Mode string `json:"mode"`
	Pass bool   `json:"pass"`
}

// collect runs the collector of this sampler's mode until the context ends.
func (s *service) collect(ctx context.Context) error {
	passed := func() { s.pass.Store(true) }
	if s.opts.Node != nil {
		node := *s.opts.Node
		node.Passed = passed
		return RunNode(ctx, node)
	}
	pod := *s.opts.Pod
	pod.Passed = passed
	return RunPod(ctx, pod)
}

func (s *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		// The header is written with the first byte of the body, so a write that fails
		// half way through cannot be answered with a status: the collector reading this
		// port sees a broken answer, and the line below says why.
		if err := s.registry.Write(w); err != nil {
			s.opts.Log("sampler: the exposition could not be written: " + err.Error())
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(health{Mode: s.opts.mode(), Pass: s.pass.Load()})
	})
	return mux
}

// overLimit is the sampler's own memory rule: the anonymous memory its process holds now, and an
// error that ends the run once that passes memoryExit. It reads what procread reads for every
// other process (readAnonymous, the package var a test replaces), so a test drives the exit
// without allocating 64 MiB of its own.
//
// A reading it cannot take leaves the sampler running: a /proc that refuses one read of one file
// is no reason to end a sampler that is serving a pod, and the Go heap's own limit still holds.
// The collector's own pass reads that same file each second and counts what fails there as
// legion_sampler_read_errors_total{source="proc"}, so the guard counts nothing of its own and
// says it once rather than once a second.
func (s *service) overLimit() error {
	anonymous, err := readAnonymous(s.opts.root(), selfPID())
	if err != nil {
		if line := "sampler: its own anonymous memory could not be read: " + err.Error(); line != s.logged {
			s.logged = line
			s.opts.Log(line)
		}
		return nil
	}
	s.logged = ""
	if anonymous <= memoryExit {
		return nil
	}
	return fmt.Errorf("the sampler holds %d bytes of anonymous memory, past its %d-byte limit: ending so the launcher starts it again", anonymous, memoryExit)
}
