# Testing

Rules for this repository's tests that the code under test does not show.

## The fixtures a test takes rather than copies

Each of these is one body per Go module, and a test that needs what it does calls it rather than writing the body again.
Each bullet says what the helper does not do, so a site that must keep its own body is not folded into it.

- A test that captures a log takes its buffer from the module's own: `packages/daemon/internal/testlog`'s `Buffer`, and `packages/envoy/internal/logtest`'s, which lives beside that package's default-logger swap.
  `packages/envoy/internal/cmdtest`'s `LockedBuffer` is an alias of it and `storetest`'s `LockedLog` embeds it, so either name reaches the same body.
  It is a mutex around a byte sink and nothing else: a type that needs a reader of its own embeds it and keeps that reader, and a writer that is no log sink — an `http.ResponseWriter`, a TCP relay, a writer that gates what it passes on — keeps its own body.

- A bounded wait is the module's `internal/testwait`.
  `Eventually` is the wait a test usually wants, a minute or the binary's own deadline less a second, at the package's pace, failing with what it waited for; `WithinPolling` takes a bound and a pace of its own; `Holds` answers rather than fails, for a caller whose failure text is its own diagnostic dump or is built at give-up.
  A loop that is not a wait-for keeps its body: one that runs its check for a whole span rather than stopping at the first success, one that sends or serves on a tick, one that drives a stand-in process's own main loop, and one that retries a compare-and-set.

- A fixture that runs git or jj takes its runner and its `init` argv from `packages/daemon/internal/fixturegit`.
  `Runner` holds the test and the child's whole environment — `Isolated(t, home)` is the usual one — and runs one command four ways: `Run` trimmed, `Raw` untrimmed, `Out` for standard output alone, and `Try` for a caller that keeps the error.
  `InitBare`, `InitWorkTree` and `JJInit` build the whole argv, and the semantic axes stay the call site's: bare or not, the initial branch, and colocation, with only the quiet spelling normalised.
  The runner chooses no binary — `argv[0]` does, so a fixture that runs a pinned git or jj passes that path itself.
  `packages/envoy` has no Go git or jj fixture runner, so this one is the daemon module's alone.

- A fake GitHub server is the module's `internal/githubtest`: `Server` for a fake its caller reaches by URL, `Serve` where the caller needs the server itself, and `Unexpected` as the default arm of a fake for which an unplanned request is a failure.
  `Spec` carries the opt-in bearer gate and the opt-in request recorder.
  The `switch`, its `default:` arm and the fake's own state stay the caller's, because they are what each test proves: a fake that reads the per-owner token the daemon minted must answer a request it then reads, and a `default:` that logs, that answers 400 or that is the happy path says something a shared arm would take away.

- An in-test `go build` in `packages/envoy` goes through `internal/cmdtest`: `Build`, `BuildStamped`, or `BuildCommand` for a build that needs a directory, a target, flags or an environment of its own.
  One body holds `ENVOY_TEST_GO_OVERLAY`, the output under the test's own temporary directory and the failure.
  A `go build` that produces no binary, such as a compile check of a generated module, is not a build of this kind.

## Unix sockets in Go tests

A unix socket path must be 107 bytes or shorter. Linux's `sun_path` holds 108 bytes with the
terminating zero, and Go refuses a longer path with `invalid argument` at `bind`, `listen` or
`dial`. A tmux server socket has the same limit (`Filename too long`). `t.TempDir()` puts the
test's name, cut to 64 bytes, in its path, under `GOTMPDIR` or else `TMPDIR`. With a long test name
and a socket under it, or under a directory made from it (an `XDG_RUNTIME_DIR`, `HOME` or state
directory a test sets to `t.TempDir()`), a temporary directory of about 20 bytes is enough to pass
the limit, as on a dev box whose `TMPDIR` is on a data disk.

