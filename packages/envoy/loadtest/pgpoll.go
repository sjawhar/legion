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

// pollPG reads Postgres over one direct connection: it calls sample at from, then every `every`
// up to and including to (poll). It returns the failed reads: a failed connect counts as one. The
// first pollErrorsLogged failures are logged, prefixed with name.
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
// ticks. A failed sample is counted and the first pollErrorsLogged are logged; an error once ctx
// has ended stops the poll uncounted. It returns the failed samples.
func poll(ctx context.Context, from, to time.Time, every time.Duration, name string, lg *log.Logger, sample func(context.Context) error) int {
	errs := 0
	defer func() {
		if errs > pollErrorsLogged {
			lg.Printf("%s: %d reads failed in all", name, errs)
		}
	}()
	for next := from; !next.After(to); {
		if !sleepUntil(ctx, next) {
			return errs
		}
		if err := sample(ctx); err != nil {
			if ctx.Err() != nil {
				return errs
			}
			if errs++; errs <= pollErrorsLogged {
				lg.Printf("%s: %v", name, err)
			}
		}
		next = next.Add(every)
		if behind := time.Since(next); behind > 0 {
			next = next.Add(behind.Truncate(every))
		}
	}
	return errs
}
