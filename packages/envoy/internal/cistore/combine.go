package cistore

import "fmt"

// keyCombiner queues one record's pending mutations while one of their callers writes a batch. A
// record has a combiner exactly while a writer is writing it.
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
func (s *Store) update(owner, repo, number, sha string, mutate func(*State) bool) error {
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
		if outcome := <-own.done; !outcome.lead {
			return outcome.err
		}
	}
	return s.writeBatch(key, c, own, State{Owner: owner, Repo: repo, Number: number, SHA: sha})
}

// writeBatch writes every mutation queued for the record in one compare-and-swap, tells each
// queued caller the outcome, and hands the writer's role to the first caller that queued since.
// It reports and hands over even when the write panics, so no queued caller waits forever.
func (s *Store) writeBatch(key string, c *keyCombiner, own *pendingMutation, identity State) (err error) {
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
		if len(c.queue) > 0 {
			c.queue[0].done <- combineOutcome{lead: true}
		} else {
			delete(s.combiners, key)
		}
		s.combineMu.Unlock()
		if recovered != nil {
			panic(recovered)
		}
	}()
	return s.write(identity, func(st *State) bool {
		changed := false
		for _, pending := range batch {
			if pending.mutate(st) {
				changed = true
			}
		}
		return changed
	})
}
