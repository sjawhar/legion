# Dispatch load test (`loadtest/`)

This package reproduces the load of the 2026-10-26 Dispatch outage on one 2-core machine. It runs
a local stack (Postgres, a latency proxy, NATS, the Envoy listener and Dispatch, all built from this
checkout), puts a measured load on it and writes tables of what happened.

- The outage-load reproduction report of 2026-10-27 (the root report, below), which ran on a
  2-vCPU pod, is the source of the tables' shape. This harness is the committed form of the
  harness that report used.

The harness measures. It changes no Dispatch, listener or outbox code. A fix of the outage runs the
same step before and after its change, and compares the two rows.

## Files

| File | What it is |
| --- | --- |
| `main.go` | The `loadgen` binary: subcommands `proxy`, `seed`, `seed-refs`, `refs-shape`, `refs-fetch`, `run`, `tables`, `series-sessions`, `trigger-sql`, `box-busy` |
| `api.go` | The Dispatch HTTP client, with the request trace that gives the arrival time |
| `seed.go` | The seed: 4 projects, 1,000 issues and 8,000 comments through Dispatch's API, and `seed.json` |
| `refs.go` | The reference seed: the plan of 1,000 cited documents and their citations, the writes, the shape check (`refs-shape`) and the answer fetch (`refs-fetch`) |
| `run.go` | One step: its phases (the window, the dashboard live sockets, the event and request generators on one open-loop schedule, the probes) and `summary.json` |
| `sessions.go` | The simulated sessions: their NATS subscriptions and receipts, their listener registrations and heartbeats |
| `events.go` | The five write kinds, the event mix and each write's plan |
| `marker.go` | The write marker `lgm<nonce><counter>x`: its format, the run's nonce and the parse that keeps one run's markers |
| `routes.go` | The route-mix file parser and the request paths |
| `delivery.go` | The join of writes, database commits and receipts into delivery latencies |
| `sample.go` | The 5 s sampler: CPU, RSS, Postgres connections, goroutines, pool waiters, heap, NATS `/varz`. The transaction-id probe. |
| `lockprobe.go` | The 20 ms sampler of the commit-order lock |
| `locksampler.go` | The 1 s sampler of every ungranted lock in `pg_locks` |
| `xactsampler.go` | The 10 ms sampler of `pg_stat_activity` that times how long each Dispatch transaction holds a transaction id (`-xact-interval`) |
| `pgpoll.go` | The poll loop of the lock probes and the transaction-id hold sampler (`pollPG`), and the error policy of the probes' reads, which the 5 s sampler shares: failed reads are counted, the first three are logged, and so is their total past three |
| `live.go` | The dashboard live sockets' reader of `GET /ws/live` event frames, and the live frame latencies |
| `proxy.go` | The TCP latency proxy toward Postgres |
| `tables.go` | `TABLES.md` from one or more `summary.json` files, the pprof tops and the goroutine summary |
| `guard.go` | `stack.json`, the endpoint flags and the check that every endpoint is the stack's own |
| `util.go` | Helpers that several files share: the sleep, the profile fetch, the JSON file write and the collector of a step's records |
| `commit-trigger.sql` | The commit-time trigger for the delivery latency from commit |
| `refs-sample-2026-10-27.json` | The reference seed's input: for each of 160 production issues sampled on 2026-10-27, the version count of every member of its references answer. 160 arrays of numbers, no issue key and no name. |
| `routes-2026-10-26-1719Z.txt` | The default route mix |
| `stack.sh` | The local stack: `setup`, `start`, `seed`, `reset`, `stop`, `status`, `build-info`, `loadgen` |
| `run-step.sh` | The one command for a named step |
| `run-series.sh` | The series of six steps, 0.25x to 10x |
| `common.sh` | What the three scripts share, sourced by each: the `LOADTEST_ROOT` and `LOADTEST_SRC` paths, the run lock (One run at a time, below), and the setup, start, seed and migration check before a step |
| `*_test.go` | Unit tests. They need no Postgres and no NATS: `cd packages/envoy && go test ./loadtest/` |
| `run-step.test.sh` | The test of the run lock. It fakes `go` and `uv` and needs no stack: `cd packages/envoy && bash loadtest/run-step.test.sh` |
| `common.test.sh` | The test of `lock_root`: a series step keeps the lock after its parent dies, a refusal names the live holder, a token without the holder's descriptor is refused, a lock held by a process that this PID namespace cannot see is refused as held (never as free), a caller takes the lock once its holder has ended, two callers waiting at once never name each other as the holder, and a record of another PID namespace whose pid runs here is refused without naming that pid. It needs no stack: `cd packages/envoy && bash loadtest/common.test.sh` |

## Requirements

- A Linux machine with 2 vCPUs, the shape of a Legion issue pod. The root report ran on such a
  pod: `nproc` 2, 19 GiB of memory, a gVisor sandbox. The samplers read `/proc`, so the harness
  runs on Linux only.
- No other heavy work on the machine during a step. `meta.json` records `box_busy_before`: the
  share of the box's CPU time that was busy over 5 s just before the step, from two `/proc/stat`
  reads. `run-step.sh` takes it before its reset, while Dispatch is stopped and the rest of the
  stack is idle (`loadgen box-busy`), so Dispatch's own start-up work is not in it; an idle box
  reads 0%. When it is above 10%, `run.log` warns that another process on the machine uses CPU. The
  load average proves nothing here: on gVisor `/proc/loadavg` always reads 0.00. Each Legion role
  runs in its own PID namespace, so the processes of other roles in the same pod are invisible to
  `ps` and to the sampler, but their CPU shows in this number.
- One run at a time on a `LOADTEST_ROOT` (One run at a time, below).
- The Go toolchain (`go`). `stack.sh setup` builds the binaries and installs `nats-server` with it.
  `loadgen tables` uses `go tool pprof`.
- `uv`. `stack.sh setup` installs Python 3.12 and the `pgserver` wheel (0.1.4) with it. The wheel
  holds Postgres 16.2 and pgvector. The harness uses no Docker.
- `jq`, `curl` and `flock` (util-linux).
- `git` is optional. Without it, `meta.json` and `seed.meta.json` record the build commit as
  `unknown`.
- Disk space under `LOADTEST_ROOT`. In the measured runs, each step directory held 20-25 MB, most
  of it `heap.txt`, and the Postgres data directory held about 520 MB: the seed template and the
  step's copy of it.

### Environment

| Variable | Default | What it sets |
| --- | --- | --- |
| `LOADTEST_ROOT` | `${TMPDIR:-/tmp}/dispatch-loadtest` | The directory that holds all the stack's state, binaries and step output. On a Legion pod, `$TMPDIR` is `/legion/scratch/tmp`, the issue's volume. |
| `LOADTEST_SRC` | The checkout that holds the script | The checkout that `stack.sh setup` builds `envoy-dispatch`, `envoy-listener` and `loadgen` from. |
| `LOADTEST_PG_PORT` | `15432` | The Postgres port. |
| `LOADTEST_PROXY_PORT` | `15433` | The latency proxy port. |
| `LOADTEST_NATS_PORT` | `14222` | The NATS client port. |
| `LOADTEST_NATS_MONITOR_PORT` | `18222` | The NATS monitoring port (`/varz`). |
| `LOADTEST_LISTENER_PORT` | `19120` | The Envoy listener port. |
| `LOADTEST_DISPATCH_PORT` | `18777` | The Dispatch HTTP port. |
| `LOADTEST_PPROF_PORT` | `16061` | The Dispatch pprof port (`DISPATCH_PPROF_PORT`). |
| `LOADTEST_PROXY_RTT` | `1ms` | The round trip that the proxy adds, as a Go duration. Read when the proxy starts. |
| `LOADTEST_PG_DIRECT` | unset | `1` connects Dispatch to Postgres directly, without the proxy. Read when Dispatch starts, so set it on `run-step.sh`. `stack.json` and `meta.json` record it as `pg_direct`. |

Every process listens on `127.0.0.1`. `stack.sh setup` writes the ports into
`$LOADTEST_ROOT/stack.json`. Each `setup` reads the port variables again, and `run-step.sh` and
`run-series.sh` call `setup` each time. Thus keep the same `LOADTEST_*_PORT` values in the
environment of every command. `setup` refuses to change a port while a stack process runs.
`start` refuses a port that another process holds, and names the variable that moves it.

Every stack process starts under `env -i` with an explicit environment. The `DISPATCH_URL`,
`ENVOY_URL`, `ENVOY_NATS_URL`, `AWS_*`, `HOME` and `XDG_*` of your shell never reach the stack or
`loadgen`. The stack's `HOME` is `$LOADTEST_ROOT/home`. Dispatch keeps its signing key there, so
the key stays the same across steps. `stack.sh` stops each process by the pid in its pidfile
(`$LOADTEST_ROOT/run/<name>.pid`), never by name.

## Run one step

```bash
packages/envoy/loadtest/run-step.sh <name> --events <per s> --sessions <n> --requests <per s> \
  [--duration 180s] [--warmup 30s] [--grace 20s] [--event-mix <preset or kind=w,...>] \
  [--route-mix <file>] [--human 0.1] [--live <n>] [--rng-seed 1] [--xact-interval 10ms] [--out <dir>]
```

Example, the 1x step of the root report:

```bash
packages/envoy/loadtest/run-step.sh step-1x --events 4 --sessions 400 --requests 60
```