- Take a test's socket directory from the module's `internal/testtmp`, the one helper every test
  that binds a socket calls (`packages/daemon/internal/testtmp`, and the same file in
  `packages/envoy`). `testtmp.Dir(t, longest)` is a fresh directory of the test's own, removed
  when the test ends, where `longest` is the longest path the test puts inside it, written
  relative to the directory (`"g9/.s.PGSQL.5432"`); `testtmp.Socket(t, name)` is the path of a
  socket named `name` in such a directory. `Dir` makes the directory under `TMPDIR` and measures
  what `longest` would come to there; only when that passes 107 bytes, or `TMPDIR` holds no
  directory at all, does it fall back to `/tmp`, and it says so with `t.Logf` when it does. Each
  module's `internal/testtmp` keeps a test that fails a new `os.TempDir()`, or a new
  `os.MkdirTemp` whose first argument is a string literal naming a directory, anywhere in that
  module's tests. It reads no other shape: a directory named by a variable or a constant, or an
  `os.CreateTemp`, is left to review.
- Two kinds of site keep a directory of their own on purpose.
  `packages/daemon/internal/shim/socketpath_test.go`'s `roleTempDir` builds a deliberately long
  one, as long as the `TMPDIR` a Legion role is told, so that its test proves sockets still bind
  where a role runs; a short directory would take the proof away. The once-per-binary
  directories a package's `TestMain` owns (the build directories of `internal/daemon`,
  `internal/stream` and `internal/runtime/tmux`, the jj configuration directory of
  `cmd/legion/main_test.go` and the home of `internal/workspace/workspace_test.go` in the daemon
  module; the sign-in secret directory of `internal/broker/config` and the certificate directory
  of `infra/seed/main_test.go` in the envoy module) bind no socket, so they need no byte budget,
  and no `*testing.T` outlives them, so they keep `os.MkdirTemp("", …)` and a removal in that
  `TestMain`.
- A test changes a process variable, `TMPDIR` above all, only through `t.Setenv`, and a test
  that must unset one calls the module's `fixtureenv.Unset`, which goes through `t.Setenv`,
  never a bare `os.Setenv` or `os.Unsetenv`. `t.Setenv` puts back what the process held when the
  test ends, and Go refuses it in a parallel test or under a parallel parent, while a parallel
  test runs only beside other parallel tests, so no test can read a value another test changed.
  A bare call has neither fence.
- Before you call a test that binds a socket done, run it with a temporary directory of about 64
  bytes. A pass under `/tmp` does not show that the test works under a longer one. Set `GOTMPDIR`
  as well, since `t.TempDir()` reads it first:

  ```sh
  L=/tmp/a-deliberately-long-tmpdir-for-unix-socket-path-tests-0001 && mkdir -p "$L"
  TMPDIR="$L" GOTMPDIR="$L" go test ./<package>/... -count=1
  ```

## Go's package deadline

`go test` panics a package that runs longer than ten minutes (`panic: test timed out after 10m0s`),
which reads as a failure of the change under test. `packages/envoy/internal/dispatch/api` alone
takes close to that, about 600 s on a workstation, so every command that runs that package, on its
own or in a pattern that includes it (`./...`, `./internal/dispatch/...`), passes `-timeout 20m`:

```sh
cd packages/envoy
go test -timeout 20m ./internal/dispatch/api
```

A Legion issue pod does not run that whole package: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

CI's `envoy-go` job runs it that way, alone and before the module's other packages
(`.github/workflows/ci.yaml`), and `envoy-go-race-api`, the race job that runs that package by
itself, gives it 30 minutes; `docs` runs under `-race` in `envoy-go-race-docs`, which gives it 60.
A command that runs one of its tests by name (`-run`) needs no deadline of its own. Run it alone
rather than beside the module's other Postgres-backed packages: together they share one database
service and the machine's CPU, and on a small machine `api` and `docs` then each overran the default
(`docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md`).

`packages/daemon/internal/daemon` is the daemon's slow, Postgres-backed package. The test
job now runs it only under the race detector, in its race step, once per job; the plain step
("Test the Go daemon") leaves it out, and `go vet -tags e2e ./...` in the `typecheck` job still
compiles it without the detector. On CI's runners, while the plain step still ran it, it took
at most 625 s there over the 49 green main `test` jobs of 2026-11-01 to 2026-11-03, and up to
733.581 s in the jobs whose race step timed out (job 125688799071). Under `-race` it took up
to 570 s over the 51 green `test` jobs of 2026-11-02 and 2026-11-03 that sjawhar/legion#2709
measured, on main and on pull requests, and ran go test's own ten minutes out on main at
job 125691048644 of run 41562777668; the race step gives it 30 minutes. On a busy machine it
takes longer still (908 s under `-race` on a shared devbox), so a command that runs the whole
package, alone or in a pattern that includes it, passes `-timeout 30m`:

