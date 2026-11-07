package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// natsHolder is the NATS connection and its durable consumers (config.HolderNATS). A rebuild
// opens a new connection with the configuration's URLs and seed, moves this project's durable
// consumers onto it, and only then ends the old connection's intake and closes it, waiting for
// the run it ended to settle its messages first (rebuild). No consumer is deleted and no
// position is lost — the consumers are durable, and the new ones resume where the old ones
// stopped.
type natsHolder struct {
	// open builds a connection and this project's durable consumers from a configuration. It
	// runs under no lock every fact applies under: the dial, the reconnect and the four consumer
	// opens are network work, and a fact applying while a rebuild ran would otherwise wait on it
	// (workflowRuntime.connect).
	open func(cfg config.Config) (*nats.Conn, *intake.Consumers, error)
	// holdMapping is the lock a mapping read is made under (workflowRuntime.mappingMu). The
	// rebuild holds it from before it opens its consumers to after the swap; rebuild says why,
	// and what that hold costs every other settings change.
	holdMapping sync.Locker
	// handover hands the consumers this rebuild opened the repositories they are to hear and
	// swaps them in, under one hold of the lock every fact applies under
	// (workflowRuntime.natsHandover, intake.SerializeFacts): it works out what those consumers
	// are to hear for the renames it is handed — the renames of the configuration being built
	// from — hands it to them, and calls swap, this holder's own swap of its connection, its
	// consumers and its generation, before the hold is let go. A mapping read holds that same
	// lock across its own SetRepositories (refreshMapping); rebuild says why the three are one
	// hold. A handover that returns an error has not called swap, so nothing was swapped.
	handover func(consumers *intake.Consumers, renames ghrepo.Renames, swap func()) error
	log      *slog.Logger

	mu         sync.Mutex
	conn       *nats.Conn
	consumers  *intake.Consumers
	generation uint64
	// cancelRun ends the intake loop's current generation; generation above is what a loop whose
	// run ended compares, to tell a rebuild's cancel from the daemon's stop (rebuilt). settled is
	// closed by that loop once its Run has returned, which is what a rebuild waits for before it
	// closes the connection that run was on; nil when no generation is running.
	cancelRun context.CancelFunc
	settled   chan struct{}
}

func newNATSHolder(conn *nats.Conn, consumers *intake.Consumers, log *slog.Logger,
	open func(cfg config.Config) (*nats.Conn, *intake.Consumers, error),
	holdMapping sync.Locker,
	handover func(consumers *intake.Consumers, renames ghrepo.Renames, swap func()) error,
) *natsHolder {
	if log == nil {
		log = slog.Default()
	}
	return &natsHolder{open: open, holdMapping: holdMapping, handover: handover, log: log, conn: conn, consumers: consumers}
}

func (h *natsHolder) holder() string { return config.HolderNATS }

// credentials are the nkey seeds the connection authenticates as, which a rebuild reads again
// (readNATSConnection).
func (h *natsHolder) credentials(cfg *config.Config) string {
	return credentialDigest(cfg.NatsNkeySeedFile, cfg.NatsDaemonNkeySeedFile)
}

// connection and intake are the connection and the consumers as they now stand.
func (h *natsHolder) connection() *nats.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn
}

func (h *natsHolder) intake() *intake.Consumers {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.consumers
}

// begin is one generation of the intake loop: the consumers to run, a context the next rebuild
// cancels, the generation's number, which the loop compares to tell a rebuild's cancel from the
// daemon's stop, and the settled the loop closes once its Run has returned.
func (h *natsHolder) begin(ctx context.Context) (*intake.Consumers, context.Context, uint64, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	running, cancel := context.WithCancel(ctx)
	h.cancelRun, h.settled = cancel, make(chan struct{})
	return h.consumers, running, h.generation, h.settled
}

// rebuilt says whether generation is behind the holder's: the loop's run was ended by a rebuild
// and the next one is the new connection's.
func (h *natsHolder) rebuilt(generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return generation != h.generation
}