The step name comes first. It can hold only letters, digits, `.`, `_` and `-`. Every flag takes a
value.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--events` | required | Writes per second. `0` sends no writes. |
| `--sessions` | required | Simulated sessions, 1 or more. |
| `--requests` | required | GET requests per second. `0` sends no requests. |
| `--duration` | `180s` | The measured window. More than 0. |
| `--warmup` | `30s` | Load before the window. Nothing in it is counted. |
| `--grace` | `20s` | The wait for late receipts after the generators stop. |
| `--event-mix` | `production-2026-10-26` | A preset name or `kind=weight,...` (Event mix, below). |
| `--route-mix` | the committed `routes-2026-10-26-1719Z.txt` | A route-mix file (Route mix, below). It must be a file. |
| `--human` | `0.1` | The share of writes that a person makes, 0 to 1. |
| `--live` | `max(1, round(requests/30))` | Dashboard live sockets (`GET /ws/live`, each subscribed to the event feed as a dashboard tab is): 2 at 60 req/s, 20 at 600 req/s. |
| `--rng-seed` | `1` | The seed of the event and request schedules. |
| `--xact-interval` | `10ms` | The interval of the transaction-id hold sampler (Transaction-id holds, below). `0` turns it off, for a control step without its reads. |
| `--out` | `$LOADTEST_ROOT/runs/<name>` | The step directory. It must not exist, or it must be empty. |

Rates are used as given. `0` is a real zero. The event and request schedules are open-loop Poisson
from the RNG seed, so two runs of one step with one seed offer the same arrivals.

`run-step.sh` does these steps in order:

1. `stack.sh setup`: it builds the binaries from the checkout (incremental), so a step always runs
   the checkout's current code.
2. `stack.sh start`, then `stack.sh seed`. Each one does nothing when its work is already done. So
   `run-step.sh` is the one command from a fresh checkout. On the first run, the seed alone takes
   37 to 50 minutes.
3. The migration check (Before and after across commits, below).
4. `stack.sh reset`, with Dispatch's log in the step directory (The reset, below).
5. `loadgen run`. It writes the step's files.
6. It stops Dispatch. It also stops Dispatch when the step fails. Postgres, the proxy, NATS and the
   listener keep running. `stack.sh stop` stops them.
7. It copies the listener and NATS logs into the step directory and runs `loadgen tables` there.

A step of a series skips steps 1 to 3: `run-series.sh` did them once, before its first step (Run
the series, below).

Before step 1, `run-step.sh` takes the run lock of the root (One run at a time, below).

**A step whose stack process exits fails.** The sampler checks every stack process (Dispatch, the
listener, NATS, the proxy and Postgres) at each 5 s sample, and once more after the grace. A
process is alive while `/proc/<pid>` exists and is not a zombie. When one has exited,
`loadgen run` still writes `summary.json`, with `stack_exits` (each process's name, pid and the
time it was first seen gone), and the other files that it can take. Then it exits with status 1:
`loadgen run: dispatch (pid N) exited during the step (first seen gone at T); this row does not
measure the load`. `run-step.sh` renders `TABLES.md` for diagnosis, with the mark
`(dispatch exited)` after the step name, and exits with status 1, pointing at `loadgen run`'s
message and at `run.log`. A Dispatch crash or out-of-memory kill at a high step thus never reads as
the overload that the series measures.

**A step whose commit rows cannot be read fails.** The committed counts, the receipts and the
delivery latencies all come from the run's rows of `loadtest.event_commits` (Delivery latency,
below). When `loadgen` cannot read them, because the database has no `commit-trigger.sql` or
Postgres does not answer, it still writes `summary.json` with `events.commits_error` (why the read
failed), `meta.json` and `samples.json`. Then it exits with status 1, naming the commit-row read and
`events.commits_error`. `run-step.sh` renders `TABLES.md` for diagnosis, where every cell that
depends on those rows shows `-` and the step name carries the mark `(commit rows not read)` (The
tables, below), and exits with status 1; `run-series.sh` stops there. The other reads of the
database (the lock probe, the ungranted-lock sampler, the 5 s sampler's `pg_stat_activity`, the
transaction-id probe and the Postgres version in `meta.json`) feed none of those columns. A failure
of one of them marks its own table, or `meta.json`, and the step does not fail.

To run a step name again, remove its step directory first.

`loadgen run` also takes `-nats-conns` (default 50), the NATS connections that the sessions share.
`run-step.sh` does not pass it.

### One run at a time

A step resets the stack and measures the whole box, so one `LOADTEST_ROOT` runs one step or one
series at a time. A second `run-step.sh` on the root of a running step would stop that step's
Dispatch and listener in its reset, and the first step would finish as a row of connection
failures.

`run-step.sh` and `run-series.sh` take an exclusive `flock` on `$LOADTEST_ROOT/.run.lock`, kept on
file descriptor 9, before they touch the stack. The lock lasts until every process that holds that
descriptor has exited: the holder and the children that inherit it. Once locked, the holder empties
the lock file and writes its pid, its PID namespace (`/proc/self/ns/pid`), command, start time and
a random token into it. Another run on the root that sees a live holder is refused at once. While
it sees none, it tries the lock for up to 3 s, on descriptor 8 so that a waiting run never holds
descriptor 9 on the file, and takes it when the lock is freed in that time. These `stack.sh`
commands are refused the same way: `setup`, `start`, `seed`, `reset` and `stop`; `stack.sh
status`, `build-info` and `loadgen` take no lock. A refusal exits with status 1 and names the root
and the holder that it can find:

- the pid in the lock file, when that pid runs in the refused caller's PID namespace. The pid is
  trusted only when the lock file names that namespace;
- else, when that run has exited and its children still hold the lock, those processes;
- else the pid, start time and command that the lock file records for that run. Then the holder
  is not visible in this PID namespace: a run in another PID namespace, such as another Legion
  role in this pod on a shared `LOADTEST_ROOT`. A pid that the lock file records for another PID
  namespace is written as `pid=N in PID namespace pid:[M]`, since pid N here is an unrelated
  process. The refusal says not to remove `.run.lock`: that run still holds the lock, and a run
  that locked a new file would reset the stack under it.

The holder's own children pass: the holder exports the token as `LOADTEST_LOCK_TOKEN`, and a caller
whose token equals the lock file's and whose descriptor 9 is the holder's locked one keeps that
descriptor and goes on without locking. Thus the series' own `run-step.sh` calls, and the
`stack.sh` calls of a run, are not refused, and a series step, with the `loadgen run` that it
starts, keeps the root when `run-series.sh` is killed. A caller with the token but without the
descriptor is refused. No stack process holds the lock: `stack.sh` starts each one with
descriptor 9 closed.

## Run the series

```bash
packages/envoy/loadtest/run-series.sh [step flags]
```

The series runs six steps with `run-step.sh`, in order, each from a fresh reset. It writes them
into `$LOADTEST_ROOT/runs/series-<UTC time>/step-<f>x/`, and one `TABLES.md` across them in the
series directory.

| step | events/s | sessions | requests/s | live sockets |
| --- | --- | --- | --- | --- |
| step-0.25x | 1 | 100 | 15 | 1 |
| step-0.5x | 2 | 200 | 30 | 1 |
| step-1x | 4 | 400 | 60 | 2 |
| step-2x | 8 | 467 | 120 | 4 |
| step-5x | 20 | 667 | 300 | 10 |
| step-10x | 40 | 1,000 | 600 | 20 |

- Events are 4/s and requests 60/s at 1x, multiplied by the load factor f.
- Live sockets are `max(1, round(2f))`.
- Sessions are piecewise linear through the spec's three points: 100 at 0.25x, 400 at 1x and
  1,000 at 10x. That is 400·f up to 1x, and 400 + 600·(f − 1)/9 above it.
  `loadgen series-sessions <f>` prints the value.
- The series passes every other step flag to each step: `--duration`, `--warmup`, `--grace`,
  `--event-mix`, `--route-mix`, `--human`, `--rng-seed`, `--xact-interval`. It refuses `--events`,
  `--sessions`, `--requests`, `--live` and `--out`, because it sets them.
- With the default window, a step takes about 5 minutes and a series about 30 minutes.
- `run-series.sh` runs the setup (one build of the checkout), the start, the seed and the migration
  check once, before the series directory exists, and reads the build's commit and dirty flag once
  (`stack.sh build-info`). It passes them to its steps in `LOADTEST_SERIES_BUILD`. Its steps skip
  the setup and record that build in `meta.json`, so every step of a series runs one build, even
  when the checkout changes during the series. A `run-step.sh` that does not hold the series' lock
  ignores `LOADTEST_SERIES_BUILD` and builds the checkout.
- The series holds the run lock for its whole run.
- A failed step stops the series. `run-series.sh` still writes the series `TABLES.md`, over the
  steps that ran (the failed step included when it wrote `summary.json`), and exits with status 1,
  naming the step.

## The stack (`stack.sh`)

| Command | What it does |
| --- | --- |
| `stack.sh setup` | Builds `envoy-dispatch`, `envoy-listener` and `loadgen` into `$LOADTEST_ROOT/bin`. Installs `nats-server` at the version that `packages/envoy/go.mod` pins, with `go install`. Installs Postgres with pgvector from the `pgserver` wheel. Runs `initdb`. Writes `stack.json`. Each run does it again, incrementally. |
| `stack.sh start` | Starts Postgres, the proxy, NATS and the listener. It keeps a process that already runs. It refuses a port that another process holds, and names it. Dispatch starts only in `seed` and `reset`. |
| `stack.sh seed [--force]` | Makes the seed template `dispatch_seed` (The seed, below). It does nothing when `dispatch_seed` and `seed.meta.json` exist. `--force` drops the old seed and seeds again. |
| `stack.sh reset [LOG]` | Resets the stack to the seed and starts Dispatch, with its log in `LOG` (default `$LOADTEST_ROOT/logs/dispatch.log`). `run-step.sh` does this before each step. |
| `stack.sh stop [NAME...]` | Stops `dispatch`, `listener`, `nats`, `proxy` and `postgres`, or only the named ones. |
| `stack.sh status` | Prints each process's pid and ports, and the seed's migration, build commit and counts. |
| `stack.sh build-info` | Prints the checkout's commit and `true` or `false` for uncommitted changes in its tree, as `<commit> <dirty>`. `run-step.sh` records them in `meta.json`. |
| `stack.sh loadgen ARGS...` | Runs `$LOADTEST_ROOT/bin/loadgen` with the stack's environment, from the root. `run-step.sh` and `run-series.sh` call `loadgen` this way. |

`setup`, `start`, `seed`, `reset` and `stop` take the run lock for their run, and are refused
while another run holds it (One run at a time, above).

The processes:

- **Postgres** 16.2 with pgvector, from the `pgserver` wheel. `initdb` adds the root report's
  settings: `max_connections 800`, `shared_buffers 1GB`, `fsync on`,
  `log_min_duration_statement 50ms`, `log_lock_waits on`. It also adds
  `track_commit_timestamp = on`, for the delivery latency from commit, and
  `unix_socket_directories = ''`, for TCP only. Its logs are in `$LOADTEST_ROOT/pg/log`.
- **The latency proxy**, `loadgen proxy -rtt 1ms`. It holds each chunk for half the round trip in
  each direction. Dispatch reaches Postgres through it. In the root report, `-rtt 1ms` added about
  4 ms per round trip: `select 1` took 4.18 ms p50 through the proxy and 0.13 ms directly. A short
  sleep in gVisor oversleeps.
- **NATS** with JetStream: `nats-server -js -a 127.0.0.1 -n <stack name> -m <monitor port>`, on a
  fresh store directory at each start.
- **The listener**, with `ENVOY_MACHINE_ID=<stack name>` and a random API token.
- **Dispatch**, with a random agent token and header identity
  (`DISPATCH_IDENTITY=header:X-Dispatch-User`, `DISPATCH_IDENTITY_HEADER_TRUSTED=1`). It reaches
  the stack's listener (`ENVOY_URL`) and NATS. It gets no `AWS_*` variables, so it calls no
  embedding model. `DISPATCH_WEB_DIST` is an empty directory.

`stack.json` holds the stack name (`loadtest-<random>`), the two random tokens, every port and
every pid. It is mode 600. `loadgen` reads every endpoint and token from it.

## The seed

`stack.sh seed` makes the database that every step starts from. It takes 37 to 50 minutes on a
2-vCPU pod: 14 to 27 minutes for the issues and comments with the wait for the block index
(1,086 s and 1,600 s in two seeds on 2026-10-27, 14 minutes on 2026-10-29), and about 23 minutes
more for the references (2,232 s for the whole seed on 2026-10-29). It does these steps:

1. It creates database `dispatch` and starts Dispatch (log: `$LOADTEST_ROOT/logs/dispatch-seed.log`).
2. `loadgen seed` writes through Dispatch's API with 8 writers (log: `$LOADTEST_ROOT/logs/seed.log`):
   - 4 projects, `LGA`, `LGB`, `LGC` and `LGD`, with 400, 300, 200 and 100 issues.
   - One spec (`spec.md`) per issue. Every fifth issue (20%) gets a 40-50 KB spec with 18-row
     tables. The others get 4-12 KB specs with 4-row tables. Each spec has one `Revision token r0`
     line. Each table row has a unique cell `Requirement <project>-<n>-R<row> holds`, which
     anchored comments quote.
   - 8 comments per issue. Every second comment is anchored to a requirement cell. So each issue
     has 4 anchored and 4 plain root comments.
   - The route of each issue, in key order: issue i goes to session `ses-lg-<i>` (4 digits). Thus
     no step spends its warm-up on route changes.
3. `loadgen seed-refs` adds the `dispatch://` references, with 8 writers (log:
   `$LOADTEST_ROOT/logs/seed-refs.log`), so `GET /api/v1/issues/{key}/references` does the work it
   does in production:
   - 1,000 target documents, `ref-target-<n>.md`, on the 600 issues of `LGB`, `LGC` and `LGD`:
     target n on the n mod 600-th of them, in key order. No step writes a `ref-target-*` document,
     so the targets stay as the seed wrote them.
   - 19,471 versions in all. Each target's version count is drawn from
     `refs-sample-2026-10-27.json`, the per-member version counts of 160 production issues'
     references answers on 2026-10-27. Each target has 1 to 3 writers from a pool of 600, and the
     first of them creates it.
   - Each writer is a session actor whose origin is padded, so its one-actor author list encodes
     to 231 bytes, production's mean per version row.
   - One `references.md` per `LGA` issue i, in key order. It cites as many targets as the sampled
     issue at position i mod 160 had members, each as
     `dispatch://<KEY>/artifact/ref-target-<n>-md`. 310 of the 400 issues get one; the other 90
     cite nothing and get none.
   - The whole plan comes from one generator seeded 650, so every seed writes the same documents
     with the same writers. `refs_test.go` pins it.