```sh
cd packages/daemon
go test -race -timeout 30m ./internal/daemon/
```

A Legion issue pod does not run that whole package either: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

## A cleanup's closure or argument holds nothing that reaches its object

A `runtime.AddCleanup` or `runtime.SetFinalizer` closure captures, and its argument carries, nothing that reaches the object the cleanup is attached to.
`runtime.AddCleanup` says what that costs: "If ptr is reachable from cleanup or arg, ptr will never be collected and the cleanup will never run."
The runtime keeps a cleanup's closure and its argument alive for as long as the object it is attached to, so either of them reaching that object makes the object immortal, and everything the object holds with it.
In `packages/envoy/internal/dispatch/docs` the object is a room's live document, and two closures held the service and one of its listings, each of which reaches every resident document through ygo's room table:
every room the tests loaded then stayed in memory with its replica and its trees, which is what took the `envoy-go-race-docs` job over its runner's memory (dispatch://INFRA-1085).

Hold the owner through a `weak.Pointer` instead, and do nothing when its `Value()` is nil, as that package's `keepInstance` (`roomcursor.go`) and `listResident` (`tree.go`) do: the cleanup drops its entry while the owner is alive, and an owner that is gone has no map left to drop it from.
Hold weakly what the argument would carry as well, and pass `struct{}{}`, which reaches nothing, as `keepInstance` does with the room load its cleanup once took as its argument.
A pointer to a field of the owner counts as the owner, since a field and the value holding it are one allocation.
`weak.Make` takes such an interior pointer and that weak pointer dies with the whole allocation, which is how `listResident` holds the listing it was given, `Service.instances` or `Service.replicas`.

Go's own detector finds a closure that reaches its object:

```sh
cd packages/envoy
GODEBUG=checkfinalizers=1 go test -count=1 -timeout 29m ./internal/dispatch/docs/
```

It prints `WARNING: LIKELY CLEANUP/FINALIZER ISSUES`, then one `is reachable from cleanup or cleanup argument` line per site naming that site's file and line, and ends the run with `fatal error: detected possible issues with cleanups and/or finalizers`.
The rule covers the cleanup's argument as well as its closure, so an argument that reaches the object is the same defect: the room load `keepInstance` passed reached a document of its own, which is the finding this job made on a head where the closure itself was already weak, and the comment on that cleanup names the path it took (dispatch://INFRA-1085).
A run that reports nothing is no proof on its own: the check runs at a garbage collection, so a short run can pass with the path present.
`GOGC=10` beside the detector makes a single test likelier to fire it, not certain, so repeat the run before you read a quiet one as a clean one: with this fix reversed, `TestALiveWritesAskBlockKeepsItsWriterWhenAnotherProcessSettlesIt` under `GOGC=10 GODEBUG=checkfinalizers=1` fired in 2 of 3 runs for the implementer and in 1 of 6 for the reviewer, and at the fixed head it was quiet in 9 of 9.
CI runs that command in `envoy-go-docs-finalizers` (`.github/workflows/ci.yaml`), on every change the `envoy_go_race_docs` filter selects.

That job runs the command with `-v` for a second check Go's detector does not make:
the package's `TestMain` prints `docs: <built> services built, <alive> still alive after <duration>` once the tests have run, and the line after it names each test that built a service still alive at the end of the run.
A test that builds a `*Service` gives it a `t.Cleanup` that shuts it down.
A service a test left running holds its rooms, and their documents, for the rest of the binary's run.

`packages/envoy/internal/dispatch/docs` is the only package of this repository that registers a cleanup today, and no repository-wide check holds that true:
`grep -rn "runtime.AddCleanup\|runtime.SetFinalizer" --include=*.go .` finds those two sites and nothing else.
A `runtime.AddCleanup` or `runtime.SetFinalizer` added in another package comes with a detector job of its own, in the same pull request.

## A database a Go test creates

