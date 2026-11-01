# Testing

Rules for this repository's tests that the code under test does not show.

## Unix sockets in Go tests

A unix socket path must be 107 bytes or shorter. Linux's `sun_path` holds 108 bytes with the
terminating zero, and Go refuses a longer path with `invalid argument` at `bind`, `listen` or
`dial`. A tmux server socket has the same limit (`Filename too long`). `t.TempDir()` puts the
test's name, cut to 64 bytes, in its path, under `GOTMPDIR` or else `TMPDIR`. With a long test name
and a socket under it, or under a directory made from it (an `XDG_RUNTIME_DIR`, `HOME` or state
directory a test sets to `t.TempDir()`), a temporary directory of about 20 bytes is enough to pass
the limit, as on a dev box whose `TMPDIR` is on a data disk.

- Put a test's socket under a short directory of its own: `os.MkdirTemp("", "<short prefix>")`,
  removed in `t.Cleanup`. A package that binds sockets in its tests keeps one helper for it,
  `shortTempDir` (`packages/daemon/internal/daemon/daemon_test.go` is one). Use the package's
  helper, or add one where the package has none.
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

CI's `envoy-go` job runs it that way, alone and before the module's other packages
(`.github/workflows/ci.yaml`), and `envoy-go-race-api`, the race job that runs that package by
itself, gives it 30 minutes; `docs` runs under `-race` in `envoy-go-race-docs`, which gives it 60.
A command that runs one of its tests by name (`-run`) needs no deadline of its own. Run it alone
rather than beside the module's other Postgres-backed packages: together they share one database
service and the machine's CPU, and on a small machine `api` and `docs` then each overran the default
(`docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md`).

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

- The two probes that got this wrong were copies of one helper, now `testbin.RequireJJFilters` in `packages/daemon/internal/testbin`, each running `jj config list` with no `Dir`.
  A probe is as much a jj child as a fixture is.

- A test that drives `packages/daemon/internal/workspace` needs nothing of its own: `execRunner.Run` refuses a `Command` whose `Dir` is not absolute, and refuses a jj command that creates a repository when that directory, or any above it, holds a `.jj` directory (`refuseEnclosingWorkspace`).
  `TestProvisionLeavesTheEnclosingRepositoryUntouched` holds both on a runner, by provisioning from inside a decoy repository.

- A test that builds a repository of its own puts it under `t.TempDir()`, and `TMPDIR` must sit inside no checkout.
  Nothing refuses the fixtures' own `jj git init --colocate` (`runSetup`, `enclosingRepository`): it runs outside the runner, where `refuseEnclosingWorkspace` never sees it, so under a `TMPDIR` inside a checkout it would read that checkout's repository's configuration first, and migrate it when that repository still holds a legacy `.jj/repo/config.toml`.

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