4. It waits until the outbox publishes every seed event (at most 900 s). Then it waits until
   Dispatch's block-index backfill owes nothing (`document_block_versions` with a null version and
   `block_grammar_owed` both empty, at most 900 s; a build older than migration 0121 has neither
   table and skips this wait). Then it stops Dispatch. A seed that still owed the backfill would
   give every step about 100 s of it in its window: Dispatch indexes 20 documents every 2 s.
5. It deletes the idempotency keys of the reference writes (`request_keys`), which protect
   retries during the seed only, and checks that none is left. Left in the template, Dispatch's
   sweep of keys older than a day would delete about 19,800 rows inside the window of every step on
   a seed older than that. Then it runs `vacuum analyze` and installs the commit-time trigger
   (`loadgen trigger-sql`).
6. It writes `seed.meta.json` and renames the database to `dispatch_seed`, the template.

The seed content does not depend on the clock. Each issue's text comes from a generator seeded by
its project and number.

`$LOADTEST_ROOT/seed.json` is what `loadgen run` reads: the project keys and, per issue, its key,
project, `artifact_id` (the spec), `spec_name`, `spec_bytes`, `quotes` (the requirement cells),
`anchored_comment_ids`, `plain_comment_ids` and `route`. `loadgen seed-refs` reads the same file
for the issue keys.

`$LOADTEST_ROOT/seed.meta.json` records `migration` (the highest Dispatch migration applied),
`build_commit`, `build_dirty`, `created_at`, `seed_seconds`, `postgres_version` and `counts`:
projects, issues, comments, artifacts, artifact versions, `ref_targets` (the artifacts named like
`ref-target-%`), `ref_target_versions` (their versions), `ref_sources` (the artifacts named
`references.md`), `artifact_refs` (the rows of `refs` that point at an artifact) and events. The
reference plan writes 1,000 targets, 19,471 target versions and 310 source documents. Before the
references, one seed counted 4 projects, 1,000 issues, 8,000 comments, 1,000 artifacts, 1,000
artifact versions and 10,004 events.

### Read the references back

Two commands read what the reference seed wrote. Each needs Dispatch running, so each follows a
reset:

```bash
packages/envoy/loadtest/stack.sh reset
packages/envoy/loadtest/stack.sh loadgen refs-shape
packages/envoy/loadtest/stack.sh loadgen refs-fetch -out $LOADTEST_ROOT/references.tsv
```

`refs-shape` reads every `LGA` issue's references answer, with no query, and prints the members,
version rows and response bytes per issue (p50, p90, max, mean), the share of author lists that
differ and the author bytes per row. It passes when the mean version rows are within 25% of
production's 3,187, the distinct author-list share is in [0.03, 0.12], and the author bytes per
row are within 25% of 231. It prints `PASS` or `FAIL` and exits 1 on a failure.

`refs-fetch` writes one TSV row per seeded issue, in key order: key, status, ETag, body sha256 and
bytes. Two builds' answers compare row by row.

## The reset

Every step starts from the same state, so the before and after runs of a fix start equal.
`stack.sh reset` does these steps:

1. It stops Dispatch and the listener.
2. It drops database `dispatch` and creates it again from the template `dispatch_seed`.
3. It restarts NATS on a fresh JetStream store, then the listener.
4. It starts Dispatch on the step's build and waits up to 120 s until `/healthz` reports `ok` and
   `nats`. If not, it prints the end of the log and exits with status 1.

The generator's revision tokens start at `r0`, because the database is at the seed again.

## Before and after across commits

```bash
packages/envoy/loadtest/stack.sh setup         # build this checkout's Dispatch, listener and loadgen
packages/envoy/loadtest/stack.sh start
packages/envoy/loadtest/stack.sh seed --force  # seed with a build that has loadgen seed-refs
LOADTEST_SRC=<older checkout> packages/envoy/loadtest/run-step.sh step-1x-before \
  --events 4 --sessions 400 --requests 60
packages/envoy/loadtest/run-step.sh step-1x-after --events 4 --sessions 400 --requests 60
```