A Go test that creates a Postgres database writes `create database … strategy file_copy`, as
`packages/envoy/internal/dispatch/store/storetest` and both modules' `internal/testpg` do. Without
it, PostgreSQL 15 and later copy the source database through the write-ahead log, every page of it:
4.4 MB of log for an empty database made from `template1`, and 6.4 MB for a clone of Dispatch's
migrated template, which made one run of the `api` and `docs` packages write 16 GiB of log. A file
copy writes a few hundred bytes. `storetest`'s own test fails when a clone writes more than a
hundredth of its template's size, and each module's `internal/testpg` test
`TestEveryCreatedDatabaseIsAFileCopy` fails on any line of the module's Go files that opens a
database's creation in a string without `strategy file_copy` on the same line.

A test that needs the schema takes a clone of a template its package migrated once, rather than
running the migrations itself: `packages/daemon/internal/testpg`'s `Clone` answers the clone's
URL, `ClonePool` an open `pgxpool` on it and `CloneStore` the opened store, each dropped when the
test ends, and the package's
`TestMain` is `testpg.Main("<prefix>", m.Run)`, which drops the template when the run ends and
composes with the `TestMain` a package already has
(`testpg.Main(prefix, func() int { return testnats.Main(m) })`).
`packages/envoy/internal/dispatch/store/storetest`'s `Open` is the same fixture for Dispatch's
schema.
`Database` is still there for a test that wants an empty database: `internal/store`'s and
`internal/record`'s own tests apply part of the migration set by hand and assert what `Migrate`
then returns, which a migrated clone would not let them do.

A test that needs one of envoy's binaries takes it from
`packages/daemon/internal/testbuild`'s `EnvoyBinary`, which builds each command once per test
process and removes the directory when `testbuild.Main` ends the run.

A test changes no server setting (`fsync`, `full_page_writes`, `max_wal_size`): the server
`DISPATCH_TEST_DATABASE_URL` names can hold other databases, such as the local Dispatch's on
`packages/envoy/scripts/dev-postgres.sh`'s container.

## A test that reads `GET /api/v1/references?since=` reads again

A since read answers only the edges at or below the event log's safe horizon (`getReferences` and
`sinceHorizon` in `packages/envoy/internal/dispatch/api/references.go`), and waits at most
`referencesSinceWait`, one second, for a horizon above the events committed before the read. Past
that wait it answers at the horizon it has and leaves the rest to a later read: the route defers,
it never loses an edge. The horizon is instance-wide,
so any transaction holding a transaction id on the Postgres instance holds it back, and in
`packages/envoy/internal/dispatch/api` the holder is the test process itself: `storetest.Open`'s
`create database … template … strategy file_copy` and `storetest`'s
`drop database … with (force)` (`packages/envoy/internal/dispatch/store/storetest/storetest.go`).

So a read that answered within `referencesSinceWait` was not held back: what it listed is what it
owed, and a wrong answer fails the test at once. Only a read held past that wait is read again,
bounded by `eventFeedWait`. The helper is `awaitSinceRead`
(`packages/envoy/internal/dispatch/api/references_test.go`), which takes the read as a function, so
each call site keeps its own reader.

A test that takes one read as final fails under a transaction someone else holds:
`TestAnAnsweredAsksPartEdgesAreListedSince` failed about one run in six on main, 7 of 40 measured
for dispatch://INFRA-592, and in this repository's own pod on 2026-11-02 it failed 15 of 40 runs
with a transaction held 2 s of every 2.5 s; under that same pulse, reading again, it passed 40 of
40.

Prove such a test under a loop that holds the state, as
`docs/solutions/testing/code-that-waits-on-instance-wide-database-state-is-tested-under-a-loop-that-holds-that-state-pr2392.md`
prescribes:

```sh
while true; do
  psql "$LEGION_TEST_PG_DSN" -Atc 'select pg_current_xact_id(), pg_sleep(2)'
  sleep 0.5
done
```

The holder must release each transaction: one that never commits holds the horizon back for good,
so no bounded re-read succeeds under it and the test fails after `eventFeedWait`, naming the hold.

## A writer a test holds

