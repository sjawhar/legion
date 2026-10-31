package cistore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// maxRecordBytes and maxSettlementBytes bound a head's record and the settlement published from
// it, each of which goes to NATS whole. GitHub allows 50,000 check runs in one check suite, so the
// number of check names a head collects has no bound of GitHub's own. The record is kept well under
// the server's 1 MiB default, since it is written whole on every observation; about 1,300 checks of
// ordinary names fit. The settlement needs its own bound, because a name costs more there: a
// failing check is named three times to the record's two, inside a JSON string in the envelope's
// JSON, so a `"` costs 12 bytes a character in the settlement against 4 in the record. Its bound
// is 64 KiB under the 1 MiB default, room for what a later settlement of the same record adds (a
// longer generation, `superseded_settlement`).
const (
	maxRecordBytes     = 384 << 10
	maxSettlementBytes = 1<<20 - 64<<10
)

// write applies mutate to the current state of identity's record, a new one carrying only the
// identity when there is none, and writes it with a compare-and-swap. A lost compare-and-swap or a
// transient KV error (kvErrorLasts) is retried from a fresh read until recordBudget runs out: the
// write carries every observation of a batch, so a NATS reconnect or a JetStream 503 during a
// server restart must not fail them all, and GitHub does not redeliver a delivery the listener
// refused. A lasting error fails it at once. Each attempt takes the handle the latest Rewatch
// installed, and each KV call is given up on if the store is rewatched before it is answered. A KV
// call is also given up on when ctx ends (errKVUnanswered, the caller's wait: update), which fails
// the write at once, since no retry fits in an ended ctx. A write that runs out of the budget, or
// of ctx, logs `ci record exceeded its retry budget`, one JSON line naming the head, the checks the
// record would have held (0 when no attempt read it), the attempts it made, the observations it
// carried (each a delivery the webhook answers 503) and the last attempt's error, so an alarm can
// count those 503s by their cause. It is an ERROR because each one means GitHub was answered 503.
//
// A write given up on that way may still land after its callers have answered. It is a
// compare-and-swap at the revision its attempt read (a create when it read none), so it never
// overwrites a write that landed since: that write moved the revision and this one conflicts and
// is dropped. If it lands, the record holds the batch's observations although each delivery was
// answered 503, and the redelivery of one finds its observation there, which its mutation's rules
// apply as no change, so nothing is written twice.
//
// A write that would take the record past maxRecordBytes, or its settlement past
// maxSettlementBytes, is refused with bus.ErrTooLarge, which a redelivery would meet again: the
// record is written as it was, marked Overflowed, so it never settles on the checks it could not
// hold.
func (s *Store) write(ctx context.Context, identity State, observations int, mutate func(*State) bool) error {
	key := Key(identity.Owner, identity.Repo, identity.Number, identity.SHA)
	deadline := time.Now().Add(recordBudget)
	var retryErr error
	checks := 0
	spent := func(attempts int, err error) error {
		s.logger.Error("ci record exceeded its retry budget",
			slog.String("owner", identity.Owner),
			slog.String("repo", identity.Repo),
			slog.String("number", identity.Number),
			slog.String("sha", identity.SHA),
			slog.Int("checks", checks),
			slog.Int("attempts", attempts),
			slog.Int("observations", observations),
			slog.String("error", err.Error()))
		return err
	}
	for attempt := 0; ; attempt++ {
		if retryErr != nil {
			if time.Now().After(deadline) {
				return spent(attempt, retryErr)
			}
			time.Sleep(casBackoff(attempt - 1))
		}
		kv := s.watcher.KV()
		entry, err := kvCall(ctx, s.logger, s.nextRewatch(), func() (nats.KeyValueEntry, error) { return kv.Get(key) })
		var st State
		var rev uint64
		switch {
		case err == nil:
			if err := json.Unmarshal(entry.Value(), &st); err != nil {
				return err
			}
			rev = entry.Revision()
		case errors.Is(err, nats.ErrKeyNotFound):
			st = identity
		case errors.Is(err, errKVUnanswered):
			return spent(attempt+1, err)
		case kvErrorLasts(err):
			return err
		default:
			retryErr = err
			continue
		}
		beforeHash := st.Hash()
		generation := st.Generation
		if !mutate(&st) {
			return nil
		}
		checks = len(st.Checks)
		if rev != 0 && st.Hash() != beforeHash && st.Generation == generation {
			bumpGeneration(&st)
		}
		buf, err := encodeRecord(&st)
		if err != nil {
			return err
		}
		settlement, err := settlementSize(st)
		if err != nil {
			return err
		}
		refused := boundsRefusal(key, len(buf), settlement)
		if refused != nil {
			// Decoded afresh rather than copied: mutate changed st in place, and the maps a shallow
			// copy of the read would share are the ones it changed.
			stored := identity
			if rev != 0 {
				if err := json.Unmarshal(entry.Value(), &stored); err != nil {
					return err
				}
			}
			if stored.Overflowed {
				return refused
			}
			st = stored
			st.Overflowed = true
			if buf, err = encodeRecord(&st); err != nil {
				return err
			}
		}
		_, err = kvCall(ctx, s.logger, s.nextRewatch(), func() (uint64, error) {
			if rev == 0 {
				return kv.Create(key, buf)
			}
			return kv.Update(key, buf, rev)
		})
		switch {
		case err == nil:
			return refused
		case errors.Is(err, errKVUnanswered):
			return spent(attempt+1, err)
		case kvErrorLasts(err):
			return err
		case isCASConflict(err):
			retryErr = errors.New("cistore: record exceeded CAS budget")
		default:
			retryErr = err
		}
	}
}