Seed with a checkout that has `loadgen seed-refs`. The references are part of the seed, and a
loadgen older than them cannot write them: `stack.sh seed` asks that loadgen for `seed-refs -h` and refuses
to drop the template, naming the cause. The references add no Dispatch migration, so a seed with
them sits at the migration level a seed without them sat at, and an older build's steps run on it.

`stack.sh seed` builds nothing. It uses the binaries of the last `setup`, so run `setup` with the
checkout that seeds first. `setup` builds `loadgen` from `LOADTEST_SRC` too, so that checkout must
hold this package.

`run-step.sh` with another `LOADTEST_SRC` prepares the stack for that checkout: it runs `setup`,
which builds that checkout's Dispatch, listener and loadgen into `$LOADTEST_ROOT/bin` over the ones
the seed used, `start` and `seed`, which does nothing while the template and `seed.meta.json` exist.
Then it compares the highest migration file in that checkout
(`packages/envoy/internal/dispatch/store/migrations`) with `seed.meta.json`. It refuses a build
that is older than the seed, and names both levels; a build at the seed's level runs. For a build
that is newer than the seed, it prints a note: a backfill that the new migrations start runs inside
each step of that build. For example, a build at migration 0128 on a seed at 0120 indexes the
blocks of all 1,000 specs in every step. A newer build migrates the restored copy forward when
Dispatch starts; an older build cannot start on a seed that a newer build made. To measure one
build alone, seed with it (`stack.sh seed --force`, with a checkout that has `seed-refs`).
`meta.json` records the build commit and whether the tree had uncommitted changes.

Committed rate at 1x varies from run to run. Across the runs of 2026-10-27 it was 0.34 to 2.14
events/s. The lowest ran a build at migration 0128 on a seed at 0120, with the block-index backfill
inside the step. With a seed made by the same build, eight 1x runs committed 0.74, 1.16, 1.38,
1.63, 1.77, 1.81, 1.83 and 1.95 events/s (median 1.70), each with the commit-order lock held in
99.6-100% of samples and 41-46 waiters (p50), and `box_busy_before` at most 1%. That number sees
only this pod's CPU: other pods on the same node are not in `/proc/stat`. So a before and after
compares several runs of each side (five or more), and their medians, not one run of each.

## The load

### Sessions

Each of N sessions (`ses-lg-0000`, `ses-lg-0001`, ...) works on one issue and watches four more:
session i has issue i mod min(N, 1,000), and the issues 37, 74, 111 and 148 places after it, with
the same wrap. A step with fewer than 1,000 sessions touches only the first N issues in key order.

- Each session registers with the listener as the pi plugin does: `POST /v1/interests/subscribe`,
  `self_subscribed: true`, port 0. It sends a heartbeat every 90 s. The heartbeats of all sessions
  are spread evenly.
- Each session subscribes over core NATS to its agent topic (`notifications.agent.<session>`) and
  to the topics of its five issues (`notifications.dispatch.issue.<key>.>`). The sessions share 50
  NATS connections. `run.log` records the subscriptions (N × 6 expected), the sessions registered
  and the registration errors.

### Event kinds

The event generator sends five kinds of write, each on its real route. Each write carries a
marker, `lgm<nonce><counter>x`: the run's nonce, 9 random decimal digits drawn when `loadgen run`
starts, then the write's counter in the run (1, 2, ...). `summary.json` (`config.marker_nonce`),
`meta.json` (`marker_nonce`) and the first line of `run.log` record the nonce. The run counts only
the commit rows and live frames whose marker carries its own nonce, so a run on a database that
earlier runs wrote to without a reset (`stack.sh loadgen run` resets nothing) still counts only its
own writes.

| Kind | Request | Primary event type | Marker field in the event |
| --- | --- | --- | --- |
| `comment` | `POST /api/v1/issues/{key}/comments` with `body` | `comment.created` | `payload.body` |
| `anchored` | The same, with `anchor: {artifact: "spec", quote}` on a seeded requirement cell | `comment.created` | `payload.body` |
| `reply` | The same, with `reply_to` a seeded root comment: an anchored root with probability 0.77, else a plain root | `comment.created` | `payload.body` |
| `edit` | `POST /api/v1/artifacts/{id}/edits`: one `replace` of `Revision token rN` with `rN+1`, with a `summary` | `artifact.version` | `payload.version.summary` |
| `upload` | `GET /api/v1/artifacts/{id}/text`, then `POST /api/v1/issues/{key}/artifacts` with `name: "spec.md"`, the same text with the token line changed, and a `summary` | `artifact.version` | `payload.version.summary` |

- The primary event types and marker fields were measured from the saved `envelope-*.json` files
  of a live step. The envelope's `payload` field holds the whole Dispatch event as a JSON string.
  The marker field is a path in that event.
- The 0.77 is from production: 1,217 of 1,576 replies were in anchored threads
  (the 2026-10-26 production write counts, §2 of the replay report of those writes).
- A write can add other events with its marker, for example an anchored comment's document
  snapshot or a reply's `comment.reopened`. `summary.json` counts them (`events.other_events`,
  `events.other_receipts`). The harness does not time them.
- Each write goes to one of the five issues of a random session. With probability `--human`, a
  person makes it: the request carries `X-Dispatch-User: person-<n>`, and route copies go to the
  agent topics. Otherwise the session makes it, with the stack's bearer token and
  `actor: {kind: "session", id}`.
- Edits and uploads to one issue run one at a time. The generator tracks each spec's revision
  token. When it does not see the response of an edit or upload, it reads the token from the
  document again (`GET /api/v1/artifacts/{id}/text`).

### Event mix

`--event-mix` is a preset name or `kind=weight,...`. The weights are relative. A kind with weight 0,
or no weight, is not sent. `loadgen run` refuses, before any connection, an unknown kind, a weight
that is not a number, a negative weight, a kind given twice and a mix whose weights are all 0. It
also refuses a mix with `reply` or `anchored` when `seed.json` has no root comment ids or no quotes.

| Preset | Weights | Source |
| --- | --- | --- |
| `production-2026-10-26` (default) | `comment=3,anchored=33,reply=35,edit=22,upload=7` | The 2026-10-26 production write counts for 22:55-23:34Z: 153 plain roots, 1,495 anchored roots, 1,576 replies, 1,329 document versions (§2 of the replay report of those writes). The 3 to 1 split of the versions into edits and uploads is INFERENCE from the production profile's write counts: `editArtifact` 6, `uploadArtifact` 2 (§3 of the production CPU profile of 2026-10-27). |
| `report-2026-10-27` | `comment=50,anchored=30,edit=20` | The root report's mix, so that its 1x row can be compared. |

### Route mix

The request generator sends GET requests at `--requests` per second, picked from a route-mix file.
One route per line. Blank lines and lines that start with `#` are skipped.

```text
<weight> <caller> <identity: person|agent> GET <path template>
22 browser person GET /api/v1/issues/{key}
```

- The weights are relative and must be more than 0.
- `person` sends `X-Dispatch-User: person-<0..6>`. `agent` sends the stack's bearer token.
- A route's name in `summary.json` and `TABLES.md` is `<caller> GET <path template>`.
- Placeholders:
  - `{key}`: the key of a random issue among the issues in use.
  - `{project}`: a random seeded project key.
  - `{artifact_id}`: the spec artifact id of that issue.
  - `{key_ref}`: `dispatch://<key>`, URL-escaped for a query string.

`loadgen run` refuses the file, with its line number, before any request when a line has:

- a field count other than 5,
- a weight that is not a positive number,
- an identity other than `person` or `agent`,
- a method other than `GET` (writes belong to the event generator),
- a path template that does not start with `/`, or has a `{` or `}` without its pair,
- an unknown placeholder,
- the same route as an earlier line.

It also refuses a file with no routes.

The default file, `routes-2026-10-26-1719Z.txt`, is built into the binary. It has 18 routes. Its
source is the request-source capture of 2026-10-26 17:19Z: 60 s of Dispatch's requests,
attributed to their callers. The capture gives the caller
groups: 39% one dashboard tab, 34% `dispatch read` calls, 17% Legion pods, 10% unattributed. The
split inside each group is INFERENCE from the capture's prose. These are the 18 routes of the root
report.

A request that the generator cannot start because 25,000 requests are in flight is dropped and
counted. 25,000 is more than 600 req/s × the 30 s client timeout.

## Delivery latency

A **receipt** is one envelope that one session gets on a topic it subscribes to. Its time is when
`loadgen`'s NATS handler runs. A receipt is matched to its write by the event `id` in the
envelope's payload.

A write's **primary event** is the event with the lowest id among the events that carry its marker
and have its kind's primary type. Only primary events on issue topics are timed.

- **From sent**: the receipt time minus the write's send time. The send time is when the client
  started the write's request. For an upload, it is the start of the `POST`, not of the `GET`
  before it. So the value also includes the client's own wait for a connection and the time to
  write the request.
- **From arrival**: the receipt time minus the write's arrival time. The arrival time is when the
  whole request is in Dispatch's socket (`httptrace.ClientTrace.WroteRequest`). When the trace does
  not see that, it is when the client started the request. So the value includes the time in
  Dispatch's accept queue, the header read, the pool wait, the lock and the commit, as the outage
  work's spec asks: "time spent queued before the commit counts" (its acceptance line 4). The
  client's own wait for a connection is in `summary.json` as `conn_wait_ms`, so a generator that
  queues its own requests shows it.