A test that needs a live socket's writer held, so that frames queue behind it while the test
sends the socket more frames, holds the writer itself: it builds its rig on a `heldWrites`
listener (`packages/envoy/internal/dispatch/api/live_socket_test.go`), calls `hold`, and waits
with `waitHeld` until a write waits at the gate. Small TCP buffers and a page that reads nothing
(`smallSendBuffers`, `smallReadDialer`) stall the writer only until the transport lets its write
through, and the transport decides when that is. Under gVisor, the runtime of Legion's worker
pods, a write blocked for a third of a second completed the moment the page sent its next frame.
`TestARepeatedSubReplacesAnotherKind` then waited for a queued frame the writer had already
written, and failed 8 of 300 runs. Those two helpers are for tests of a slow page itself, such as
one whose queue overflows.

## A test that starts jj or git names a working directory

jj searches upward from the directory it runs in for the closest `.jj` and opens that repository, and it has no ceiling option.
`-R` names the workspace a command acts on, and the two that create a repository, `jj git clone` and `jj git init --colocate`, take no `-R` at all.
`-R` does not keep jj off the repository its working directory sits in: jj 0.45.1 loads that repository's configuration before it applies `-R`, and `jj -R <b> log` run inside a repository `a` that still held a legacy `.jj/repo/config.toml` migrated it, writing `a/.jj/repo/config-id`.
Every `-R` call site of `packages/daemon/internal/workspace` is safe because its `Dir` **is** its `-R` target, not because the flag fences jj in: `runCheckedOnClone`'s comment states that invariant, where the directory a command runs in is chosen.
A child process started with no working directory of its own inherits the test process's, which in a Legion issue pod is inside `$LEGION_WORKSPACE`, a jj workspace of the pod's real shared clone under `/legion/repos`.
So a test whose jj child names no directory opens and writes that clone, and the same test is green on a GitHub runner, whose checkout is git-only with no `.jj` anywhere above it (dispatch://INFRA-623).

- Set `Dir` on every `exec.Command` a test starts that runs `jj`, and on every one that runs `git`, to a directory the test made.
  A `--git-dir` is not enough on its own: it tells git which repository to act on and leaves the child standing wherever the test process stands, which is what the next reader of the command has to work out again.
  `Dir` is not enough on its own either, and this is the step the rule stopped short of: an inherited `GIT_DIR` names the repository git acts on whatever directory the child stands in, so it outranks `Dir` and `-C` alike, and a Legion issue pod hands every process such a block.
  So a test that starts git takes those variables out of the child's environment as well as placing it: `fixtureenv.Guard` in the package's `TestMain` for a child that inherits `os.Environ()`, or `fixtureenv.Isolated` for one given an environment of its own.

- The two probes that got this wrong were copies of one helper, now `testbin.RequireJJFilters` in `packages/daemon/internal/testbin`, each running `jj config list` with no `Dir`.
  A probe is as much a jj child as a fixture is.

- A test that drives `packages/daemon/internal/workspace` needs nothing of its own: `execRunner.Run` refuses a `Command` whose `Dir` is not absolute, and refuses a jj command that creates a repository when that directory, or any above it, holds a `.jj` directory (`refuseEnclosingWorkspace`).
  `TestProvisionLeavesTheEnclosingRepositoryUntouched` holds both on a runner, by provisioning from inside a decoy repository.

- A test that builds a repository of its own puts it under `t.TempDir()`, and `TMPDIR` must sit inside no checkout.
  Nothing refuses the fixtures' own `jj git init --colocate` (`runSetup`, `enclosingRepository`): it runs outside the runner, where `refuseEnclosingWorkspace` never sees it, so under a `TMPDIR` inside a checkout it would read that checkout's repository's configuration first, and migrate it when that repository still holds a legacy `.jj/repo/config.toml`.

## A test that starts jj or git inherits no pod's environment

A Legion issue pod hands every process its live workspace (`LEGION_WORKSPACE`), its git configuration (`GIT_CONFIG_*`), its GitHub credential, the deployment's own session store (`OMP_SESSION_SQL_DSN_FILE`, `OMP_SESSION_STORAGE`) and more.
A test that let git or jj inherit them read and pushed the pod's own work: the shim's rescue test, run with no `LEGION_WORKSPACE` of its own, pushed the pod's workspace to the real origin as `legion/WIDGETS-42-rescue-…` branches.

- A `packages/daemon` package whose tests run git or jj, as children or in-process through the code under test, calls `fixtureenv.MustGuard` in its `TestMain`, after any branch where the test binary runs as a helper child.
  MustGuard is Guard with a `TestMain`'s only answer to a failure: it prints it and ends the process with status 1, which `packages/daemon/internal/fixtureenv`'s own test holds, with the guarantee Guard gives every package (`fixtureenv.RequireGuarded` stays for the test that needs the check in its own body).
  Such a package is also a line of `guardingPackages` in `packages/daemon/internal/fixtureenv/guardingpackages_test.go`, which fails when a listed package's `TestMain` calls no `MustGuard`, calls it only inside a branch, runs the tests before it, or runs them under a branch whose condition does not say a helper-child marker is set (`os.Getenv(<name>) != ""`, or `if _, ok := os.LookupEnv(<name>); ok`), since every ordinary run of the package takes such a branch too.
  It also fails a `TestMain` that a build constraint keeps out of the default `go test` build, and a package of the module whose `TestMain` calls `MustGuard` while the list does not hold it, so adding a package to the guard adds it to the list.
  No test can see another package's process, so that list is the one check of every package's guard.
  Guard unsets every `LEGION_*`, `GIT_*`, `JJ_*`, `GH_*` and `GITHUB_*` variable, `SSH_AUTH_SOCK` and `SSH_ASKPASS`, and the three names that place an Oh My Pi session (`OMP_SESSION_SQL_DSN_FILE`, `OMP_SESSION_STORAGE` and `OMP_SESSION_SQL_DSN`, which carries the same URL by value), keeping the tests' own inputs (`LEGION_TEST_*`, `LEGION_E2E*`, `GITHUB_ACTIONS`, and the names the package passes), and sets `GIT_ALLOW_PROTOCOL=file`, which a repository's own `protocol.<name>.allow` cannot override.
  The session store a pod names holds the conversation of every agent of the deployment, so a test that opened it would read and write what those agents are reading and writing.
  Code under test that names its own transport, as provisioning's `GIT_ALLOW_PROTOCOL=https` does, still uses it.
  Guard does the same under the `e2e` build tag, whose untagged tests are the same fixtures.
  A live proof of that build, which reaches a real cluster and GitHub on purpose, calls `fixtureenv.Live(t)` first in its entry point: that gives the test the process's own values back of everything Guard took out or set, until the test ends.

- A fixture command runs under `fixtureenv.Isolated(home)`: the same variables dropped, a home of the test's, and an identity.
  A package whose code under test reads the process's home sets `fixtureenv.Home` on the process and gives its fixtures that same home (`packages/daemon/internal/workspace`'s `setupEnvironment`).
  Isolated keeps no test input either, so a fixture that needs one appends it after.

