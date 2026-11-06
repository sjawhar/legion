# Testing

Rules for this repository's tests that the code under test does not show.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts docs/testing.md` checks the layout, and `--write` fixes it.

## Bun tests

`bun test` runs every file of a package in one process, so what one file leaves behind is what the next file reads:
a directory, an environment variable, a port, a container, a cache.
These rules are how a file leaves nothing behind.
`@legion/test-fixtures` holds the shared pieces, and eight packages depend on it: broker-client, claude-envoy, contracts, dispatch, envoy-client, pi-envoy, pi-legion and pi-shared.
The two Bun packages that do not are the fixture package itself, whose tests import its sources by relative path, and `packages/proof-editor`, whose tests make no directory, bind no port and write no environment variable.

- Take a temporary directory from `@legion/test-fixtures/temporary-directories`, never from an `mkdtemp` with a cleanup of your own.
  One directory for a whole file or `describe` is `temporaryDirectory(prefix)`: call it at the test file's module scope, or in the body of a `describe`, never inside a test body.
  It reserves the path at the call and returns it, and registers the `beforeAll` that makes the directory and the `afterAll` that removes it where it is called.
  A directory per test is `temporaryDirectories(prefix)`, registered in those same two places, which returns a maker that makes a directory at each call and removes them in its `afterEach` — or, with `scope: "suite"`, its `afterAll`.
  Call that maker from a test body or a hook only: it makes its directory at the call, where the one-shot form makes nothing until its hook runs.
  `bun:test` takes a hook's scope from the point of the call, and Bun 1.4.4 evaluates a module body and a `describe` body in three cases where it then runs no hook that body registered: a `bun test -t <pattern>` that selects no row of the file, one that selects no row of the `describe`, and `describe.skipIf(true)`.
  Nothing catches what such a run leaves: under `bun test` neither `process.once("exit")` nor `"beforeExit"` runs.
  So anything a file creates, sets or writes at module scope or in a `describe` body survives a filtered run, and a directory must be made where the hooks run.
  Write into a reserved directory from a test body, or from a hook registered below the reservation, since nothing is at the path until that `beforeAll` has run and Bun runs a scope's hooks in the order they were registered.
  A `describe` whose rows are each `.skip` runs both its `beforeAll` and its `afterAll`, so a directory reserved in its body is made and removed as usual; `describe.skipIf(true)` runs neither, so that directory is never made (`packages/test-fixtures/src/temporary-directories.test.ts` holds both, and `filtered-run.test.ts` runs a file under a filter that selects nothing and reads its `TMPDIR` afterwards).
  The fixture's own module body registers no hook, and cannot:
  Bun evaluates an imported module once per process, so a hook there would take the scope of whichever test file imported the fixture first, and every other file's directories would outlive their tests.
  `temporaryTmpdir(prefix)` (`@legion/test-fixtures/temporary-tmpdir`) is the same shape for a whole file's `TMPDIR`: it reserves one root, and makes it and points `TMPDIR` at it in a `beforeAll` whose `afterAll` puts the name back and removes the root.
  Anything staged under that root is staged in a hook too, since the name holds the caller's value until that `beforeAll` has run.

- Put a variable back with `restoreEnvironment` or `captureEnvironment` (`@legion/test-fixtures/environment`), which set and delete the names themselves.
  A name that held no value is captured as unset and the restore deletes it again;
  setting it to the empty string instead would leave the next reader a value its caller never had.
  A file that loads a plugin entry captures the names that entry writes above the import that loads it:
  `packages/pi-envoy/extensions/envoy.ts` puts its own `bin/` first on `PATH`, sets `DISPATCH_HOST` and picks `ENVOY_CONFIG_FILE` when its module loads, and a capture below that import would put back what the entry wrote rather than what the caller had.
  Never restore by replacing the whole object (`process.env = { ...saved }`):
  that swaps the runtime's environment for a plain one, and after it `process.env !== Bun.env`, a write reaches neither `Bun.env` nor a child process the test spawns, and the names the test deleted stay deleted in the real environment.
  Under `--parallel`, which implies `--isolate`, the next file gets a fresh `process` and never sees the swap, so the damage shows only where it is hardest to read.