- **From commit**: the receipt time minus the commit time of the primary event's transaction.
  - The seed installs `loadtest.event_commits` and an `AFTER INSERT` trigger on `events`. For each
    new event that carries a marker, the trigger copies the event id, the type and the first marker
    in its payload. Events without a marker insert nothing.
  - After the grace period, `loadgen` reads `pg_xact_commit_timestamp(xmin)` of the rows whose
    marker is one of the run's in-window writes, its nonce included. This needs
    `track_commit_timestamp = on`.
  - The trigger runs inside the transaction and inside the commit-order lock that it measures. Its
    cost is one small insert, with no round trip.
  - The value compares a time from `loadgen`'s clock with a time from Postgres's clock. On the local
    stack both are the box's clock. Elsewhere the column needs the generator and Postgres on one
    clock.

The counts:

- **Offered**: the writes whose scheduled arrival is in the window.
- **Committed**: the offered writes whose primary event is in `loadtest.event_commits` with the
  write's marker, whatever their HTTP status. A write that hit the 30 s client timeout but committed
  counts, and gets delivery latencies. Another run's rows never count: their nonce differs.
- **Expected issue-topic receipts**: the committed primary events × the sessions that subscribe to
  the topic of the event's issue. **Got** is the issue-topic receipts of primary events.
  `missing` = expected − got.
- **Agent-topic receipts**: route copies on session agent topics. Counted (`agent_topic_receipts`),
  not timed.
- **`clock_anomalies`**: the receipts whose event committed before its write arrived. It must be 0
  on the local stack. A value above 0 means that the two clocks disagree.
- **`commit_times_missing`**: the primary events with no commit time. They get no latency from
  commit.

When `loadgen` cannot read the run's rows of `loadtest.event_commits`, `events.commits_error` holds
why. Then every count and latency that comes from the commit rows is 0 because nothing was read, not measured:
committed (also per kind), the issue-topic and agent-topic receipts, the expected receipts,
`missing`, `other_events`, `other_receipts`, `commit_times_missing`, `clock_anomalies` and every
delivery latency. So is `live.live_frame_ms_from_write_response`, and
`resources.dispatch_cpu_s_per_op` is null. The tables show those cells as `-` with the mark
`(commit rows not read)`, and the step fails (Run one step, above). A step with no write in the
window reads no rows: nothing depends on them.

Every percentile in the harness is the lower nearest rank: the value at index
⌊p/100 × (n − 1)⌋ of the sorted values.

## The step directory

| File | What it holds |
| --- | --- |
| `summary.json` | The step's results. Every table cell comes from it (The tables, below). `config.marker_nonce` is the run's marker nonce (Event kinds, above). `stack_exits` is there only when a stack process exited during the step: each entry's `name`, `pid` and `first_seen_gone`. Such a step failed. `events.commits_error` is there only when the run's commit rows could not be read (Delivery latency, above). Such a step failed too. |
| `TABLES.md` | The tables of this step. `loadgen tables` writes it. |
| `meta.json` | Mode 600 (owner only). The step name and arguments (`args`: the normalized flag list, each flag given to `loadgen run` once as `-name=value` in name order, with the values of `-token` and `-listener-token` replaced by `<redacted>`, the URL of each of `-url`, `-listener`, `-nats-mon` and `-pprof` read whole by Go's `net/url` and written without its userinfo and its `password` parameter, and `-nats` split at commas as nats.go splits it, each server without its own userinfo; the user name goes too, since nats.go takes a user name with no password as the NATS token. `-pg` is written from pgx's own parse of it (`pgconn.ParseConfig`, the parser `pgx.Connect` uses), never from the text given: `postgres://`, each host and port pgx would try, comma-separated (a port the URL leaves out as pgx fills it in, such as 5432), then `/` and the database, such as `postgres://127.0.0.1:15432/dispatch`; never its user, password, `sslpassword` or any other parameter, and `<redacted>` when pgx does not read it as a URL. No file of the step, and no line that `loadgen run` prints, holds a token, a URL password or a NATS token), `start`, `window_start`, `window_end`, `end`, `nproc`, CPU model, kernel, `box_busy_before` and `box_busy_before_window_s` (5), `GOMAXPROCS` of Dispatch (`unset` by default), the Postgres version (`postgres_version`: empty when it could not be read, and then `postgres_version_error` says why), the build commit and `build_dirty`, the seed meta, the `stack.json` path, `pg_direct` (always `false` under `-allow-foreign`, which takes nothing about the target from `stack.json`), `allow_foreign`, the pids and `marker_nonce`. `box_busy_before` is the share of the box's CPU time, all CPUs, that was not idle or I/O wait over 5 s just before the step, from two `/proc/stat` reads: `run-step.sh` reads it with `loadgen box-busy` before its reset and passes it as `-box-busy-before`; a `loadgen run` without that flag reads it itself (Requirements, above). `null` when `/proc/stat` gave no ticks. |
| `run.log` | `loadgen run`'s log: the configuration with the marker nonce, the guard, a warning when `box_busy_before` is above 10%, the sampler's pids, the NATS subscriptions, the registrations, the window, failed writes, the first three failed reads of each Postgres probe and of the 5 s sampler's Postgres, pprof and NATS reads (and each one's total, past three), a failed read of the commit rows, of the transaction ids or of the Postgres version, the result line, and the failure line when a stack process exited or the commit rows were not read. An endpoint that a line names is written without its URL password. |
| `samples.json` | The 5 s samples: CPU ticks and RSS per process, `/proc/stat` ticks, Postgres connections by state, goroutines, pool waiters, heap (every 15 s), NATS `/varz`. `failed_reads` names a sample's reads that failed (`postgres`, `goroutines`, `heap`, `nats`); their values in that sample are 0 (`nats` empty) and enter no statistic. A read that failed once the run had ended is neither named nor counted. |
| `cpu.pprof` | Dispatch's CPU profile over the whole window. |
| `heap.pprof` | Dispatch's heap profile at the end of the window. |
| `heap.txt` | The same heap profile as text (`/debug/pprof/heap?debug=1`), with the runtime's memory statistics. |
| `goroutines.txt` | Dispatch's goroutine dump (`debug=2`) at the end of the window, after the CPU profile. |
| `goroutines.summary.txt` | The goroutines that wait for a pool connection, the goroutines in `events.(*Broker).Append`, other request and document goroutines, and the top states. |
| `cpu.top.txt`, `cpu.cum.txt` | `go tool pprof -top` of `cpu.pprof`: by own time (40 nodes) and by cumulative time (80 nodes). |
| `heap.top.txt` | `go tool pprof -top` of `heap.pprof`, by in-use bytes (40 nodes). |
| `envelope-<type>.json` | One envelope that a session got on an issue topic, per event type, with a marker when one arrived. |
| `envelope-agent.<type>.json` | The same for an agent topic. |
| `dispatch.log` | Dispatch's log for the step. |
| `listener.log`, `nats.log` | The listener's and NATS's logs since their restart in the reset. |

A file that a step could not take is absent, and `run.log` names the reason. The Postgres log stays
in `$LOADTEST_ROOT/pg/log`, the proxy log in `$LOADTEST_ROOT/logs/proxy.out`.

`loadgen tables <step dir>...` renders the tables of one or more steps again. With one directory it
writes `TABLES.md` in it. With more, it writes `TABLES.md` in the current directory, or the file
that `-out` names. Under `stack.sh loadgen`, the current directory is `$LOADTEST_ROOT`. It also
writes `cpu.top.txt`, `cpu.cum.txt`, `heap.top.txt` and `goroutines.summary.txt` in each step
directory. `-tops=false` does not write these files. `run-series.sh` uses `-tops=false` for the
series `TABLES.md`, because `run-step.sh` already wrote them for each step.

## The tables

`TABLES.md` has eight tables, with one row per step. The window is the measured window
(`--duration`). "Cores" means CPU-seconds per second. A cell is `-` when it has no value, and also
when the read that it comes from failed; a failed read also marks the step name with `(commit
rows not read)`, `(1 failed read)` or `(<n> failed reads)` (below), so no cell shows a failed read
as a measured 0. A JSON path below is a field of the step's `summary.json`.

