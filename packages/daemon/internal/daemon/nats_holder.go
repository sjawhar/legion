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
	// holdMapping takes the lock a mapping read is made under (workflowRuntime.mappingMu) and
	// answers the release of it. The rebuild holds it from before it reads the repositories its
	// new consumers are opened with to after the swap, because opening them is an UpdateConsumer
	// on the very GitHub durable the old intake still pulls from: a mapping read that set a
	// wider set on that durable while the rebuild dialled would have its filter written back to
	// the rebuild's older set, and the pinned nats-server steps a consumer past the messages a
	// narrowed filter does not match rather than holding them, so an event of a repository
	// mapped in that window is never delivered. It is this lock and not the fact lock because
	// the dial must stay out of the fact lock, and because the mapping read is the one writer of
	// that filter beside the rebuild; the read takes this lock before the fact lock, so the
	// rebuild taking them in that same order adds no cycle. nil holds nothing, which is what a
	// caller with no mapping read means.
	holdMapping func() (release func())
	// serialize runs what it is handed under one hold of the lock every fact applies under
	// (intake.SerializeFacts), which a mapping read takes across its own SetRepositories
	// (refreshMapping): the rebuild reads there what the consumers it replaces hear, hands it to
	// the new ones and swaps, all inside that hold, so a mapping read landing between the read
	// and the swap cannot set its repositories on the consumers being replaced and lose them
	// with them.
	serialize func(func(ctx context.Context) error) error
	log       *slog.Logger

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
	holdMapping func() (release func()),
	serialize func(func(ctx context.Context) error) error,
) *natsHolder {
	if log == nil {
		log = slog.Default()
	}
	if holdMapping == nil {
		holdMapping = func() func() { return func() {} }
	}
	return &natsHolder{open: open, holdMapping: holdMapping, serialize: serialize, log: log, conn: conn, consumers: consumers}
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
// were they held under it. What is held under the fact lock (serialize) is the handover alone:
// the repositories the new consumers hear are read from the consumers they replace and set on
// them there, and the swap happens before the hold is let go, so a mapping read that lands
// between the read and the swap waits and then sets its repositories on the consumers this
// rebuild swapped in (refreshMapping's SetRepositories). Nothing after the swap can fail, so a
// rebuild that swapped never reports a failure the rebuilder would log as "the daemon keeps the
// one it has" — which would leave the holder's digest unrecorded and rebuild NATS again at the
// next reread.
//
// The open is held under the mapping's own lock (holdMapping) from before the repositories it
// opens with are read to after the swap. Opening the consumers is an UpdateConsumer on the very
// GitHub durable the old intake is still pulling from, so without that hold a mapping read
// could widen that durable's filter while this rebuild dialled and the dial's own open would
// write the older, narrower filter back over it; until the swap set the wider one again, the
// old intake would pull under the narrow filter, and the pinned nats-server steps a consumer
// past the messages a filter does not match rather than holding them, so a GitHub event of a
// repository mapped in that window is delivered to nothing, ever. The hold costs no fact a
// wait, since nothing but a mapping read takes it, and it is let go before the old generation
// is cancelled and drained.
func (h *natsHolder) rebuild(cfg *config.Config) error {
	release := h.holdMapping()
	conn, consumers, err := h.open(*cfg)
	if err != nil {
		release()
		return err
	}
	var old *nats.Conn
	var cancel context.CancelFunc
	var settled chan struct{}
	err = h.serialize(func(ctx context.Context) error {
		if err := consumers.SetRepositories(ctx, h.intake().Repositories()); err != nil {
			return fmt.Errorf("hand the rebuilt consumers the repositories this daemon hears: %w", err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		old, cancel, settled = h.conn, h.cancelRun, h.settled
		h.conn, h.consumers, h.generation = conn, consumers, h.generation+1
		h.cancelRun, h.settled = nil, nil
		return nil
	})
	release()
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