- A suite that runs jj or git takes its isolation from `versionControlEnvironment` (`@legion/test-fixtures/version-control-environment`): a home of the suite's own with a jj configuration file in it, no git configuration at all (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`), no credential prompt, and the identity jj commits as.
  So neither the developer's `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor the system gitconfig decides a result.
  A suite whose code under test starts jj with the process's own environment puts those names on `process.env` itself, and hands `versionControlNames` to `restoreEnvironment` so the next file finds its own again.

- Take a port from the kernel: bind port 0, read back the port the listener reports, and hold that listener from the moment it binds, so nothing else on the machine can take the port between the question and the row that asks it (`stubBroker` in `packages/envoy-client/src/__tests__/broker-credential.test.ts`).
  Never write a port number into a test.
  The Dispatch e2e harness is the same rule at the scale of a run:
  a port variable left unset is port 0, one that is set is used as given, and two variables naming one port are refused where the ports resolve rather than by whichever listener loses the race (`harnessPort` and `harnessPorts` in `packages/dispatch/e2e/harness-ports.ts`).

- A suite that needs a program this host may not have skips with that program's name.
  Call `skipWithout("docker")` once, at the file's module scope, and hand the answer to every `describe.skipIf` and `test.skipIf` of the file:
  it prints one line naming the missing programs and the file whose tests did not run (`@legion/test-fixtures/host-tools`), and a call per test would print that line per test.
  A suite that needs a service does the same with the variable that names it, and makes a database of its own rather than reading the one it finds (`LEGION_TEST_PG_DSN`, `packages/dispatch/e2e/__tests__/psql-session.test.ts`).

- Start a container with `docker run --rm` and register its removal the moment `docker run` returns, not once the whole start has succeeded:
  a start that throws half-way has already made the container (`startNats` in `scripts/e2e/lib/nats-stream.test.ts`).
  Remove it with `docker rm -f -v`, and give it the CPU and memory limits "Containers a test starts" below requires.

- Two caches stay where their tool puts them, under the user's cache directory, and this is the one rule here that holds only under a condition.
  Oh My Pi's native modules, about 116 MiB a pin, are hardlinked into each test's fresh HOME from one copy per binary at `$XDG_CACHE_HOME/legion/test-omp-natives`, or `$HOME/.cache/legion/test-omp-natives` where `XDG_CACHE_HOME` is unset (`nativesCacheRoot` in `packages/pi-shared/test/omp-natives.ts`);
  `nativesCache` in `packages/daemon/internal/testbin/natives.go` computes that same path with `os.UserCacheDir` and `OMPHome` hardlinks out of it, so moving the Bun side alone would split a cache that exists to be shared by both languages and either fills for the other.
  The Go build and module caches of `scripts/e2e/lib/held-port.test.ts` and of the `go run` in `runDaemonPane` (`packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts`) stay for the same reason:
  CI's `test` job restores `~/.cache/go-build` and `~/go/pkg/mod` in its `go-cache` step, keyed `go-daemon-<runner os>-go<version>`, immediately before the step that runs `bun test scripts/e2e/lib` (`.github/workflows/ci.yaml`).
  The condition is the rename: `fillNativesCache` in `packages/pi-shared/test/omp-natives.ts` renames a directory inside the cache root, and a directory rename under `$HOME` panics the gVisor sandbox of a Legion pod and kills every role in it.
  So the permission holds only while `XDG_CACHE_HOME` is honoured and the pod points it at its own volume, which Legion's runtime does for every role (`/legion/scratch/cache`).
  With `XDG_CACHE_HOME` unset in a pod, that cache is no longer a tool's cache but the pod's death.

## Unix sockets in Go tests

A unix socket path must be 107 bytes or shorter.
Linux's `sun_path` holds 108 bytes with the terminating zero, and Go refuses a longer path with `invalid argument` at `bind`, `listen` or `dial`.
A tmux server socket has the same limit (`Filename too long`).
`t.TempDir()` puts the test's name, cut to 64 bytes, in its path, under `GOTMPDIR` or else `TMPDIR`.
With a long test name and a socket under it, or under a directory made from it (an `XDG_RUNTIME_DIR`, `HOME` or state directory a test sets to `t.TempDir()`), a temporary directory of about 20 bytes is enough to pass the limit, as on a dev box whose `TMPDIR` is on a data disk.