## Containers a test starts

Every container a test or a proof script starts carries a CPU limit and a memory limit sized to its
work, and is removed when the test stops, except kind's node containers (`scripts/e2e/README.md`
says why) and the Pulumi containers of `packages/envoy/infra/host/live_test.go`, whose released
`Listener` component has no CPU or memory inputs. Tests and the live proofs run on shared dev boxes
as well as on CI runners, and an unlimited server can take every core and all the memory of a host
that other work shares.

- In Go, set `Resources.NanoCPUs` and `Resources.Memory` in the container's host configuration:
  through `testcontainers.WithHostConfigModifier` for a `Run`, or as the request's
  `HostConfigModifier` for a `GenericContainer` (`packages/envoy/internal/testpg/testpg.go` is one).
  `WithHostConfigModifier` replaces any modifier an earlier option set, so a test that needs a host
  configuration of its own sets the limits in that same modifier.
- In a script, `docker run` takes `--cpus` and `--memory` before the image. A Postgres whose data
  directory is a `tmpfs` mount counts that data against its memory limit. `scripts/e2e/README.md`
  lists each proof's containers and their limits.
- Remove a container with its anonymous volumes: `docker rm -f -v`, since the Postgres images
  declare a `VOLUME` that a plain `docker rm -f` leaves on the host.
