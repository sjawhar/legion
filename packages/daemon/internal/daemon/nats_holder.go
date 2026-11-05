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
	// open builds a connection and its consumers from a configuration.
	open func(cfg config.Config) (*nats.Conn, *intake.Consumers, error)
	log  *slog.Logger

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

func newNATSHolder(conn *nats.Conn, consumers *intake.Consumers, log *slog.Logger, open func(cfg config.Config) (*nats.Conn, *intake.Consumers, error)) *natsHolder {
	if log == nil {
		log = slog.Default()
	}
	return &natsHolder{open: open, log: log, conn: conn, consumers: consumers}
}

func (h *natsHolder) holder() string { return config.HolderNATS }

// credentialFiles are the nkey seeds the connection authenticates as, which a rebuild reads
// again (readNATSConnection).
func (h *natsHolder) credentialFiles(cfg *config.Config) []string {
	return []string{cfg.NatsNkeySeedFile, cfg.NatsDaemonNkeySeedFile}
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
func (h *natsHolder) rebuild(cfg *config.Config) error {
	conn, consumers, err := h.open(*cfg)
	if err != nil {
		return err
	}
	h.mu.Lock()
	old, cancel, settled := h.conn, h.cancelRun, h.settled
	h.conn, h.consumers, h.generation = conn, consumers, h.generation+1
	h.cancelRun, h.settled = nil, nil
	h.mu.Unlock()
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