- Put a test's socket under a short directory of its own: `os.MkdirTemp("", "<short prefix>")`, removed in `t.Cleanup`.
  A package that binds sockets in its tests keeps one helper for it, `shortTempDir` (`packages/daemon/internal/daemon/daemon_test.go` is one).
  Use the package's helper, or add one where the package has none.

- Before you call a test that binds a socket done, run it with a temporary directory of about 64 bytes.
  A pass under `/tmp` does not show that the test works under a longer one.
  Set `GOTMPDIR` as well, since `t.TempDir()` reads it first:

  ```sh
  L=/tmp/a-deliberately-long-tmpdir-for-unix-socket-path-tests-0001 && mkdir -p "$L"
  TMPDIR="$L" GOTMPDIR="$L" go test ./<package>/... -count=1
  ```

## Go's package deadline

`go test` panics a package that runs longer than ten minutes (`panic: test timed out after 10m0s`), which reads as a failure of the change under test.
`packages/envoy/internal/dispatch/api` alone takes close to that, about 600 s on a workstation, so every command that runs that package, on its own or in a pattern that includes it (`./...`, `./internal/dispatch/...`), passes `-timeout 20m`:

```sh
cd packages/envoy
go test -timeout 20m ./internal/dispatch/api
```

A Legion issue pod does not run that whole package: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

CI's `envoy-go` job runs it that way, alone and before the module's other packages (`.github/workflows/ci.yaml`), and `envoy-go-race-api`, the race job that runs that package by itself, gives it 30 minutes;
`docs` runs under `-race` in `envoy-go-race-docs`, which gives it 60.
A command that runs one of its tests by name (`-run`) needs no deadline of its own.
Run it alone rather than beside the module's other Postgres-backed packages:
together they share one database service and the machine's CPU, and on a small machine `api` and `docs` then each overran the default (`docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md`).

`packages/daemon/internal/daemon` is the daemon's slow, Postgres-backed package.
The test job now runs it only under the race detector, in its race step, once per job;
the plain step ("Test the Go daemon") leaves it out, and `go vet -tags e2e ./...` in the `typecheck` job still compiles it without the detector.
On CI's runners, while the plain step still ran it, it took at most 625 s there over the 49 green main `test` jobs of 2026-11-01 to 2026-11-03, and up to 733.581 s in the jobs whose race step timed out (job 125688799071).
Under `-race` it took up to 570 s over the 51 green `test` jobs of 2026-11-02 and 2026-11-03 that sjawhar/legion#2709 measured, on main and on pull requests, and ran go test's own ten minutes out on main at 2201840998; the race step gives it 30 minutes.
On a busy machine it takes longer still (908 s under `-race` on a shared devbox), so a command that runs the whole package, alone or in a pattern that includes it, passes `-timeout 30m`:

```sh
cd packages/daemon
go test -race -timeout 30m ./internal/daemon/
```

A Legion issue pod does not run that whole package either: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

## A database a Go test creates

A Go test that creates a Postgres database writes `create database … strategy file_copy`, as `packages/envoy/internal/dispatch/store/storetest` and both modules' `internal/testpg` do.
Without it, PostgreSQL 15 and later copy the source database through the write-ahead log, every page of it:
4.4 MB of log for an empty database made from `template1`, and 6.4 MB for a clone of Dispatch's migrated template, which made one run of the `api` and `docs` packages write 16 GiB of log.
A file copy writes a few hundred bytes.
`storetest`'s own test fails when a clone writes more than a hundredth of its template's size, and each module's `internal/testpg` test `TestEveryCreatedDatabaseIsAFileCopy` fails on any line of the module's Go files that opens a database's creation in a string without `strategy file_copy` on the same line.

A test changes no server setting (`fsync`, `full_page_writes`, `max_wal_size`):
the server `DISPATCH_TEST_DATABASE_URL` names can hold other databases, such as the local Dispatch's on `packages/envoy/scripts/dev-postgres.sh`'s container.

## A test that reads `GET /api/v1/references?since=` reads again