### Events, delivery and requests

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(<process> exited)` when a stack process exited during the step, and `(commit rows not read)` when the run's commit rows could not be read (below). | `config.step`, `stack_exits`, `events.commits_error` |
| `start (UTC)` | date and time | The start of the window. | `meta.json` `window_start` |
| `sessions` | count | The simulated sessions. | `config.sessions` |
| `events/s target / offered / committed` | writes per second | The target rate, then the offered writes and the committed writes, each divided by the window. `0` when the target is 0. Committed is `-` when the commit rows were not read. | `config.events_per_s`, `events.offered_per_s`, `events.committed_per_s` |
| `write errors non-2xx / transport` | count | The offered writes that got a status outside 2xx, then the offered writes that ended with no complete response: a connection error, the 30 s client timeout, or a failed read of the spec before an upload. | `events.write_errors.non_2xx`, `events.write_errors.transport` |
| `delivery ms from arrival p50 / p99` | ms | Delivery latency from arrival, over every issue-topic receipt of a primary event. `-` when the commit rows were not read. | `events.delivery_ms_from_arrival` |
| `delivery ms from commit p50 / p99` | ms | Delivery latency from commit, over the same receipts. `-` when the commit rows were not read. | `events.delivery_ms_from_commit` |
| `issue-topic receipts got / expected` | count | The issue-topic receipts of primary events, then the expected number. `-` when the commit rows were not read. | `events.issue_topic_receipts`, `events.expected_issue_topic_receipts` |
| `req/s target / sent` | requests per second | The target rate, then the requests sent in the window divided by the window. `0` when the target is 0. The mark `(below 95%)` follows when the sent rate is below 95% of the target. | `config.requests_per_s`, `requests.sent_per_s`, `requests.below_target` |
| `req 2xx` | percent | The share of sent requests that got a 2xx status. | `requests.ok_2xx` ÷ `requests.sent` |
| `req ms p50 / p99` | ms | Request latency, from arrival (as for a write) to the end of the response body, over every sent request, failures included. A failed request counts with the time it took to fail: about 30,000 for the client timeout, and about 5,000 for the 5 s connect timeout. A request that never connected has no arrival, so its time starts when the client began the request. | `requests.latency_ms` |
| `requests dropped at the in-flight cap` | count | The requests scheduled in the window that the generator did not send, because 25,000 were in flight. | `requests.dropped_inflight_cap` |

**The below-target mark.** `(below 95%)` in `req/s target / sent` means that the step sent fewer
than 95% of its target requests per second. Its request columns then describe a lighter load than
its name says. `TABLES.md` repeats this under the table when a row has the mark.

**The stack-exit mark.** `(dispatch exited)` after the step name, in every table, means that the
named stack process (`dispatch`, `listener`, `nats`, `proxy` or `postgres`) exited during the step
(`stack_exits` in `summary.json`). The step failed, and its row does not measure the load: its
numbers show a stack without that process. `TABLES.md` repeats this under the first table when a
row has the mark.

**The commit-rows mark.** `(commit rows not read)` after the step name, in this table, the
write-latency-by-kind table, the resources table and the delivery-from-sending table, means that
`loadgen` could not read the run's rows of `loadtest.event_commits`: the database has no
`commit-trigger.sql`, or Postgres did not answer. `events.commits_error` says why. Every cell that
depends on those rows shows `-`, not a measured 0: here the committed rate, both delivery
latencies and the receipts; per kind the committed count; in the resources table the Dispatch
CPU-s per operation; and the delivery from sent and the live frame latency from write response. The
step failed (Run one step, above). `TABLES.md` repeats this under each table where a row has the
mark.

**Connect timeouts above 1x.** At 10x on the 2-vCPU pod (series run of 2026-10-27, step
`step-10x`), most requests and writes ended with `dial tcp …: i/o timeout` after 5 s, and
`req ms p50` was about 5,000. During a second 10x run, a second client (`curl` to `/healthz` from
another process) did not connect either, while about 3,200 connections to Dispatch's port waited
in `SYN_SENT` (`/proc/net/tcp`). It connected again when the generators stopped. Thus new
connections to Dispatch's port were not accepted on the box, and the cause is not the
generator's own client. `run.log` lists each failed write with its error.

### Write latency by kind

There is one column per kind, in this order: `comment`, `anchored`, `reply`, `edit`, `upload`.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(commit rows not read)` when the run's commit rows could not be read (Events, delivery and requests, above). | `config.step`, `events.commits_error` |
| `comment ms p50 / p99 (committed / offered)` | ms, then count | Write latency of the kind's writes that got a 2xx status: from arrival to the end of the response body. Then the committed and offered writes of the kind. `-` alone when the step offered no write of the kind. `-` before the counts when no write of the kind got a 2xx status. `-` for committed when the commit rows were not read. | `events.by_kind.comment.write_ms`, `.committed`, `.offered` |
| `anchored ms p50 / p99 (committed / offered)` | ms, then count | The same for `anchored`. | `events.by_kind.anchored` |
| `reply ms p50 / p99 (committed / offered)` | ms, then count | The same for `reply`. | `events.by_kind.reply` |
| `edit ms p50 / p99 (committed / offered)` | ms, then count | The same for `edit`. | `events.by_kind.edit` |
| `upload ms p50 / p99 (committed / offered)` | ms, then count | The same for `upload`. The write latency covers the `POST` only, not the `GET` before it. | `events.by_kind.upload` |

`events.by_kind.<kind>` also holds `primary_type` and the kind's delivery latencies.

### Resources

The sampler reads every 5 s. It uses the samples from the window start to 5 s after the window end.
A process's cores are its CPU ticks at the last of those samples minus the ticks at the first,
divided by 100 ticks per second and by the seconds between the two samples.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(commit rows not read)` when the run's commit rows could not be read (Events, delivery and requests, above), then `(1 failed read)` or `(<n> failed reads)` when n of the sampler's Postgres and pprof reads in the window failed (below). | `config.step`, `events.commits_error`, `resources.pg_read_errors`, `resources.pprof_read_errors` |
| `Dispatch cores mean / peak 5 s` | cores | Dispatch's mean cores in the window, then its highest cores in one 5 s sample interval. | `resources.cpu_cores_mean.dispatch`, `resources.cpu_cores_peak_5s.dispatch` |
| `Postgres cores` | cores | Mean cores of the stack's Postgres: the postmaster, its live children and the postmaster's time for children that exited. | `resources.cpu_cores_mean.postgres` |
| `listener cores` | cores | Mean cores of the listener. | `resources.cpu_cores_mean.listener` |
| `NATS cores` | cores | Mean cores of `nats-server`. | `resources.cpu_cores_mean.nats` |
| `loadgen cores` | cores | Mean cores of the `loadgen run` process: the generators, the sessions and the samplers. | `resources.cpu_cores_mean.loadgen` |
| `proxy cores` | cores | Mean cores of the latency proxy. | `resources.cpu_cores_mean.proxy` |
| `box busy` | percent | The share of the box's total CPU time (all CPUs) that was not idle or I/O wait, from `/proc/stat`. | `resources.sys_busy_frac` |
| `box busy before` | percent | The same share over 5 s just before the step, read by `run-step.sh` before its reset, while Dispatch is stopped: the other work on the machine, other roles' processes in the pod included. Above 10%, the step shared the box (`run.log` warns). | `meta.json` `box_busy_before` |
| `Dispatch RSS max MiB` | MiB | Dispatch's highest resident memory (`VmRSS`) in a sample. | `resources.rss_mib_max.dispatch` |
| `heap in use MiB p50 / max` | MiB | Dispatch's `HeapInuse` from `/debug/pprof/heap?debug=1`, read every 15 s. | `resources.heap_inuse_mib` |
| `goroutines p50 / max` | count | Dispatch's goroutines, from `/debug/pprof/goroutine?debug=1`. | `resources.goroutines` |
| `goroutines waiting for a pool connection p50 / max` | count | Dispatch's goroutines with a pgx pool `Acquire` frame (puddle) in their stack. | `resources.pool_acquire_goroutines` |
| `Postgres connections max` | count | The highest number of `pg_stat_activity` rows for database `dispatch`. It includes up to 5 connections of `loadgen`: the sampler, the lock probe, the ungranted-lock sampler, the transaction-id hold sampler and the transaction-id probe. | `resources.pg_conns_total.max` |
| `connections waiting on a lock p50` | count | The p50 of `dispatch` connections with state `active` and wait event type `Lock`, over the samples that saw at least one. `0` when no sample saw one. An advisory lock wait has this type. | `resources.pg_conns_by_state["active\|Lock"]` |
| `Dispatch CPU-s per operation` | CPU-seconds | Dispatch's mean cores divided by the operation rate. An operation is a committed write or a sent request in the window. `-` when the commit rows were not read. | `resources.dispatch_cpu_s_per_op` |

`resources` also holds every process's RSS, Postgres connections by state, and NATS messages in and
out per second and slow consumers. The NATS rates are over the first and last in-window `/varz`
reads that worked, `null` when fewer than two worked; slow consumers are the last such read's,
`null` when none worked. Under `-allow-foreign`, `loadgen` has no stack pids, so only the `loadgen`
cores have a value, and no stack process is checked for an exit.

A sample's read of `pg_stat_activity` fails when the query fails or the sampler cannot connect;
its read of Dispatch's goroutine or heap profile fails when the GET fails, answers other than 200,
or holds no goroutine total (no `HeapInuse` for the heap); its read of NATS `/varz` fails when the
GET fails, answers other than 200 or holds no JSON. `resources.pg_read_errors` counts the
in-window samples whose Postgres read failed, `resources.pprof_read_errors` the in-window goroutine
and heap reads that failed, and `resources.nats_read_errors` the in-window NATS reads that failed.
The row carries the mark `(1 failed read)` or `(<n> failed reads)` with the sum of the first two;
no column shows the NATS reads. `run.log` shows the first three of each kind and their total; its
totals count every sample from the warm-up to the end of the grace period, while
`pg_read_errors`, `pprof_read_errors` and `nats_read_errors` count in-window samples only, so the
two totals can differ. A failed read enters no statistic, so the Postgres, goroutine, pool-waiter
and heap columns are over the reads that worked, and a column with no read that worked shows `-`,
not 0. `TABLES.md` repeats this under the table when a row has the mark.

### Commit-order lock

The lock probe reads every 20 ms from 20 s after the window start to the window end. When the
window is shorter than 20 s, it starts at the window start. The lock is Dispatch's database-wide
advisory lock `0x4449535041544348` (`internal/dispatch/events/broker.go`), which orders event
commits.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(1 failed read)` or `(<n> failed reads)` when the probe had failed reads. | `config.step`, `commit_order_lock.errors` |
| `time held` | percent | The share of samples in which a backend held the lock. | `commit_order_lock.held_fraction` |
| `waiters p50 / max` | count | The backends that wait for the lock, per sample. | `commit_order_lock.waiters` |
| `hold ms p50 / p90 / p99` | ms | Hold durations. One hold is a run of samples with the same holder pid and transaction start. It lasts from its first sample to the sample that ends it. A hold that is open at the last sample is not counted. The resolution is the sample interval. | `commit_order_lock.hold_ms` |
| `holds seen` | count | The holds counted for `hold ms`. | `commit_order_lock.holds_observed` |
| `what the holder was doing (share of held samples)` | text, percent | Up to four holder states, most frequent first, each with its share of held samples. A state is `<state> (<wait event type>:<wait event>)`, or `active: <query>` with the query cut to 70 characters. `idle in transaction (Client:ClientRead)` means that Postgres waits for Dispatch to send the next statement. | `commit_order_lock.holder_doing` (up to 12 entries) |

