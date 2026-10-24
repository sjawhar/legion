# Getting the tool your work needs

Part of `skill://legion-worker`. Read it the moment your phase needs a tool the worker image may
not carry — a database client, a linter, a one-off CLI — before you install, download, or vendor
anything. Every path it cites is in sjawhar/legion.

## The five clients the image already carries

Check this list first: each of these is on your `PATH` now, and needs no runner.

- `psql` — the Dispatch browser suite (`bun run e2e`) refuses to start without it, so a role whose
  change owes that suite can run it.
- `redis-cli` — no repository of the project speaks Redis today; it is carried anyway, at about a
  megabyte, because a second image roll costs more than a megabyte.
- `sqlite3` — Oh My Pi's own session files are SQLite databases, and a role reading its own session
  or a repository's SQLite fixture has no other way to open one.
- `nc` — the reachability check you would otherwise write as an inline Python loop. The name comes
  from the alternatives system, so ask for `nc`, never `nc.openbsd`.
- `dig` — how a role whose connection to a service name fails tells "the name does not resolve"
  from "the packets are dropped".

## Why you cannot install a sixth

Your container is an unprivileged user with no writable system prefix: `/usr/local/bin`, `/opt` and
`/home` are read-only, and `apt-get` cannot take dpkg's lock even to download.

```text
$ apt-get install -d -y postgresql-client
E: Could not open lock file /var/lib/dpkg/lock-frontend - open (13: Permission denied)
```

So a compiled client is not something you can fetch for yourself, however much room the issue's
disk has. For everything the five do not cover, the one documented step is a pinned run through one
of the three package runners the image already carries.

## The three pinned runner forms

- `uvx <name>@<version>`, for a Python tool. `uvx ruff@0.14.0 --version` prints `ruff 0.14.0`, in
  1.7 s with the uv cache empty and 0.15 s warm. A version that does not exist fails closed, exit
  1, naming it: `uvx ruff@99.99.99 --version` prints `error: No solution found when resolving tool
  dependencies`, with `cause: Because there is no version of ruff==99.99.99 …`.
- `bunx --package <name>@<version> <binary>`, for an npm tool, naming the package and the binary
  separately because they differ. `bunx --package @biomejs/biome@2.6.3 biome --version` prints
  `Version: 2.6.3`, in 0.3 s with bun's cache warm; `bunx --package @biomejs/biome@99.99.99 biome
  --version` fails closed, exit 1, with `error: No version matching "99.99.99" found for specifier
  "@biomejs/biome" (but package exists)`.
- `go run <module>@<version>`, for a Go tool. `go run github.com/nats-io/natscli/nats@v0.1.5
  --version` prints `v0.1.5`, in 7.2 s with the module cache warm and 103 s on a cold module cache
  — budget for the cold one the first time an issue runs a Go tool. `go run
  github.com/nats-io/natscli/nats@v99.99.99 --version` fails closed, exit 1, with
  `go: github.com/nats-io/natscli/nats@v99.99.99: invalid version: unknown revision nats/v99.99.99`.

## Always write the exact version

The name is written with its version every time, never bare: unpinned, the command can run a
different package altogether.

```text
$ bunx biome --version
0.3.3
$ bunx --package @biomejs/biome@2.6.3 biome --version
Version: 2.6.3
```

The npm package `biome` is "A simple way to manage environment variables on a per-project basis",
with one maintainer; the formatter is `@biomejs/biome`. Nothing refuses the unpinned command: no
guard, no lint and no check stops you typing a bare `bunx biome`. What you are given is a correct
form and a tool that is already there, not a gate — so the pin is yours to type.

## When no runner reaches it

A tool that is neither in the image nor reachable through one of the three runners is an image
change: a pull request against sjawhar/legion that adds it to
`packages/daemon/docker/worker.Dockerfile` and to the `toolchain` row in
`packages/daemon/internal/capabilities/image.go`. It reaches a role only once a deployment pins the
new image digest, as every Legion change does — never within your own phase.

So when your work needs such a tool, say so to the architect that owns your issue (`envoy_publish`
to its role topic), with the tool, what your phase needs it for, and the runner forms you already
ruled out. Improvising — an extraction prefix, a vendored binary, a hand-rolled substitute — is not
the step: the architect decides whether the image change is work to schedule.

The full decision, with its measurement table and every number behind the forms above, is in
`docs/kubernetes.md`, the subsection "The tools a role's own work needs".
