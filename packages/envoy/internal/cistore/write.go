package cistore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// write applies mutate to the current state of identity's record, a new one carrying only the
// identity when there is none, and writes it with a compare-and-swap. A lost compare-and-swap or a
// transient KV error (kvErrorLasts) is retried from a fresh read until recordBudget runs out: the
// write carries every observation of a batch, so a NATS reconnect or a JetStream 503 during a
// server restart must not fail them all, and GitHub does not redeliver a delivery the listener
// refused. A lasting error fails it at once. Each attempt takes the handle the latest Rewatch
// installed, and each KV call is given up on if the store is rewatched before it is answered.
func (s *Store) write(identity State, mutate func(*State) bool) error {
	key := Key(identity.Owner, identity.Repo, identity.Number, identity.SHA)
	deadline := time.Now().Add(recordBudget)
	var retryErr error
	for attempt := 0; ; attempt++ {
		if retryErr != nil {
			if time.Now().After(deadline) {
				return retryErr
			}
			time.Sleep(casBackoff(attempt - 1))
		}
		kv := s.watcher.KV()
		entry, err := kvCall(s.nextRewatch(), func() (nats.KeyValueEntry, error) { return kv.Get(key) })
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
		if rev != 0 && st.Hash() != beforeHash && st.Generation == generation {
			bumpGeneration(&st)
		}
		buf, err := json.Marshal(st)
		if err != nil {
			return err
		}
		_, err = kvCall(s.nextRewatch(), func() (uint64, error) {
			if rev == 0 {
				return kv.Create(key, buf)
			}
			return kv.Update(key, buf, rev)
		})
		switch {
		case err == nil:
			return nil
		case kvErrorLasts(err):
			return err
		case isCASConflict(err):
			retryErr = errors.New("cistore: record exceeded CAS budget")
		default:
			retryErr = err
		}
	}
}

// errKVRewatched is what a KV call answers when the store was rewatched before its answer came.
var errKVRewatched = errors.New("cistore: store rewatched before the KV call was answered")

// kvCall returns call's answer, or errKVRewatched if rewatched closes first. A legacy nats.go KV
// call takes no context and waits up to the JetStream MaxWait for its answer. One sent just before
// the server went away gets none, so it would hold its write, and every caller queued behind the
// write, for the whole wait. Rewatch runs on every reconnect, so the call is given up on then and
// retried on the new connection within the write's budget. Rewatch also runs when the listener's
// self-health rebuilds a watcher on a live connection; a call given up on there may still have
// been answered, and is retried like any other. Until the store is rewatched a call is waited for
// as long as the server takes, up to the MaxWait: a server that stalls and then answers loses
// nothing. A call given up on ends on its own, with its answer or at the MaxWait. A write it
// carried is a compare-and-swap at the revision its attempt read, so if the server applies it
// after the retry's write it conflicts, and if before, the retry's fresh read finds the batch's
// observations already there and writes nothing. A call that panics re-panics in its caller, as it
// would without the goroutine, so the batch fails (writeBatch); one that panics after its caller
// has moved on is logged.
func kvCall[T any](rewatched <-chan struct{}, call func() (T, error)) (T, error) {
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
	select {
	case a := <-answered:
		if a.panicked != nil {
			panic(a.panicked)
		}
		return a.value, a.err
	case <-rewatched:
		go func() {
			if a := <-answered; a.panicked != nil {
				slog.Error("cistore: a KV call given up on at a rewatch panicked", slog.String("panic", fmt.Sprint(a.panicked)))
			}
		}()
		var zero T
		return zero, errKVRewatched
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
// (authorization, an expired credential, a permissions violation); no stream answers for the
// record (a deleted bucket, like a key no stream serves, answers no responders); JetStream is not
// enabled for the server or the account, which JetStream reports as a 503 but which is
// configuration no retry within the budget changes, unlike the 503 of a server restarting; or
// JetStream refuses the request itself (any other 4xx, an invalid key, a record over the payload
// limit). Anything else is transient, and what the retry actually rescues is a NATS reconnect
// (ErrReconnectBufExceeded, a request refused while the connection reconnects, and errKVRewatched,
// a request given up on at the rewatch that follows it) and a JetStream 503 while a server
// restarts; a timeout is transient too, but it arrives only after the JetStream MaxWait, past the
// whole budget (recordBudget).
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
	nats.ErrBucketNotFound, nats.ErrNoResponders,
	nats.ErrJetStreamNotEnabled, nats.ErrJetStreamNotEnabledForAccount,
	nats.ErrInvalidKey, nats.ErrMaxPayload,
}