**The failed-reads mark.** `(<n> failed reads)`, or `(1 failed read)`, after the step name, in this
table, the ungranted-locks table and the transaction-id holds table, means that n of that probe's
reads (of `pg_locks`; of `pg_stat_activity` for the transaction-id hold sampler) failed: a failed
query, or a failed connect, which counts as one and leaves the probe with no sample.
`commit_order_lock.errors`, `ungranted_locks.errors` and `xid_holds.errors` count them, and
`run.log` shows the first three of each probe and their total. The probe's samples miss those
reads, so its shares are over the reads that worked, and a row with no sample reads `-`, not 0: a
probe that failed is never shown as a lock nobody held. `TABLES.md` repeats this under the table
when a row has the mark. The transaction-ids table uses the mark `(1 failed read)` for its probe
(Transaction ids, below). None of these probes feeds the committed, receipt or delivery columns, so
a failed read marks its own table and does not fail the step.

When the event-lock change removes the commit-order lock, this table reads 0 by construction. No
backend can hold or wait for a lock that Dispatch does not take. The ungranted-locks table
(Ungranted locks, below) is then the measure of lock waits.

### Delivery from sending, and live socket frames

Each dashboard live socket (`--live`) opens `GET /ws/live` as `viewer-<i>` and sends the frame a
dashboard tab sends for the event feed, `{"type":"subscribe","sub":"events","view":{"kind":"events"}}`,
with no resume, so it follows the feed from its head. Dispatch answers `events_start`, then one
`event` frame per event: `{"type":"event","sub":"events","id":"<n>","event":<the event's JSON>}`. Only
`event` frames count as frames; `events_start`, `heartbeat` and every other frame type are read and
not counted. The frame time is when its message arrives. Only the frames that arrive in the window
count. A frame's event id is its `id`. An `ended` frame (the subscription fell behind) is answered
with the same subscribe frame on the same socket, as a tab does. When the socket ends (Dispatch
closes each at a random 30-60 min age with 4001, and at shutdown with 1012), or its subscription is
refused or ends before its `events_start`, the socket is dialed again a second later. Each socket
offers permessage-deflate, as a browser does. `live_bytes` is the bytes of every message the
sockets read, after decompression.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(commit rows not read)` when the run's commit rows could not be read (Events, delivery and requests, above). | `config.step`, `events.commits_error` |
| `NATS delivery ms from sent p50 / p99` | ms | Delivery latency from sent (Delivery latency, above), over every issue-topic receipt of a primary event. `-` when the commit rows were not read. | `events.delivery_ms_from_sent` |
| `live frame ms from created_at p50 / p99` | ms | The frame time minus the `created_at` of the frame's event, over every frame with a `created_at`. | `live.live_frame_ms_from_created_at` |
| `live frame ms from write response p50 / p99` | ms | The frame time minus the time when the client read the write's response. Only the frame of a primary event counts. Its write must be in the window, have a 2xx status and be committed. A write with no complete response has no response time, so its frames do not count. `-` when the commit rows were not read. | `live.live_frame_ms_from_write_response` |
| `live frames` | count | The event frames of all live sockets that arrived in the window. | `live.live_frames` |
| `live order violations` | count | The frames whose event id is not more than the event id of the previous frame on the same connection. The check starts again on each new connection. A value above 0 means that a socket sent events out of id order. | `live.live_order_violations` |

### Ungranted locks

The ungranted-lock sampler reads `pg_locks` once a second, from the same start to the same end as the
lock probe (Commit-order lock, above). It reads over a direct Postgres connection, not through the
proxy. Each sample lists every heavyweight lock that a backend waits for (`granted` is false). The
sampler groups these waits by lock type, target and mode.

The target names the lock:

- `commit_order_lock`: Dispatch's commit-order advisory lock (Commit-order lock, above).
- `notify_commit_lock`: the commit lock of Postgres's `NOTIFY`. It is the `object` lock on database
  0 that `PreCommit_Notify` takes.
- A relation name, for a `relation` or `tuple` lock. For a relation in another database, `loadgen`
  reads the name after the step, with one short connection to that database. When that read
  fails, the target is `<database>:<oid>`.
- `row_wait`, for a `transactionid` lock: a wait for a row that another transaction locked.
- `<classid>:<objid>`, for another advisory lock.
- The ids of the lock, joined by `:`, for any other lock.

For each wait, the sampler records the waiter's statement and the statement of its first blocker.
The first blocker is the backend with the lowest pid that holds the same lock, granted, in the same
`pg_locks` read. For a relation lock, that holder can hold a mode that does not conflict with the
wait. Each statement is the `query` of `pg_stat_activity`, cut to 80 characters.

The sampler reads once a second, not every 20 ms as the lock probe does, because each read costs
Postgres CPU on the box it measures. Read every 20 ms with about 45 waiters at 1x, one read took 75
to 180 ms of a Postgres backend, and 1x committed 0.29 to 0.82 events/s, against 1.63 to 1.81
without the sampler, on the same build and seed (2026-10-27). It never calls `pg_blocking_pids`,
which takes exclusive access to Postgres's lock manager.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(1 failed read)` or `(<n> failed reads)` when the sampler had failed reads (Commit-order lock, above). | `config.step`, `ungranted_locks.errors` |
| `samples with an ungranted lock` | percent | The share of samples that saw at least one ungranted lock. | `ungranted_locks.share_of_samples_with_any_ungranted` |
| `locks waited for: type target mode, samples with a waiter, waiters p50 / max` | text, percent, count | Up to four groups. The group with a waiter in the most samples comes first. Each group shows its lock type, target and mode. Then it shows the share of samples with at least one waiter in the group. Then it shows the waiters per sample, p50 / max. A sample with no waiter in the group counts as 0. | `ungranted_locks.groups[]`: `locktype`, `target`, `mode`, `share_of_samples_with_waiter`, `waiters_p50`, `waiters_max` |
| `top waiter query → first blocker query (count), per lock` | text, count | One entry for each group of the column before, in the same order. The entry is the most frequent pair of waiter statement and first-blocker statement, then the number of waits with that pair. `(none)` is an empty statement: the sampler found no blocker, or the backend had no statement text. | `ungranted_locks.groups[].top_pairs[0]`: `waiter_query`, `blocker_query`, `count` |

`ungranted_locks.samples` is the number of samples and `ungranted_locks.errors` the failed reads.
`groups` holds every group, and `top_pairs` holds up to 3 pairs per group, most frequent first. A
step whose sampler took no sample has `-` in every cell.