// rebuild opens a connection with the configuration's URLs and seed, moves this project's
// durable consumers onto it, and only then ends the old connection's intake and closes it: the
// generation the intake loop runs (begin) is cancelled, and the connection is closed once that
// loop says its Run has returned, which is once every message it was holding has been acked or
// nak'd. An ack of a fact in flight therefore goes to the connection it was delivered on, never
// to a closed one, and a fact the handler had not finished is redelivered by the durable
// consumer the new connection holds. No consumer is deleted and no position is lost.
//
// The connection and its consumers are opened under no fact lock (open), because that is the
// network work: a dial, a reconnect and four consumer opens, which every fact would wait behind
// were they held under it. What is held under the fact lock is the handover alone (handover,
// workflowRuntime.natsHandover): the repositories the new consumers are to hear are worked out
// there and set on them there, and the swap this rebuild hands it runs before the hold is let
// go, so a mapping read that lands between the two waits and then sets its repositories on the
// consumers this rebuild swapped in (refreshMapping's SetRepositories). Nothing after the swap
// can fail, so a rebuild that swapped never reports a failure the rebuilder would log as "the
// daemon keeps the one it has" — which would leave the holder's digest unrecorded and rebuild
// NATS again at the next reread.
//
// What those consumers hear is worked out again from the daemon's state and from the
// configuration this rebuild is building from (handover, workflowRuntime.natsHearing); it is not
// copied from the consumers being replaced alone. A `repository_renames` change rebuilds this
// holder (config.Classes' row), and the set the last mapping read left was worked out under the
// renames before it, so a copy would leave the new pair's partner off the GitHub durable's
// FilterSubjects: the merges, check runs and reviews GitHub publishes under the name the
// repository moved to would reach nothing, and nothing redelivers them, until some unrelated
// thing read the mapping again. Working it out here costs one read of the project's records in
// the hold the handover already has, and no read of Dispatch or GitHub: the renames are the
// configuration's, and what the mapping serves is already in memory.
//
// The open and the handover are held under the mapping's own lock (holdMapping), from before the
// consumers are opened to after the swap. Opening them is an UpdateConsumer on the very GitHub
// durable the old intake is still pulling from, so without that hold a mapping read could widen
// that durable's filter while this rebuild dialled and the dial's own open would write the
// older, narrower filter back over it; until the swap set the wider one again, the old intake
// would pull under the narrow filter, and the pinned nats-server steps a consumer past the
// messages a filter does not match rather than holding them, so a GitHub event of a repository
// mapped in that window is delivered to nothing, ever. It is this lock and not the fact lock
// because the dial must stay out of the fact lock, and because a mapping read is the one writer
// of that filter beside this rebuild; the read takes this lock before the fact lock, so the
// rebuild taking them in that same order adds no cycle.
//
// That hold costs no fact a wait, since nothing but a mapping read takes the mapping lock, and
// it is let go before the old generation is cancelled and drained. It does cost the daemon's
// other settings changes: a rebuild runs under the settings holder's lock — config.Live.Apply
// holds l.mu across an apply and its observers (rebuilder.observe), and the watcher's reread
// runs under Live.WhileSettled (daemon.go's serve) — so for as long as this rebuild waits behind
// a mapping read already running, every other settings change and the file watcher's own settle
// wait behind it. That read is Dispatch's listing of the mapping and GitHub's installation
// reads, bounded at 15 s each (appMintAttempt, installReadAttempt), so a change that rebuilds
// NATS can hold the daemon's settings for tens of seconds, as the drain after the swap can
// (drainBound).
func (h *natsHolder) rebuild(cfg *config.Config) error {
	h.holdMapping.Lock()
	conn, consumers, err := h.open(*cfg)
	if err != nil {
		h.holdMapping.Unlock()
		return err
	}
	var old *nats.Conn
	var cancel context.CancelFunc
	var settled chan struct{}
	err = h.handover(consumers, cfg.RepositoryRenames, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		old, cancel, settled = h.conn, h.cancelRun, h.settled
		h.conn, h.consumers, h.generation = conn, consumers, h.generation+1
		h.cancelRun, h.settled = nil, nil
	})
	h.holdMapping.Unlock()
	if err != nil {
		// Nothing was swapped, so the connection this rebuild opened is the daemon's to close:
		// left open it would hold a second set of consumers delivering onto a connection no
		// reader of the holder can reach.
		conn.Close()
		return err
	}
	if cancel != nil {
		cancel()
	}
	if settled != nil {
		select {
		case <-settled:
		case <-time.After(drainBound):
			h.log.Warn("legion settings: the intake of the NATS connection being replaced did not settle; the daemon closes it and its durable consumers redeliver whatever was in flight",
				"holder", config.HolderNATS, "waited", drainBound)
		}
	}
	if old != nil {
		old.Close()
	}
	return nil
}

// close ends the connection the holder holds, which the daemon's stop calls.
func (h *natsHolder) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		h.conn.Close()
		h.conn = nil
	}
}

// openNATS is how the daemon opens a connection and this project's durable consumers from a
// configuration: boot's own path (workflowRuntime.connect) and the holder's rebuild run it, so
// the two can never settle differently. It reads the daemon's nkey seed again (readNATSConnection),
// which is how a rotated seed reaches the connection.
func openNATS(ctx context.Context, w *workflowRuntime, cfg config.Config, lookup func(string) (string, bool), spec intake.ConsumerSpec) (*nats.Conn, *intake.Consumers, error) {
	nc, err := readNATSConnection(cfg, lookup)
	if err != nil {
		return nil, nil, err
	}
	conn, err := natsauth.Connect(cfg.NatsURLs, nc.seed, natsauth.LogEvents(w.log), natsauth.ReconnectForever())
	if err != nil {
		return nil, nil, fmt.Errorf("connect Envoy NATS: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("open Envoy JetStream: %w", natsauth.WithLastError(err, conn))
	}
	consumers, err := intake.OpenConsumers(ctx, js, w.pool, spec)
	if err != nil {
		conn.Close()
		return nil, nil, natsauth.WithLastError(err, conn)
	}
	return conn, consumers, nil
}