// boundsRefusal is the refusal of a write to key's record of record bytes whose settlement is
// settlement bytes, when either is past its bound (maxRecordBytes, maxSettlementBytes), and nil
// otherwise.
func boundsRefusal(key string, record, settlement int) error {
	switch {
	case record > maxRecordBytes:
		return fmt.Errorf("%w: head %s's record would be %d bytes, past its %d-byte bound",
			bus.ErrTooLarge, key, record, maxRecordBytes)
	case settlement > maxSettlementBytes:
		return fmt.Errorf("%w: head %s's settlement would be %d bytes, past its %d-byte bound",
			bus.ErrTooLarge, key, settlement, maxSettlementBytes)
	}
	return nil
}

// settlementSize is the size of the settlement st would publish, encoded as the summary loop sends
// it; its topic travels in the protocol line, which a server's max payload does not count.
func settlementSize(st State) (int, error) {
	env, err := settlementEnvelope(st, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(env)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// errKVRewatched is what a KV call answers when the store was rewatched before its answer came.
var errKVRewatched = errors.New("cistore: store rewatched before the KV call was answered")

// errKVUnanswered is what a KV call answers, wrapping ctx's error, when ctx ended before its
// answer came.
var errKVUnanswered = errors.New("cistore: the KV call did not answer")

// kvCall returns call's answer, errKVRewatched if rewatched closes first, or errKVUnanswered if ctx
// ends first; a ctx that has already ended starts no call. A legacy nats.go KV call takes no
// context and waits up to the JetStream MaxWait for its answer. One sent just before the server
// went away gets none, so it would hold its write, and every caller queued behind the write, for
// the whole wait. Rewatch runs on every reconnect, so the call is given up on then and retried on
// the new connection within the write's budget. Rewatch also runs when the listener's self-health
// rebuilds a watcher on a live connection; a call given up on there may still have been answered,
// and is retried like any other. Otherwise a call is waited for until ctx ends (the caller's wait,
// two budgets at most: update), even past the write's budget, so a server that stalls and then
// answers within that wait still answers that call, and one that stays silent fails the write when
// it ends rather than at the MaxWait. A stall of a few seconds can also fail a batch whose
// compare-and-swap then loses, since such an attempt returns rather than reading again once the
// budget is spent (write). This is bus.Bounded with the rewatch and the panic handling below added.
// A call given up on ends on its own, with its answer or at the MaxWait. A write it carried is a
// compare-and-swap at the revision its attempt read, so if the server applies it after the retry's
// write it conflicts, and if before, the retry's fresh read finds the batch's observations already
// there and writes nothing; one given up on at ctx's end has no retry, and write says why its
// landing late is safe too. A call that panics re-panics in its caller, as it would without the
// goroutine, so the batch fails (writeBatch); one that panics after its caller has moved on is
// logged through logger.
func kvCall[T any](ctx context.Context, logger *logging.Logger, rewatched <-chan struct{}, call func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("%w: %w", errKVUnanswered, err)
	}
	type answer struct {
		value    T
		err      error
		panicked any
	}
	answered := make(chan answer, 1)
	go func() {
		var a answer
		defer func() {
			if p := recover(); p != nil {
				a = answer{panicked: p}
			}
			answered <- a
		}()
		a.value, a.err = call()
	}()
	giveUp := func(at string) {
		go func() {
			if a := <-answered; a.panicked != nil {
				logger.Error("cistore: a KV call given up on at "+at+" panicked", slog.String("panic", fmt.Sprint(a.panicked)))
			}
		}()
	}
	select {
	case a := <-answered:
		if a.panicked != nil {
			panic(a.panicked)
		}
		return a.value, a.err
	case <-rewatched:
		giveUp("a rewatch")
		return zero, errKVRewatched
	case <-ctx.Done():
		giveUp("its context's end")
		return zero, fmt.Errorf("%w: %w", errKVUnanswered, ctx.Err())
	}
}

// nextRewatch returns the channel the next Rewatch closes.
func (s *Store) nextRewatch() <-chan struct{} {
	s.rewatchMu.Lock()
	defer s.rewatchMu.Unlock()
	return s.rewatched
}

// kvErrorLasts reports whether a KV error will outlast a retry within a write's budget. A
// compare-and-swap conflict never does: both kinds are JetStream 400s (ErrKeyExists, wrong last
// sequence), so it is answered first, and a caller need not ask isCASConflict before it. Lasting:
// the connection is closed, draining or invalid; the connection is not allowed the request
// (authorization, an expired credential, a permissions violation); the bucket is not there when
// the handle is taken; JetStream is not enabled for the server or the account, which JetStream
// reports as a 503 but which is configuration no retry within the budget changes, unlike the 503
// of a server restarting; JetStream refuses the request itself (any other 4xx, a record over the
// payload limit); or the handle refuses the key (bus.ErrRefused, bus.EnsureKeyValue: one whose
// subject NATS would refuse, or one outside nats.go's key alphabet). Anything else is transient,
// and what the retry actually rescues is a NATS reconnect (ErrReconnectBufExceeded, a request
// refused while the connection reconnects, and errKVRewatched, a request given up on at the
// rewatch that follows it), a JetStream 503 while a server restarts, and no responders, which a
// request gets whenever no server answers for the stream it names. A timeout is transient too,
// though one at the JetStream MaxWait never comes: the caller's wait (update, two budgets) ends
// first, and the call is given up on then (errKVUnanswered, which write fails on first).
//
// No responders is transient because it cannot be told apart from a restart: a restarting server
// answers it for as long as it is away, and a batch that should have been retried failed instead.
// It is the read (kv.Get) that this decides: on the write side nats.go answers ErrNoStreamResponse
// instead (js.go), which this never named as lasting. The trade is the case it also covers, a
// bucket deleted under a live handle, which answers no responders like a key no stream serves:
// such a write now spends the whole budget before it fails, rather than failing at once. It still
// fails within the budget, and nothing is written either way; a caller queued behind that write
// waits for its own write too, so it can wait two budgets (cistore.go).
func kvErrorLasts(err error) bool {
	if isCASConflict(err) {
		return false
	}
	for _, lasting := range lastingKVErrors {
		if errors.Is(err, lasting) {
			return true
		}
	}
	var jsErr nats.JetStreamError
	if errors.As(err, &jsErr) {
		if api := jsErr.APIError(); api != nil && api.Code >= 400 && api.Code < 500 {
			return true
		}
	}
	return false
}

// lastingKVErrors are the errors kvErrorLasts names by value.
var lastingKVErrors = []error{
	nats.ErrConnectionClosed, nats.ErrConnectionDraining, nats.ErrInvalidConnection,
	nats.ErrAuthorization, nats.ErrAuthExpired, nats.ErrPermissionViolation,
	nats.ErrBucketNotFound,
	nats.ErrJetStreamNotEnabled, nats.ErrJetStreamNotEnabledForAccount,
	nats.ErrMaxPayload, bus.ErrRefused,
}
