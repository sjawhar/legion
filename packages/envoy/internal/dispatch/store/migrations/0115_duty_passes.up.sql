-- INFRA-66: when each leased maintenance loop's pass last began, one row per lease. The lease the
-- loops take (duty.Exclusive) is a session advisory lock, which excludes only passes that overlap:
-- two processes whose tickers are out of phase - the ordinary case, since each draws its phase at
-- boot (duty.RunJittered) and a rolling update starts the second process minutes after the first -
-- never meet inside a pass, so each one ran the whole pass. Measured on two real processes: the
-- GitHub webhook redelivery sweep listed the App webhook's deliveries twice for one sweep window,
-- which is the cost redeliver/sweep.go's own comment says that lease exists to avoid.
--
-- This row is what the second process's tick reads instead: a pass is due only once the loop's own
-- period has run out since the last one began, whichever process ran it, so one period costs one
-- pass however many processes tick. A departure from INFRA-66's plan, which promised "no new NATS
-- subject, durable, consumer or KV bucket, and no new table" (plan §6.2); the architect took it,
-- because a pass another process ran is what has to count, and nothing a process keeps to itself
-- can say so. No existing row is every leased loop's.
--
-- duty.Exclusive alone reads and writes this table, on the connection holding that loop's lease:
-- the decision and the record both sit inside the lease, so no process's tick can read a row the
-- pass it would skip for has not written yet.
--
-- The elapsed time is compared here, in the database (began_at > now() - make_interval(...)), and
-- never in Go: two processes' wall clocks differ by whatever their hosts' do, and the database's
-- clock is the one clock both passes are judged by.
--
-- began_at is when the pass began, though it is written when the pass ends. A loop's ticker
-- measures its interval from the tick that began the pass, so a period measured from the end would
-- be short by that pass's own duration at every tick, and a loop whose period is its ticker's
-- interval - the daily document compaction, the suggestion-outcome sweep's minute - would find its
-- period served at every second tick and run at half the rate it is configured for.
--
-- A new table: nothing existing is read, locked or rewritten here, and the release before this one
-- neither reads nor writes it, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations") - the earlier build keeps taking the lease and
-- running every pass, as it does today, while the newer one skips the periods it finds served.
create table duty_passes (
    lease text primary key,
    began_at timestamptz not null
);
