package cistore

import (
	"context"
	"fmt"
	"slices"
)

// keyCombiner queues one record's pending mutations while one of their callers writes a batch. A
// record has a combiner exactly while a writer is writing it, or while a caller it named the next
// writer has yet to start.
type keyCombiner struct {
	queue []*pendingMutation
}

// pendingMutation is one caller's mutation and the channel that tells it the outcome: the error of
// the write that carried it, or that it now writes the next batch itself. The channel receives at
// most one message, so a send never blocks.
type pendingMutation struct {
	mutate func(*State) bool
	done   chan combineOutcome
}

type combineOutcome struct {
	err  error
	lead bool
}

// update folds mutate into the record of one commit. Every check of a head writes that one record,
// and a check_run burst is many concurrent webhooks, so concurrent calls for a record are combined:
// one caller at a time writes, applying every mutation queued when its write starts, in arrival
// order, in one compare-and-swap, and every caller in that batch returns the write's outcome; the
// first caller that queued during the write then writes the next batch. Written one at a time, N
// concurrent observations cost O(N) rounds in which every writer decodes, hashes and re-encodes the
// whole record and only one wins, and the last ones run out of the budget. Each mutation's own
// rules still decide whether it applies, so a batch leaves the record as the same mutations written
// one at a time in that order would, except that generation advances once for the write that
// changed the record rather than once per observation: it still strictly increases whenever the
// snapshot changes.
//
// A caller waits within ctx and recordWait, whichever ends first: in the queue (leave), and for the
// KV calls of the batch it writes (write), so every caller in a batch shares the bound of the
// caller writing it.
func (s *Store) update(ctx context.Context, owner, repo, number, sha string, mutate func(*State) bool) error {
	ctx, cancel := context.WithTimeout(ctx, recordWait)
	defer cancel()
	key := Key(owner, repo, number, sha)
	own := &pendingMutation{mutate: mutate, done: make(chan combineOutcome, 1)}
	s.combineMu.Lock()
	c := s.combiners[key]
	writer := c == nil
	if writer {
		c = &keyCombiner{}
		s.combiners[key] = c
	}
	c.queue = append(c.queue, own)
	s.combineMu.Unlock()
	if !writer {
		var outcome combineOutcome
		select {
		case outcome = <-own.done:
		case <-ctx.Done():
			return s.leave(key, c, own, ctx.Err())
		}
		if !outcome.lead {
			return outcome.err
		}
		// Named the next writer as ctx ended: a write under an ended ctx would fail the whole batch.
		if err := ctx.Err(); err != nil {
			s.combineMu.Lock()
			s.passLeadLocked(key, c, own)
			s.combineMu.Unlock()
			return err
		}
	}
	return s.writeBatch(ctx, key, c, own, State{Owner: owner, Repo: repo, Number: number, SHA: sha})
}

// leave ends the wait of a queued caller whose ctx ended, returning cause unless the write that
// carried its mutation had already answered. Under combineMu it finds its caller in one of three
// places. Answered: the write's error is returned. Named the next writer: the role goes to the next
// caller queued, or with none the combiner goes, so no caller waits on a writer that never starts.
// Still queued: it leaves the queue, so no write carries the mutation. Otherwise the mutation is in
// the batch being written now, which may still land it after cause is returned (write says why
// that is safe).
func (s *Store) leave(key string, c *keyCombiner, own *pendingMutation, cause error) error {
	s.combineMu.Lock()
	defer s.combineMu.Unlock()
	select {
	case outcome := <-own.done:
		if !outcome.lead {
			return outcome.err
		}
		s.passLeadLocked(key, c, own)
		return cause
	default:
	}
	c.queue = slices.DeleteFunc(c.queue, func(p *pendingMutation) bool { return p == own })
	return cause
}

// passLeadLocked takes own, named the next writer, out of the queue and names the caller after it
// instead (handOverLocked). combineMu must be held.
func (s *Store) passLeadLocked(key string, c *keyCombiner, own *pendingMutation) {
	c.queue = slices.DeleteFunc(c.queue, func(p *pendingMutation) bool { return p == own })
	s.handOverLocked(key, c)
}

// handOverLocked names the first queued caller the next writer, or removes the record's combiner
// when none is queued. combineMu must be held.
func (s *Store) handOverLocked(key string, c *keyCombiner) {
	if len(c.queue) > 0 {
		c.queue[0].done <- combineOutcome{lead: true}
		return
	}
	delete(s.combiners, key)
}

// writeBatch writes every mutation queued for the record in one compare-and-swap within ctx, tells
// each queued caller the outcome, and hands the writer's role to the first caller that queued
// since. It reports and hands over even when the write panics, so no queued caller waits forever.
func (s *Store) writeBatch(ctx context.Context, key string, c *keyCombiner, own *pendingMutation, identity State) (err error) {
	s.combineMu.Lock()
	batch := c.queue
	c.queue = nil
	s.combineMu.Unlock()
	defer func() {
		recovered := recover()
		if recovered != nil {
			err = fmt.Errorf("cistore: record write panicked: %v", recovered)
		}
		s.combineMu.Lock()
		for _, pending := range batch {
			if pending != own {
				pending.done <- combineOutcome{err: err}
			}
		}
		s.handOverLocked(key, c)
		s.combineMu.Unlock()
		if recovered != nil {
			panic(recovered)
		}
	}()
	return s.write(ctx, identity, len(batch), func(st *State) bool {
		changed := false
		for _, pending := range batch {
			if pending.mutate(st) {
				changed = true
			}
		}
		return changed
	})
}
