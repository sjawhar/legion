// Package reread carries one secret's reread from the broker pod that was asked for it to every
// other pod serving the same database, over Postgres LISTEN/NOTIFY. A reread is how a write to
// the secret store reaches the broker before its next policy reload: the pod that handles
// POST /v1/secrets/{name}/reread rereads that one name at once, and until this fan-out existed
// every other pod went on serving the old tags for up to a reload interval. A retag that narrows
// who may have a secret is therefore enforced fleet-wide within seconds rather than at each pod's
// next five-minute reload.
//
// The two halves are deliberately apart: Notifier is the sending half, which cmd/broker hands the
// HTTP layer, and Listener is the receiving half, which cmd/broker runs for the life of the
// process. Neither imports internal/broker/api or internal/broker/policy; the caller wires a
// reread function in, so this package can be tested against a real database with no secret store
// anywhere near it.
package reread

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pglisten"
)

// Channel is the Postgres channel both halves speak on, and the one place its name is written.
// pglisten.Listener splices its Channels into a listen statement as written, so a channel name
// may only ever be a compile-time constant of the package that listens on it - never a value
// read from a database, a request or the environment, which would be a statement a caller wrote.
const Channel = "broker_secret_reread"

// refreshTimeout bounds the reread one notification drives. OnNotify runs on the listener's one
// goroutine and the notifications behind it wait, so the reread cannot be left to run for as
// long as the process does: a store that accepts the connection and never answers would stop the
// fan-out for this pod entirely. Thirty seconds is long enough for a reread that waits out a full
// policy reload holding the writer lock and then reads one secret, and a reread that needs more
// than that is one the pod's own five-minute reload is the backstop for.
const refreshTimeout = 30 * time.Second

// notification is what one reread sends: the secret's name, and the sender id of the process that
// sent it, so a pod can tell its own notification from another's. Both halves marshal and parse it
// through this one definition.
type notification struct {
	Name   string `json:"name"`
	Sender string `json:"sender"`
}

// marshal renders the payload one reread sends.
func marshal(name, sender string) (string, error) {
	payload, err := json.Marshal(notification{Name: name, Sender: sender})
	if err != nil {
		return "", fmt.Errorf("render the secret-reread notification of %s: %w", name, err)
	}
	return string(payload), nil
}

// parse reads a payload back, refusing one that is not this package's own JSON object or that
// names no secret. A notification naming no sender is kept: it can only come from a process that
// is not this build, and dropping the reread would be worse than running it.
func parse(payload string) (notification, error) {
	var note notification
	if err := json.Unmarshal([]byte(payload), &note); err != nil {
		return notification{}, fmt.Errorf("read a secret-reread notification: %w", err)
	}
	if note.Name == "" {
		return notification{}, fmt.Errorf("read a secret-reread notification: it names no secret")
	}
	return note, nil
}

// Execer is the one call the sending half makes: the shared pool's own Exec, or a transaction's.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Notifier answers the function that tells every other pod to reread name: cmd/broker makes one
// from the shared pool and this process's own sender id and hands it to the HTTP layer, which
// calls it after a reread of its own. The statement is parameterized, so no caller's secret name
// reaches Postgres as text of a statement.
//
// A failure is returned rather than logged, because what a failed fan-out means belongs to the
// caller: the pod's own reread has already happened and its answer stands, and only the caller
// knows at what level to say that the other pods will not learn of it before their next reload.
func Notifier(db Execer, sender string) func(ctx context.Context, name string) error {
	return func(ctx context.Context, name string) error {
		payload, err := marshal(name, sender)
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `select pg_notify($1, $2)`, Channel, payload); err != nil {
			return fmt.Errorf("tell the other broker pods to reread %s: %w", name, err)
		}
		return nil
	}
}

// Listener is the receiving half: one LISTEN connection of its own on Channel, which rereads
// every secret another pod announces.
type Listener struct {
	// Sender is this process's own sender id, the one its Notifier stamps on what it sends.
	// Postgres delivers a notification to every session listening on the channel, the one this
	// process holds included - the LISTEN connection is not the connection that ran pg_notify -
	// so a notification this id sent is this pod's own reread coming back, and is dropped.
	Sender string
	// Dial opens the LISTEN connection: cmd/broker passes (*store.Store).Listener, which hands
	// over a connection of its own.
	Dial func(ctx context.Context) (*pgx.Conn, error)
	// Refresh rereads one secret. It is (*policy.Current).RefreshOne without its Lookup, kept a
	// plain function field so this package imports no policy: the fan-out decides nothing about
	// a secret, it only says which name to read again.
	//
	// A reread this drives never notifies. The fan-out is one hop by construction - only a
	// caller of the HTTP reread sends, and a notification's reread sends nothing - so no
	// notification can come back round, and nothing here needs to recognize or break a cycle.
	Refresh func(ctx context.Context, name string) error
	// connected, when set, runs on each successful LISTEN, beside the line onConnected logs.
	// Only this package's own test sets it: it is how a test knows its listener's session is
	// listening before it sends the notification that session must not miss.
	connected func()
}

// Run listens until ctx ends, redialing on every failure (pglisten.Listener).
func (l *Listener) Run(ctx context.Context) {
	listener := &pglisten.Listener{
		Owner:       "broker",
		Dial:        l.Dial,
		Channels:    []string{Channel},
		OnConnected: l.onConnected,
		OnNotify:    func(_, payload string) { l.notified(ctx, payload) },
	}
	listener.Run(ctx)
}

// onConnected logs that the listener has its connection, and deliberately catches nothing up.
// A notification sent while this pod had no connection is lost, and that is the trade this
// design makes: the catch-up available here is a full policy refresh, which would cost every pod
// a whole-namespace read of the secret store on every redial - a backoff that reaches thirty
// seconds means a database outage ends in a burst of them - and would buy nothing the pod's own
// five-minute reload does not already give. So the promise the fan-out is documented as keeping
// is "within seconds, except while a pod's listener is redialing, where that reload is the
// backstop".
func (l *Listener) onConnected() {
	slog.Info("broker: listening for the other pods' secret rereads", "channel", Channel)
	if l.connected != nil {
		l.connected()
	}
}

// notified rereads the secret one notification names, on the listener's goroutine. A payload this
// build cannot read, and a name the reread refuses, are each dropped with one WARN and nothing
// else: a notification is a hint to read a secret again, so there is nothing to retry and nobody
// to answer. Neither line carries the payload's own text, which an unreadable payload makes
// arbitrary bytes, and each names the fan-out rather than repeating the HTTP reread's own
// "broker: reread secret failed", so an operator alarming on that line still reads one pod's
// own request path alone.
func (l *Listener) notified(ctx context.Context, payload string) {
	note, err := parse(payload)
	if err != nil {
		slog.Warn("broker: dropped an unreadable secret-reread notification", "error", err, "payload_bytes", len(payload))
		return
	}
	if note.Sender == l.Sender {
		return
	}
	refresh, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if err := l.Refresh(refresh, note.Name); err != nil {
		slog.Warn("broker: a secret reread another pod sent failed", "secret", note.Name, "sender", note.Sender, "error", err)
	}
}