### Transaction ids

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(1 failed read)` when the transaction-id probe's connect or one of its two reads failed (Commit-order lock, above). | `config.step`, `resources.xid_read_error` |
| `transaction ids per s` | transaction ids per second | The transaction ids that Postgres assigned per second in the window, in all databases of the server. The transaction-id probe reads the next transaction id (`pg_snapshot_xmax(pg_current_snapshot())`) at the window start and at the window end. It divides the difference by the seconds between the two reads. The probe assigns no transaction id itself. `-` with the mark when a read failed (`resources.xid_read_error` holds the error, and `run.log` shows it); `-` alone when `resources` is null. | `resources.xids_per_s` |

### Transaction-id holds

The transaction-id hold sampler reads `pg_stat_activity` every `--xact-interval` (10 ms by default)
from the window start to the window end, over a direct Postgres connection, not through the proxy.
Each read lists the client backends of database `dispatch` that are in a transaction (`xact_start`
set), the sampler's own backend excluded: the pid, `xact_start`, whether `backend_xid` is set, the
state and the first 80 characters of the statement. It reads no `pg_locks`. `--xact-interval 0`
turns the sampler off, for a control step against a step with it on; the row then reads `off`.

One transaction is one pid and `xact_start`. It ends at the first read that does not show it. A
transaction counts when a read showed it with a transaction id (`backend_xid` set) and it ended in
the window: one still open at the last read has no known end and is not counted. A transaction
gets its id at its first write and keeps it to its end, so the hold leaves out the reads before the
first write, which `transaction ms` and the idle time without an id include.

A read sees a transaction only while it is open. A hold shorter than the interval is seen with a
chance of about its length over the interval, so the sampler misses many short holds:
`xid holders / committed events` falls as holds shorten, and the percentiles are over the holds
that a read saw.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name, with the mark `(<n> failed reads)` when the sampler had failed reads (Commit-order lock, above). | `config.step`, `xid_holds.errors` |
| `interval ms (samples/s)` | ms, reads per second | The interval, then the reads per second that the sampler made: the reads minus one, divided by the seconds from the first read to the last. Below 1,000 / interval, slow reads made it skip slots (`pgpoll.go`). `off` when `--xact-interval 0` turned the sampler off. | `xid_holds.interval_ms`, `xid_holds.samples_per_s` |
| `xid holders / committed events` | count | The transactions counted (above), then the committed writes of the window. | `xid_holds.xid_holders`, `xid_holds.committed_events` (`events.committed`) |
| `xid hold ms` | ms, p50 / p99 / max | From the transaction's first read with a transaction id to its last, plus one interval. A hold that one read saw is one interval. | `xid_holds.xid_hold_ms` |
| `transaction ms` | ms, p50 / p99 / max | From `xact_start` to the transaction's last read. | `xid_holds.xact_ms` |
| `idle in transaction with xid ms` | ms, p50 / p99 / max | The transaction's reads in state `idle in transaction` with a transaction id, times the interval: Postgres waited for Dispatch's next statement while the transaction held its id. | `xid_holds.idle_in_xact_with_xid_ms` |
| `idle in transaction without xid ms` | ms, p50 / p99 / max | The same, without a transaction id: before the transaction's first write. | `xid_holds.idle_in_xact_without_xid_ms` |
| `longest xid holders: hold ms, last statement` | ms, text | The five longest holds, longest first, each with the transaction's statement at its last read, whitespace collapsed. `(none)` is an empty statement. | `xid_holds.longest_xid_holders[]`: `xid_hold_ms`, `xact_ms`, `last_query` |

Each value of the four ms columns is one transaction counted. `xid_holds.samples` is the number of
reads and `xid_holds.errors` the failed reads; each statistic also has its p90. A step whose
sampler made no read has `-` in every cell. A `summary.json` written by a `loadgen` without the
sampler has `xid_holds` null, and its row is all `-`.

### Requests by route

There is one row per route and one column per step.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `route` | name | The route name, `<caller> GET <path template>`. | the keys of `requests.routes` |
| `<step name>` | ms, then count | Request latency p50 / p99 of the route in that step, then (failures / count). A failure is a status outside 2xx or a transport error. The count is the sent requests of the route. `-` when the step sent no request on the route. | `requests.routes.<route>.latency_ms`, `.failures`, `.count` |

## The guard

`loadgen run`, `loadgen seed` and the reference commands (`seed-refs`, `refs-shape`, `refs-fetch`)
read every endpoint and token from `stack.json`. Never point them at the pod's live listener
(`127.0.0.1:9020`) or at production. A step writes comments and document versions at the rate it
is given. The guard stops a mistaken target before the first write:

1. An endpoint flag (`-url`, `-listener`, `-nats`, `-nats-mon`, `-pprof`, `-pg`, `-token`,
   `-listener-token`) that differs from `stack.json` is refused before any connection, unless
   `-allow-foreign` is given. The refusal names the flag and `-allow-foreign`. Under
   `-allow-foreign`, every one of these flags is required, and the first one left empty is refused
   by name before any connection; so is a URL flag whose value is not a URL with a scheme and a
   host as Go's `net/url` reads it (for `-nats`, a comma-separated list of servers, each naming a
   host), and a `-pg` that does not start with `postgres://` or `postgresql://` (case-sensitive,
   as pgx checks it) or that pgx's own parser (`pgconn.ParseConfig`) refuses. Each is refused by
   its name and the reason alone, never the value or pgx's error, since an error that printed the
   value would print its credentials.
2. Before its first write, `loadgen` checks:
   - NATS `/varz` `server_name` is the stack name.
   - Dispatch answers `GET /api/v1/projects` with the stack's random agent token.
   - The listener answers `/v1/sessions` with the stack's random API token, and refuses a random
     other token.

   A failed check exits before any write, and names the check.

The pod's live listener and production Dispatch hold other tokens, so a mistaken target fails the
check.

`-allow-foreign` is for a stack that is not local, such as the cluster load test's copy.
With it, every endpoint and token comes from its flag, and none from `stack.json`. A flag that fell
back to the local stack would have a run write to the remote Dispatch while it counted commits in
the local database or profiled the local Dispatch, with no error.
Give `-nats-server-name` to keep the NATS check. The sampler gets no pids. `meta.json` records
`allow_foreign`, and `pg_direct` false.

## Differences from production

- **Two vCPUs, shared.** Postgres, the proxy, NATS, the listener, Dispatch and `loadgen` share the
  2 CPUs. Production Dispatch has 16 CPUs and a 16-vCPU database instance (root report). So the
  load at which Dispatch fails here is lower than in production. The mechanism is what carries
  over.
- **gVisor.** Every syscall costs more in the sandbox. In the root report, `Syscall6` was 16-61% of
  Dispatch's CPU samples. INFERENCE: Dispatch's CPU per operation here is higher than in
  production, if the production node is not gVisor.
- **The database.** Local Postgres 16.2 with pgvector, from the `pgserver` wheel, with the settings
  above. Dispatch reaches it through the latency proxy at `-rtt 1ms`, which adds about 4 ms per
  round trip, not 1 ms. `LOADTEST_PG_DIRECT=1` removes the proxy.
- **Postgres 16, not 18.** The harness runs PostgreSQL 16.2 (the `pgserver` wheel), while
  production Dispatch runs PostgreSQL 18 since 2026-10-28 (`gcp:Dispatch`'s default database
  version is `POSTGRES_18`). So the harness compares a fix's before and after on one version, and
  absolute numbers on 18 come from the cluster load test.
- **Header identity.** A person's request names the person in `X-Dispatch-User`. Each such request
  also writes a `people` row. Production uses cookie identity. Agents use the shared bearer token
  and a session actor.
- **No embeddings.** Dispatch gets no AWS credentials or region. Meaning search is unavailable, and
  search falls back to keyword.
- **A smaller dataset.** 1,000 issues and about 10,000 events at the start of each step.
- **No browser documents.** No document websockets and no dashboard refetch fan-out. The browser
  share of the route mix is plain GETs. The live sockets subscribe to the event feed alone (no
  delivery or agent views), time each event frame, and do nothing more.
- **One listener, sessions on NATS.** The sessions receive from NATS directly, as the pi plugin's
  self-subscription does. They register with port 0, so the listener delivers nothing to them.
- **One process each.** One Dispatch process, one NATS server and one listener, all on
  `127.0.0.1`. `GOMAXPROCS` is not set.

## Differences from the root report

Compare the right rows:

- 0.25x and 0.5x run 100 and 200 sessions (the spec's definition). The report ran 350 and 367.
- Every step starts from the seed. The report's database grew across steps, which it ran in the
  order 1x, 2x, 5x, controls, 10x, 1x-rerun, 0.25x, 0.5x.
- Delivery is reported from arrival and from commit (the spec's columns), and from sent. The report
  had "after commit response" and "after event created_at".
- Committed writes come from the database. The report counted 2xx responses.
- The default event mix is production's (`production-2026-10-26`), which adds replies and uploads.
  `--event-mix report-2026-10-27` runs the report's mix. Compare that row with the report's
  `step-1x-rerun`.
- The CPU profile covers the whole window. The report took 30 s.
- The goroutine dump and the heap profile come at the end of the window, outside the CPU profile.
  The report took the dump inside the profile (its bias 6).
- The lock probe runs from 20 s into the window to its end. The report sampled for 60 s.
- Postgres CPU counts only the stack's postmaster, its children and its exited children. The
  report summed every process named `postgres` on the box.
- The request generator caps at 25,000 requests in flight. The report capped at 4,000.
- Dispatch is built from this checkout. The report built a branch with a patched pool size, which
  main now has (`sharedPoolSize = 64`).

## What this harness does not do

- The cluster load test can use `loadgen run -allow-foreign` with every endpoint and
  token flag and its own seed. It gets:
  - no `/proc` CPU or RSS columns, because it has no pids,
  - no committed counts, issue-topic receipts or delivery latencies at all when its database lacks
    `loadtest.event_commits`. The read of the commit rows fails, every cell that depends on them
    shows `-` with the mark `(commit rows not read)` (The tables, above), and `loadgen run` exits
    non-zero naming the read (`events.commits_error`). Such a database needs `commit-trigger.sql`
    (`loadgen trigger-sql` prints it) installed before the run, and `track_commit_timestamp = on`,
  - no delivery latency from commit when its database has the trigger but
    `track_commit_timestamp` is off: `commit_times_missing` counts those events, and the
    committed counts, receipts and the other latencies stay,
  - no reset per step.
- These belong to the cluster load test: 10-minute steps, one-hour soak trends (the 5 s series in
  `samples.json` is there for them), the replay of production's log, sampling of the production
  database and restarts during load.
- The browser-editor load work adds its browser-editor workload, its round-trip counter and its
  document-room sampler
  to this package.
- The request-path CPU work's request generator, a replay of production's writes, is to be built
  into this package, so that
  one generator remains. The route-mix file and `routes.go` are this package's own GET generator
  until then.
