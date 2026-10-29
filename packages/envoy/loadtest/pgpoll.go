package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// pollErrorsLogged is how many failed reads of one probe run.log shows; the summary counts them
// all.
const pollErrorsLogged = 3

// readErrors is the error policy of a probe's reads: every failed read is counted, the first
// pollErrorsLogged are logged, prefixed with name, and done logs the total past that.
type readErrors struct {
	name string
	lg   *log.Logger
	n    int
}

// add counts a failed read, and logs it when it is one of the first pollErrorsLogged.
func (e *readErrors) add(err error) {
	if e.n++; e.n <= pollErrorsLogged {
		e.lg.Printf("%s: %v", e.name, err)
	}
}

// done logs the total when more reads failed than were logged, and returns it.
func (e *readErrors) done() int {
	if e.n > pollErrorsLogged {
		e.lg.Printf("%s: %d reads failed in all", e.name, e.n)
	}
	return e.n
}

// pollPG reads Postgres over one direct connection: it calls sample at from, then every `every`
// up to and including to (poll). It returns the failed reads: a failed connect counts as one and
// is logged, prefixed with name.
func pollPG(ctx context.Context, url string, from, to time.Time, every time.Duration, name string, lg *log.Logger, sample func(context.Context, *pgx.Conn) error) int {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		lg.Printf("%s: %v", name, err)
		return 1
	}
	defer conn.Close(context.Background())
	return poll(ctx, from, to, every, name, lg, func(ctx context.Context) error { return sample(ctx, conn) })
}

// poll calls sample at from, then at from + k·every while that is not after to. A slot that a slow
// sample overran is skipped, except the latest one, which runs at once, as a time.Ticker drops
// ticks. A failed sample is counted and logged as readErrors does; an error once ctx has ended
// stops the poll uncounted. It returns the failed samples.
func poll(ctx context.Context, from, to time.Time, every time.Duration, name string, lg *log.Logger, sample func(context.Context) error) int {
	errs := readErrors{name: name, lg: lg}
	for next := from; !next.After(to); {
		if !sleepUntil(ctx, next) {
			break
		}
		if err := sample(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			errs.add(err)
		}
		next = next.Add(every)
		if behind := time.Since(next); behind > 0 {
			next = next.Add(behind.Truncate(every))
		}
	}
	return errs.done()
}