A since read answers only the edges at or below the event log's safe horizon (`getReferences` and `sinceHorizon` in `packages/envoy/internal/dispatch/api/references.go`), and waits at most `referencesSinceWait`, one second, for a horizon above the events committed before the read.
Past that wait it answers at the horizon it has and leaves the rest to a later read: the route defers, it never loses an edge.
The horizon is instance-wide, so any transaction holding a transaction id on the Postgres instance holds it back, and in `packages/envoy/internal/dispatch/api` the holder is the test process itself:
`storetest.Open`'s `create database … template … strategy file_copy` and `storetest`'s `drop database … with (force)` (`packages/envoy/internal/dispatch/store/storetest/storetest.go`).

So a read that answered within `referencesSinceWait` was not held back: what it listed is what it owed, and a wrong answer fails the test at once.
Only a read held past that wait is read again, bounded by `eventFeedWait`.
The helper is `awaitSinceRead` (`packages/envoy/internal/dispatch/api/references_test.go`), which takes the read as a function, so each call site keeps its own reader.

A test that takes one read as final fails under a transaction someone else holds:
`TestAnAnsweredAsksPartEdgesAreListedSince` failed about one run in six on main, 7 of 40 measured for dispatch://INFRA-592, and in this repository's own pod on 2026-11-02 it failed 15 of 40 runs with a transaction held 2 s of every 2.5 s;
under that same pulse, reading again, it passed 40 of
40.

Prove such a test under a loop that holds the state, as `docs/solutions/testing/code-that-waits-on-instance-wide-database-state-is-tested-under-a-loop-that-holds-that-state-pr2392.md` prescribes:

```sh
while true; do
  psql "$LEGION_TEST_PG_DSN" -Atc 'select pg_current_xact_id(), pg_sleep(2)'
  sleep 0.5
done
```

The holder must release each transaction:
one that never commits holds the horizon back for good, so no bounded re-read succeeds under it and the test fails after `eventFeedWait`, naming the hold.

## A writer a test holds

A test that needs a live socket's writer held, so that frames queue behind it while the test sends the socket more frames, holds the writer itself:
it builds its rig on a `heldWrites` listener (`packages/envoy/internal/dispatch/api/live_socket_test.go`), calls `hold`, and waits with `waitHeld` until a write waits at the gate.
Small TCP buffers and a page that reads nothing (`smallSendBuffers`, `smallReadDialer`) stall the writer only until the transport lets its write through, and the transport decides when that is.
Under gVisor, the runtime of Legion's worker pods, a write blocked for a third of a second completed the moment the page sent its next frame.
`TestARepeatedSubReplacesAnotherKind` then waited for a queued frame the writer had already written, and failed 8 of 300 runs.
Those two helpers are for tests of a slow page itself, such as one whose queue overflows.

## A test that starts jj or git names a working directory

