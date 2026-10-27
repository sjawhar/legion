# Dispatch load test (`loadtest/`)

This package reproduces the load of the 2026-10-26 Dispatch outage on one 2-core machine. It runs
a local stack (Postgres, a latency proxy, NATS, the Envoy listener and Dispatch, all built from this
checkout), puts a measured load on it and writes tables of what happened.

- The root report of the outage load:
  [load reproduction, 2026-10-27](dispatch://INFRA-514/artifact/load-reproduction-2026-10-27-md).
  This harness is the committed form of the harness that report used.
- The spec of this package: [INFRA-589](dispatch://INFRA-589). The root spec:
  [INFRA-514](dispatch://INFRA-514/spec).

The harness measures. It changes no Dispatch, listener or outbox code. A fix of INFRA-514 runs the
same step before and after its change, and compares the two rows.

## Files

| File | What it is |
| --- | --- |
| `main.go` | The `loadgen` binary: subcommands `proxy`, `seed`, `run`, `tables`, `series-sessions`, `trigger-sql` |
| `api.go` | The Dispatch HTTP client, with the request trace that gives the arrival time |
| `seed.go` | The seed: 4 projects, 1,000 issues and 8,000 comments through Dispatch's API, and `seed.json` |
| `run.go` | One step: sessions, the event generator, the request generator, SSE streams, the window, `summary.json` |
| `events.go` | The five write kinds and the event mix |
| `routes.go` | The route-mix file parser and the request paths |
| `delivery.go` | The join of writes, database commits and receipts into delivery latencies |
| `sample.go` | The 5 s sampler: CPU, RSS, Postgres connections, goroutines, pool waiters, heap, NATS `/varz` |
| `lockprobe.go` | The 20 ms sampler of the commit-order lock |
| `proxy.go` | The TCP latency proxy toward Postgres |
| `tables.go` | `TABLES.md` from one or more `summary.json` files, the pprof tops and the goroutine summary |
| `guard.go` | `stack.json`, the endpoint flags and the check that every endpoint is the stack's own |
| `commit-trigger.sql` | The commit-time trigger for the delivery latency from commit |
| `routes-2026-10-26-1719Z.txt` | The default route mix |
| `stack.sh` | The local stack: `setup`, `start`, `seed`, `reset`, `stop`, `status`, `build-info`, `loadgen` |
| `run-step.sh` | The one command for a named step |
| `run-series.sh` | The series of six steps, 0.25x to 10x |
| `*_test.go` | Unit tests. They need no Postgres and no NATS: `cd packages/envoy && go test ./loadtest/` |

## Requirements

- A Linux machine with 2 vCPUs, the shape of a Legion issue pod. The root report ran on such a
  pod: `nproc` 2, 19 GiB of memory, a gVisor sandbox. The samplers read `/proc`, so the harness
  runs on Linux only.
- No other heavy work on the machine during a step. `meta.json` records the load average before
  each step.
- The Go toolchain (`go`). `stack.sh setup` builds the binaries and installs `nats-server` with it.
  `loadgen tables` uses `go tool pprof`.
- `uv`. `stack.sh setup` installs Python 3.12 and the `pgserver` wheel (0.1.4) with it. The wheel
  holds Postgres 16.2 and pgvector. The harness uses no Docker.
- `jq` and `curl`.
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
  [--route-mix <file>] [--human 0.1] [--sse <n>] [--rng-seed 1] [--out <dir>]
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
| `--sse` | `max(1, round(requests/30))` | Dashboard SSE streams (`GET /api/v1/events`): 2 at 60 req/s, 20 at 600 req/s. |
| `--rng-seed` | `1` | The seed of the event and request schedules. |
| `--out` | `$LOADTEST_ROOT/runs/<name>` | The step directory. It must not exist, or it must be empty. |

Rates are used as given. `0` is a real zero. The event and request schedules are open-loop Poisson
from the RNG seed, so two runs of one step with one seed offer the same arrivals.

`run-step.sh` does these steps in order:

1. `stack.sh setup`: it builds the binaries from the checkout (incremental), so a step always runs
   the checkout's current code.
2. `stack.sh start`, then `stack.sh seed`. Each one does nothing when its work is already done. So
   `run-step.sh` is the one command from a fresh checkout. On the first run, the seed alone takes
   about 17 minutes.
3. The migration check (Before and after across commits, below).
4. `stack.sh reset`, with Dispatch's log in the step directory (The reset, below).
5. `loadgen run`. It writes the step's files.
6. It stops Dispatch. It also stops Dispatch when the step fails. Postgres, the proxy, NATS and the
   listener keep running. `stack.sh stop` stops them.
7. It copies the listener and NATS logs into the step directory and runs `loadgen tables` there.

To run a step name again, remove its step directory first.

`loadgen run` also takes `-nats-conns` (default 50), the NATS connections that the sessions share.
`run-step.sh` does not pass it.

## Run the series

```bash
packages/envoy/loadtest/run-series.sh [step flags]
```

The series runs six steps with `run-step.sh`, in order, each from a fresh reset. It writes them
into `$LOADTEST_ROOT/runs/series-<UTC time>/step-<f>x/`, and one `TABLES.md` across them in the
series directory.

| step | events/s | sessions | requests/s | SSE |
| --- | --- | --- | --- | --- |
| step-0.25x | 1 | 100 | 15 | 1 |
| step-0.5x | 2 | 200 | 30 | 1 |
| step-1x | 4 | 400 | 60 | 2 |
| step-2x | 8 | 467 | 120 | 4 |
| step-5x | 20 | 667 | 300 | 10 |
| step-10x | 40 | 1,000 | 600 | 20 |

- Events are 4/s and requests 60/s at 1x, multiplied by the load factor f.
- SSE streams are `max(1, round(2f))`.
- Sessions are piecewise linear through the spec's three points: 100 at 0.25x, 400 at 1x and
  1,000 at 10x. That is 400·f up to 1x, and 400 + 600·(f − 1)/9 above it.
  `loadgen series-sessions <f>` prints the value.
- The series passes every other step flag to each step: `--duration`, `--warmup`, `--grace`,
  `--event-mix`, `--route-mix`, `--human`, `--rng-seed`. It refuses `--events`, `--sessions`,
  `--requests`, `--sse` and `--out`, because it sets them.
- With the default window, a step takes about 5 minutes and a series about 30 minutes. The setup
  and the seed run once, before the series directory exists.

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

`stack.sh seed` makes the database that every step starts from. It took 1,025 s (about 17 minutes)
on a 2-vCPU pod. It does these steps:

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
3. It waits until the outbox publishes every seed event (at most 900 s), then stops Dispatch.
4. It runs `vacuum analyze`, then installs the commit-time trigger (`loadgen trigger-sql`).
5. It writes `seed.meta.json` and renames the database to `dispatch_seed`, the template.

The seed content does not depend on the clock. Each issue's text comes from a generator seeded by
its project and number.

`$LOADTEST_ROOT/seed.json` is what `loadgen run` reads: the project keys and, per issue, its key,
project, `artifact_id` (the spec), `spec_name`, `spec_bytes`, `quotes` (the requirement cells),
`anchored_comment_ids`, `plain_comment_ids` and `route`.

`$LOADTEST_ROOT/seed.meta.json` records `migration` (the highest Dispatch migration applied),
`build_commit`, `build_dirty`, `created_at`, `seed_seconds`, `postgres_version` and `counts`
(projects, issues, comments, artifacts, artifact versions, events). One seed counted 4 projects,
1,000 issues, 8,000 comments, 1,000 artifacts, 1,000 artifact versions and 10,004 events.

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
export LOADTEST_SRC=<older checkout>
packages/envoy/loadtest/stack.sh setup        # build the older Dispatch, listener and loadgen
packages/envoy/loadtest/stack.sh start
packages/envoy/loadtest/stack.sh seed --force  # seed with the older build
packages/envoy/loadtest/run-step.sh step-1x-before --events 4 --sessions 400 --requests 60
LOADTEST_SRC=<newer checkout> packages/envoy/loadtest/run-step.sh step-1x-after \
  --events 4 --sessions 400 --requests 60
```

`stack.sh seed` builds nothing. It uses the binaries of the last `setup`, so run `setup` with the
older checkout first. `setup` builds `loadgen` from `LOADTEST_SRC` too, so the older checkout must
hold this package.

A newer build migrates the restored copy forward when Dispatch starts. An older build cannot start
on a seed that a newer build made. `run-step.sh` compares the highest migration file in the
checkout (`packages/envoy/internal/dispatch/store/migrations`) with `seed.meta.json`. It refuses a
build that is older than the seed, and names both levels. `meta.json` records the build commit and
whether the tree had uncommitted changes.

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
marker, `lgm<N>x`, where N is a counter that is unique in the step.

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
  ([INFRA-512 report](dispatch://INFRA-512/artifact/dispatch-cpu-profile-linux-replay-md) §2).
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
| `production-2026-10-26` (default) | `comment=3,anchored=33,reply=35,edit=22,upload=7` | INFRA-512's counts for 22:55-23:34Z on 2026-10-26: 153 plain roots, 1,495 anchored roots, 1,576 replies, 1,329 document versions ([INFRA-512 report](dispatch://INFRA-512/artifact/dispatch-cpu-profile-linux-replay-md) §2). The 3 to 1 split of the versions into edits and uploads is INFERENCE from the production profile's write counts: `editArtifact` 6, `uploadArtifact` 2 ([production profile](dispatch://INFRA-514/artifact/production-profile-2026-10-27-md) §3). |
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
source is the 60 s capture of Dispatch's requests at 17:19Z on 2026-10-26
(dispatch://INFRA-324/comment/dcb8960b-923a-4d07-a3d8-8b8d3d2cf134). The capture gives the caller
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

- **From arrival**: the receipt time minus the write's arrival time. The arrival time is when the
  whole request is in Dispatch's socket (`httptrace.ClientTrace.WroteRequest`). When the trace does
  not see that, it is when the client started the request. So the value includes the time in
  Dispatch's accept queue, the header read, the pool wait, the lock and the commit, as the root spec
  asks: "time spent queued before the commit counts" (dispatch://INFRA-514/spec, acceptance 4). The
  client's own wait for a connection is in `summary.json` as `conn_wait_ms`, so a generator that
  queues its own requests shows it.
- **From commit**: the receipt time minus the commit time of the primary event's transaction.
  - The seed installs `loadtest.event_commits` and an `AFTER INSERT` trigger on `events`. For each
    new event that carries a marker, the trigger copies the event id, the type and the first marker
    in its payload. Events without a marker insert nothing.
  - After the grace period, `loadgen` reads `pg_xact_commit_timestamp(xmin)` of those rows. This
    needs `track_commit_timestamp = on`.
  - The trigger runs inside the transaction and inside the commit-order lock that it measures. Its
    cost is one small insert, with no round trip.
  - The value compares a time from `loadgen`'s clock with a time from Postgres's clock. On the local
    stack both are the box's clock. Elsewhere the column needs the generator and Postgres on one
    clock.

The counts:

- **Offered**: the writes whose scheduled arrival is in the window.
- **Committed**: the offered writes whose primary event is in `loadtest.event_commits`, whatever
  their HTTP status. A write that hit the 30 s client timeout but committed counts, and gets
  delivery latencies.
- **Expected issue-topic receipts**: the committed primary events × the sessions that subscribe to
  the topic of the event's issue. **Got** is the issue-topic receipts of primary events.
  `missing` = expected − got.
- **Agent-topic receipts**: route copies on session agent topics. Counted (`agent_topic_receipts`),
  not timed.
- **`clock_anomalies`**: the receipts whose event committed before its write arrived. It must be 0
  on the local stack. A value above 0 means that the two clocks disagree.
- **`commit_times_missing`**: the primary events with no commit time. They get no latency from
  commit.

Every percentile in the harness is the lower nearest rank: the value at index
⌊p/100 × (n − 1)⌋ of the sorted values.

## The step directory

| File | What it holds |
| --- | --- |
| `summary.json` | The step's results. Every table cell comes from it (The tables, below). |
| `TABLES.md` | The tables of this step. `loadgen tables` writes it. |
| `meta.json` | The step name and arguments, `start`, `window_start`, `window_end`, `end`, `nproc`, CPU model, kernel, the load average before the step, `GOMAXPROCS` of Dispatch (`unset` by default), the Postgres version, the build commit and `build_dirty`, the seed meta, the `stack.json` path, `pg_direct`, `allow_foreign` and the pids. |
| `run.log` | `loadgen run`'s log: the configuration, the guard, the sampler's pids, the NATS subscriptions, the registrations, the window, failed writes and the result line. |
| `samples.json` | The 5 s samples: CPU ticks and RSS per process, `/proc/stat` ticks, Postgres connections by state, goroutines, pool waiters, heap (every 15 s), NATS `/varz`. |
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

`TABLES.md` has five tables, with one row per step. The window is the measured window
(`--duration`). "Cores" means CPU-seconds per second. A cell is `-` when it has no value. A JSON
path below is a field of the step's `summary.json`.

### Events, delivery and requests

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name. | `config.step` |
| `start (UTC)` | date and time | The start of the window. | `meta.json` `window_start` |
| `sessions` | count | The simulated sessions. | `config.sessions` |
| `events/s target / offered / committed` | writes per second | The target rate, then the offered writes and the committed writes, each divided by the window. `0` when the target is 0. | `config.events_per_s`, `events.offered_per_s`, `events.committed_per_s` |
| `write errors non-2xx / transport` | count | The offered writes that got a status outside 2xx, then the offered writes that ended with no complete response: a connection error, the 30 s client timeout, or a failed read of the spec before an upload. | `events.write_errors.non_2xx`, `events.write_errors.transport` |
| `delivery ms from arrival p50 / p99` | ms | Delivery latency from arrival, over every issue-topic receipt of a primary event. | `events.delivery_ms_from_arrival` |
| `delivery ms from commit p50 / p99` | ms | Delivery latency from commit, over the same receipts. | `events.delivery_ms_from_commit` |
| `issue-topic receipts got / expected` | count | The issue-topic receipts of primary events, then the expected number. | `events.issue_topic_receipts`, `events.expected_issue_topic_receipts` |
| `req/s target / sent` | requests per second | The target rate, then the requests sent in the window divided by the window. `0` when the target is 0. The mark `(below 95%)` follows when the sent rate is below 95% of the target. | `config.requests_per_s`, `requests.sent_per_s`, `requests.below_target` |
| `req 2xx` | percent | The share of sent requests that got a 2xx status. | `requests.ok_2xx` ÷ `requests.sent` |
| `req ms p50 / p99` | ms | Request latency, from arrival (as for a write) to the end of the response body, over every sent request, failures included. A failed request counts with the time it took to fail: about 30,000 for the client timeout, and about 5,000 for the 5 s connect timeout. A request that never connected has no arrival, so its time starts when the client began the request. | `requests.latency_ms` |
| `requests dropped at the in-flight cap` | count | The requests scheduled in the window that the generator did not send, because 25,000 were in flight. | `requests.dropped_inflight_cap` |

**The below-target mark.** `(below 95%)` in `req/s target / sent` means that the step sent fewer
than 95% of its target requests per second. Its request columns then describe a lighter load than
its name says. `TABLES.md` repeats this under the table when a row has the mark.

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
| `step` | name | The step name. | `config.step` |
| `comment ms p50 / p99 (committed / offered)` | ms, then count | Write latency of the kind's writes that got a 2xx status: from arrival to the end of the response body. Then the committed and offered writes of the kind. `-` alone when the step offered no write of the kind. `-` before the counts when no write of the kind got a 2xx status. | `events.by_kind.comment.write_ms`, `.committed`, `.offered` |
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
| `step` | name | The step name. | `config.step` |
| `Dispatch cores mean / peak 5 s` | cores | Dispatch's mean cores in the window, then its highest cores in one 5 s sample interval. | `resources.cpu_cores_mean.dispatch`, `resources.cpu_cores_peak_5s.dispatch` |
| `Postgres cores` | cores | Mean cores of the stack's Postgres: the postmaster, its live children and the postmaster's time for children that exited. | `resources.cpu_cores_mean.postgres` |
| `listener cores` | cores | Mean cores of the listener. | `resources.cpu_cores_mean.listener` |
| `NATS cores` | cores | Mean cores of `nats-server`. | `resources.cpu_cores_mean.nats` |
| `loadgen cores` | cores | Mean cores of the `loadgen run` process: the generators, the sessions and the samplers. | `resources.cpu_cores_mean.loadgen` |
| `proxy cores` | cores | Mean cores of the latency proxy. | `resources.cpu_cores_mean.proxy` |
| `box busy` | percent | The share of the box's total CPU time (all CPUs) that was not idle or I/O wait, from `/proc/stat`. | `resources.sys_busy_frac` |
| `Dispatch RSS max MiB` | MiB | Dispatch's highest resident memory (`VmRSS`) in a sample. | `resources.rss_mib_max.dispatch` |
| `heap in use MiB p50 / max` | MiB | Dispatch's `HeapInuse` from `/debug/pprof/heap?debug=1`, read every 15 s. | `resources.heap_inuse_mib` |
| `goroutines p50 / max` | count | Dispatch's goroutines, from `/debug/pprof/goroutine?debug=1`. | `resources.goroutines` |
| `goroutines waiting for a pool connection p50 / max` | count | Dispatch's goroutines with a pgx pool `Acquire` frame (puddle) in their stack. | `resources.pool_acquire_goroutines` |
| `Postgres connections max` | count | The highest number of `pg_stat_activity` rows for database `dispatch`. It includes the 2 connections of `loadgen`'s sampler and lock probe. | `resources.pg_conns_total.max` |
| `connections waiting on a lock p50` | count | The p50 of `dispatch` connections with state `active` and wait event type `Lock`, over the samples that saw at least one. `0` when no sample saw one. An advisory lock wait has this type. | `resources.pg_conns_by_state["active\|Lock"]` |
| `Dispatch CPU-s per operation` | CPU-seconds | Dispatch's mean cores divided by the operation rate. An operation is a committed write or a sent request in the window. | `resources.dispatch_cpu_s_per_op` |

`resources` also holds every process's RSS, Postgres connections by state, and NATS messages in and
out per second and slow consumers. Under `-allow-foreign`, `loadgen` has no stack pids, so only
the `loadgen` cores have a value.

### Commit-order lock

The lock probe reads every 20 ms from 20 s after the window start to the window end. When the
window is shorter than 20 s, it starts at the window start. The lock is Dispatch's database-wide
advisory lock `0x4449535041544348` (`internal/dispatch/events/broker.go`), which orders event
commits.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `step` | name | The step name. | `config.step` |
| `time held` | percent | The share of samples in which a backend held the lock. | `commit_order_lock.held_fraction` |
| `waiters p50 / max` | count | The backends that wait for the lock, per sample. | `commit_order_lock.waiters` |
| `hold ms p50 / p90 / p99` | ms | Hold durations. One hold is a run of samples with the same holder pid and transaction start. It lasts from its first sample to the sample that ends it. A hold that is open at the last sample is not counted. The resolution is the sample interval. | `commit_order_lock.hold_ms` |
| `holds seen` | count | The holds counted for `hold ms`. | `commit_order_lock.holds_observed` |
| `what the holder was doing (share of held samples)` | text, percent | Up to four holder states, most frequent first, each with its share of held samples. A state is `<state> (<wait event type>:<wait event>)`, or `active: <query>` with the query cut to 70 characters. `idle in transaction (Client:ClientRead)` means that Postgres waits for Dispatch to send the next statement. | `commit_order_lock.holder_doing` (up to 12 entries) |

### Requests by route

There is one row per route and one column per step.

| Column | Unit | What it shows | Source |
| --- | --- | --- | --- |
| `route` | name | The route name, `<caller> GET <path template>`. | the keys of `requests.routes` |
| `<step name>` | ms, then count | Request latency p50 / p99 of the route in that step, then (failures / count). A failure is a status outside 2xx or a transport error. The count is the sent requests of the route. `-` when the step sent no request on the route. | `requests.routes.<route>.latency_ms`, `.failures`, `.count` |

## The guard

`loadgen run` and `loadgen seed` read every endpoint and token from `stack.json`. Never point them
at the pod's live listener (`127.0.0.1:9020`) or at production. A step writes comments and
document versions at the rate it is given. The guard stops a mistaken target before the first
write:

1. An endpoint flag (`-url`, `-listener`, `-nats`, `-nats-mon`, `-pprof`, `-pg`, `-token`,
   `-listener-token`) that differs from `stack.json` is refused before any connection, unless
   `-allow-foreign` is given. The refusal names the flag and `-allow-foreign`.
2. Before its first write, `loadgen` checks:
   - NATS `/varz` `server_name` is the stack name.
   - Dispatch answers `GET /api/v1/projects` with the stack's random agent token.
   - The listener answers `/v1/sessions` with the stack's random API token, and refuses a random
     other token.

   A failed check exits before any write, and names the check.

The pod's live listener and production Dispatch hold other tokens, so a mistaken target fails the
check.

`-allow-foreign` is for a stack that is not local, such as a cluster copy (dispatch://INFRA-598).
With it, a given endpoint flag wins and an empty one falls back to `stack.json`. Give
`-nats-server-name` to keep the NATS check. The sampler gets no pids. `meta.json` records
`allow_foreign`.

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
- **Header identity.** A person's request names the person in `X-Dispatch-User`. Each such request
  also writes a `people` row. Production uses cookie identity. Agents use the shared bearer token
  and a session actor.
- **No embeddings.** Dispatch gets no AWS credentials or region. Meaning search is unavailable, and
  search falls back to keyword.
- **A smaller dataset.** 1,000 issues and about 10,000 events at the start of each step.
- **No browser documents.** No document websockets and no dashboard refetch fan-out. The browser
  share of the route mix is plain GETs. The SSE streams read `/api/v1/events` and do nothing more.
- **One listener, sessions on NATS.** The sessions receive from NATS directly, as the pi plugin's
  self-subscription does. They register with port 0, so the listener delivers nothing to them.
- **One process each.** One Dispatch process, one NATS server and one listener, all on
  `127.0.0.1`. `GOMAXPROCS` is not set.

## Differences from the root report

Compare the right rows:

- 0.25x and 0.5x run 100 and 200 sessions (the spec's definition). The report ran 350 and 367.
- Every step starts from the seed. The report's database grew across steps, which it ran in the
  order 1x, 2x, 5x, controls, 10x, 1x-rerun, 0.25x, 0.5x.
- Delivery is reported from arrival and from commit (the spec's columns). The report had "after
  commit response" and "after event created_at".
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

- A cluster run (dispatch://INFRA-598) can use `loadgen run -allow-foreign` with explicit endpoints
  and its own seed. It gets:
  - no `/proc` CPU or RSS columns, because it has no pids,
  - no delivery latency from commit, unless its database has the trigger and
    `track_commit_timestamp = on`,
  - no reset per step.
- These belong to INFRA-598: 10-minute steps, one-hour soak trends (the 5 s series in
  `samples.json` is there for them), the replay of production's log, sampling of the production
  database and restarts during load.
- INFRA-605 adds its browser-editor workload, its round-trip counter and its document-room sampler
  to this package.
- INFRA-512's request generator (dispatch://INFRA-512) is to be built into this package, so that
  one generator remains. The route-mix file and `routes.go` are this package's own GET generator
  until then.
