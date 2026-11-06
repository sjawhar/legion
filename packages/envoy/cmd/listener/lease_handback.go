package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// A clean-hand-back mark is how a pod that stopped gracefully tells the pod that takes its lease
// record that the hand-over went through: that it NAKed everything it held with the hand-back
// delay, released the record itself, saw another pod receiving on the deliver subject before it
// let go, and let go only once the server said its subscription was closed. The next holder's
// settle hold then needs to wait no longer than that hand-back delay, rather than the ack wait a
// message is back at when it reached no subscriber of this machine's.
//
// The mark is its own key in the listener state bucket, `handback.<durable>.<revision>`
// (bus.ListenerHandBackPrefix), named for the lease revision it vouches for: the release the
// stopping pod wrote. A key of its own is what keeps a stop from writing the lease record after
// another pod has taken it, and keying it by revision keeps a mark of one hand-over from being
// read as another's, since revisions never repeat. Nothing removes a mark: the bucket's MaxAge
// (listenerStateMaxAge, 20 minutes) drops it, no pod reads a mark for a revision it did not
// replace, and the next holder reads its own mark once at most per settle poll.
//
// The value names the pod that wrote it and the revision again, so a mark read for the wrong
// revision - which only a bucket that answered another key could give - is not taken for this
// one's.
type handBackMark struct {
	Instance string `json:"instance"`
	Boot     string `json:"boot"`
	Revision uint64 `json:"revision"`
}

// handBackKey is the key of the mark vouching for the lease record released at revision.
func (l *lease) handBackKey(revision uint64) string {
	return l.handBackKeys + strconv.FormatUint(revision, 10)
}

// MarkHandBack writes the clean-hand-back mark for the record this process released, so the pod
// that took that record ends its settle hold at the hand-back delay rather than an ack wait
// (HandBackMarked). The caller vouches for what it alone knows - its linger ended with another
// pod receiving, its drain was confirmed, and the message in hand's handler returned - and the
// lease for what it knows: it writes nothing unless the release of the term being stopped wrote
// a record (a release that outlasted its bound wrote none, and the late release that follows it
// is past the linger; a release an earlier term made is cleared when this one starts,
// startTermLocked) and that record carried no stale binding, in which case what a lost node's
// binding held comes back only at its ack wait. It reports whether the mark was written. ctx
// bounds the write, which is one bucket call; a write that fails is the caller's to log, and
// costs the next holder its ack wait and no more.
func (l *lease) MarkHandBack(ctx context.Context) (bool, error) {
	l.mu.Lock()
	revision, stale := l.released, l.releasedStale
	mark := handBackMark{Instance: l.instance, Boot: l.boot, Revision: revision}
	l.mu.Unlock()
	if revision == 0 || stale {
		return false, nil
	}
	value, err := json.Marshal(mark)
	if err != nil {
		return false, fmt.Errorf("encode the hand-back mark: %w", err)
	}
	key := l.handBackKey(revision)
	if _, _, err := bus.Bounded(ctx, func() (uint64, error) { return l.kv.Put(key, value) }); err != nil {
		return false, fmt.Errorf("write the hand-back mark %s: %w", key, err)
	}
	return true, nil
}

// HandBackMarked reports whether the pod that released the record term was taken from marked that
// hand-over clean (MarkHandBack). It is the one judgement of a hand-back there is: a term that is
// no candidate (leaseStatus.HandBack), or is no longer the current term, is not marked and asks
// the bucket nothing. A read that fails is reported with false, so the caller holds on: a settle
// hold that cannot read the mark waits as long as any other, which is what a message that reached
// no subscriber needs.
func (l *lease) HandBackMarked(ctx context.Context, term uint64) (bool, error) {
	l.mu.Lock()
	candidate, revision := l.term == term && l.handBack, l.replaced
	l.mu.Unlock()
	if !candidate {
		return false, nil
	}
	key := l.handBackKey(revision)
	entry, _, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return l.kv.Get(key) })
	if errors.Is(err, nats.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the hand-back mark %s: %w", key, err)
	}
	var mark handBackMark
	if err := json.Unmarshal(entry.Value(), &mark); err != nil {
		return false, fmt.Errorf("decode the hand-back mark %s: %w", key, err)
	}
	return mark.Revision == revision, nil
}