jj searches upward from the directory it runs in for the closest `.jj` and opens that repository, and it has no ceiling option.
`-R` names the workspace a command acts on, and the two that create a repository, `jj git clone` and `jj git init --colocate`, take no `-R` at all.
`-R` does not keep jj off the repository its working directory sits in:
jj 0.45.1 loads that repository's configuration before it applies `-R`, and `jj -R <b> log` run inside a repository `a` that still held a legacy `.jj/repo/config.toml` migrated it, writing `a/.jj/repo/config-id`.
Every `-R` call site of `packages/daemon/internal/workspace` is safe because its `Dir` **is** its `-R` target, not because the flag fences jj in:
`runCheckedOnClone`'s comment states that invariant, where the directory a command runs in is chosen.
A child process started with no working directory of its own inherits the test process's, which in a Legion issue pod is inside `$LEGION_WORKSPACE`, a jj workspace of the pod's real shared clone under `/legion/repos`.
So a test whose jj child names no directory opens and writes that clone, and the same test is green on a GitHub runner, whose checkout is git-only with no `.jj` anywhere above it (dispatch://INFRA-623).

- Set `Dir` on every `exec.Command` a test starts that runs `jj`, and on every one that runs `git`, to a directory the test made.
  A `--git-dir` is not enough on its own: it tells git which repository to act on and leaves the child standing wherever the test process stands, which is what the next reader of the command has to work out again.

- The two probes that got this wrong were copies of one helper, now `testbin.RequireJJFilters` in `packages/daemon/internal/testbin`, each running `jj config list` with no `Dir`.
  A probe is as much a jj child as a fixture is.

- A test that drives `packages/daemon/internal/workspace` needs nothing of its own:
  `execRunner.Run` refuses a `Command` whose `Dir` is not absolute, and refuses a jj command that creates a repository when that directory, or any above it, holds a `.jj` directory (`refuseEnclosingWorkspace`).
  `TestProvisionLeavesTheEnclosingRepositoryUntouched` holds both on a runner, by provisioning from inside a decoy repository.

- A test that builds a repository of its own puts it under `t.TempDir()`, and `TMPDIR` must sit inside no checkout.
  Nothing refuses the fixtures' own `jj git init --colocate` (`runSetup`, `enclosingRepository`):
  it runs outside the runner, where `refuseEnclosingWorkspace` never sees it, so under a `TMPDIR` inside a checkout it would read that checkout's repository's configuration first, and migrate it when that repository still holds a legacy `.jj/repo/config.toml`.

## A test that starts jj or git inherits no pod's environment

A Legion issue pod hands every process its live workspace (`LEGION_WORKSPACE`), its git configuration (`GIT_CONFIG_*`), its GitHub credential and more.
A test that let git or jj inherit them read and pushed the pod's own work:
the shim's rescue test, run with no `LEGION_WORKSPACE` of its own, pushed the pod's workspace to the real origin as `legion/WIDGETS-42-rescue-…` branches.

- A `packages/daemon` package whose tests run git or jj, as children or in-process through the code under test, calls `fixtureenv.Guard` in its `TestMain`, after any branch where the test binary runs as a helper child, and holds `fixtureenv.RequireGuarded` in a test.
  Guard unsets every `LEGION_*`, `GIT_*`, `JJ_*`, `GH_*` and `GITHUB_*` variable, `SSH_AUTH_SOCK` and `SSH_ASKPASS`, keeping the tests' own inputs (`LEGION_TEST_*`, `LEGION_E2E*`, `GITHUB_ACTIONS`, and the names the package passes), and sets `GIT_ALLOW_PROTOCOL=file`, which a repository's own `protocol.<name>.allow` cannot override.
  Code under test that names its own transport, as provisioning's `GIT_ALLOW_PROTOCOL=https` does, still uses it.
  Guard does the same under the `e2e` build tag, whose untagged tests are the same fixtures.
  A live proof of that build, which reaches a real cluster and GitHub on purpose, calls `fixtureenv.Live(t)` first in its entry point:
  that gives the test the process's own values back of everything Guard took out or set, until the test ends.

- A fixture command runs under `fixtureenv.Isolated(home)`: the same variables dropped, a home of the test's, and an identity.
  A package whose code under test reads the process's home sets `fixtureenv.Home` on the process and gives its fixtures that same home (`packages/daemon/internal/workspace`'s `setupEnvironment`).

## Containers a test starts

Every container a test or a proof script starts carries a CPU limit and a memory limit sized to its work, and is removed when the test stops, except kind's node containers (`scripts/e2e/README.md` says why) and the Pulumi containers of `packages/envoy/infra/host/live_test.go`, whose released `Listener` component has no CPU or memory inputs.
Tests and the live proofs run on shared dev boxes as well as on CI runners, and an unlimited server can take every core and all the memory of a host that other work shares.

- In Go, set `Resources.NanoCPUs` and `Resources.Memory` in the container's host configuration:
  through `testcontainers.WithHostConfigModifier` for a `Run`, or as the request's `HostConfigModifier` for a `GenericContainer` (`packages/envoy/internal/testpg/testpg.go` is one).
  `WithHostConfigModifier` replaces any modifier an earlier option set, so a test that needs a host configuration of its own sets the limits in that same modifier.

- In a script, `docker run` takes `--cpus` and `--memory` before the image.
  A Postgres whose data directory is a `tmpfs` mount counts that data against its memory limit.
  `scripts/e2e/README.md` lists each proof's containers and their limits.

- Remove a container with its anonymous volumes: `docker rm -f -v`, since the Postgres images declare a `VOLUME` that a plain `docker rm -f` leaves on the host.
