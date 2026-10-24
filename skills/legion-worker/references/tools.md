# Getting the tool your work needs

Part of `skill://legion-worker`. Read it the moment your phase needs a tool the worker image may
not carry — a database client, a linter, a one-off CLI — before you install, download, or vendor
anything. Every path it cites is in sjawhar/legion.

## The five clients the image already carries

Check this list first: each of these is on your `PATH` now, and needs no runner.

- `psql` — the Dispatch browser suite (`bun run e2e`) resets its database for every spec through
  `packages/dispatch/e2e/psql.ts`, which `seed.ts` calls, and the local stack (`bun run
  dispatch:local`, `packages/dispatch/e2e/local-instance.ts`) refuses to start without it on
  `PATH` — so a role whose change owes that suite can run it.
- `redis-cli` — no repository of the project speaks Redis today; it is carried anyway, at about a
  megabyte, because a second image roll costs more than a megabyte.
- `sqlite3` — Oh My Pi's own session files are SQLite databases, and a role reading its own session
  or a repository's SQLite fixture has no other way to open one.
- `nc` — the reachability check you would otherwise write as an inline Python loop. The name comes
  from the alternatives system, so ask for `nc`, never `nc.openbsd`.
- `dig` — how a role whose connection to a service name fails tells "the name does not resolve"
  from "the packets are dropped".

## Why a sixth is an image change, not an install

Your container is an unprivileged user with no writable system prefix: `/usr/local/bin`, `/opt` and
`/home` are read-only, and `apt-get` cannot take dpkg's lock even to download.

```text
$ apt-get install -d -y postgresql-client
E: Could not open lock file /var/lib/dpkg/lock-frontend - open (13: Permission denied)
```

Unpacking files under `$HOME` or the issue's disk is a different question, and on the mechanics the
answer is not a flat no: `redis-cli`, `sqlite3` and `dig` did run from an extraction prefix with
`LD_LIBRARY_PATH` set, and `psql` did not — Debian's `/usr/bin/psql` is a Perl wrapper that loads
`PgCommon.pm` through Perl's own search path, which no extraction prefix is on. Legion rules the
attempt out whichever way it would have gone: an extraction prefix is the withheld `operator-setup`
capability (next section). So the answer for a role is never an extraction, however much room the
issue's disk has: for everything the five do not cover, the one documented step is a pinned run
through one of the three package runners the image already carries, and a tool no runner reaches is
an image change ("When no runner reaches it", below).

## What the withheld capability is, and what it is not

The withheld row reads `installs or configures its own dependencies in the pod: Legion owns its
dependencies` (`packages/daemon/internal/capabilities/capabilities.go`, ruling
dispatch://LEGION-200). Read as a sentence on its own, that wording would also forbid `uvx`,
`bunx --package` and `go run`, each of which fetches a package and writes a cache. It does not, and
three artefacts in this repository are what draw the line — not a claim about what the ruling meant:

- **What the image ships for a role to use.** `packages/daemon/docker/worker.Dockerfile` installs
  `uv` and `uvx`, `bun` with its `bunx` link, and the Go toolchain, and the `toolchain` capability
  row (`packages/daemon/internal/capabilities/capabilities.go`) grants them by name. A runner put
  in the image deliberately and granted by a capability row is not a route around a withheld one.
- **What the pod configures for those runners to write.** A role container's environment relocates
  `UV_CACHE_DIR` (`podkind.go`) and `BUN_INSTALL_CACHE_DIR`, `GOCACHE` and `GOMODCACHE`
  (`roleEnvironment`, `packages/daemon/internal/runtime/sandbox/manifest.go`) onto the issue's
  volume, shared by the issue's roles. Legion provisions the caches a pinned run fills; filling
  them is what they are for.
- **What Legion itself does at launch.** `installBlock`
  (`packages/daemon/internal/runtime/sandbox/plugins.go`) renders a `bun add` under
  `/usr/bin/flock` into that shared bun install cache on every installing launch. Fetching a pinned
  package at run time is something Legion does, not something it withholds.

So the withheld row is about a dependency **the pod carries**: something that outlives the command
that created it, and that another process or a later launch then resolves *differently* because it
is there. That last clause is the test to apply to a new case — after your command exits, does
anything else resolve differently?

- A pinned runner run: no. `uvx --from httpie@3.2.4 http --version` leaves a cache entry addressed
  by that coordinate and that version; the same line next time gets the same bytes whether the
  cache was warm or cold, and nothing else on the pod resolves anything differently for it. That is
  the sanctioned side, and it is the side the three forms below are on.
- An extraction prefix: yes. Unpacking Debian packages under `$HOME` and exporting
  `LD_LIBRARY_PATH` changes what every later command in that shell — and every subagent that
  inherits the environment — resolves `psql` or its libraries to, on a prefix nothing records,
  pins or rebuilds. That is a dependency the pod carries, configured by the role, which is why the
  extraction option is rejected (`docs/kubernetes.md`, the extraction row of the options table)
  even for the three clients that did run from it.

A repository's own dependency set is on the sanctioned side too, for the same reason the caches
are: `uv sync` or `bun install` in your workspace materialises what that project already declares,
and the runtime puts `UV_PYTHON_INSTALL_DIR` and the workspace itself on the issue's volume exactly
so a later pod reuses it.

What the artefacts do not settle: a pinned runner run whose *tool* then installs something of its
own outside the runner's cache — a browser download, a second toolchain under `$HOME`. The command
is the sanctioned form and what it leaves behind is not, and no capability row or runtime provision
covers it either way. Treat that as unsettled and ask the architect that owns your issue, rather
than reading either half of it as the rule.

## The three pinned runner forms

- `uvx --from <package>@<version> <command>`, for a Python tool: the distribution you pin and the
  command you run are named separately, because they are usually different names.
  `uvx --from httpie@3.2.4 http --version` prints `3.2.4`. Where the project's own name *is* its
  command's — checked once, never assumed — the short `uvx <name>@<version>` runs it:
  `uvx ruff@0.14.0 --version` prints `ruff 0.14.0`, in 1.7 s with the uv cache empty and 0.15 s
  warm. A version that does not exist fails closed, exit 1, naming it: `uvx ruff@99.99.99
  --version` prints `error: No solution found when resolving tool dependencies`, with `cause:
  Because there is no version of ruff==99.99.99 …`.
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

## Always write the exact coordinate and the exact version

A pin has two halves, and a runner resolves what you asked for only when both are right:

1. **The coordinate** — the registry name the project itself publishes, scope and all:
   `@biomejs/biome`, not `biome`. The command's name is no evidence of the package's. Take the
   coordinate from the project's own published installation instructions — its documentation site
   or its repository's README, reached independently of the registry rather than through the
   registry entry of a name you guessed. That is the step, and it is the only one that establishes
   identity. Do it once, before the tool's first use.
2. **The version** on that coordinate.

Everything a registry can tell you afterwards is corroboration, and it is worth knowing which kind
each piece is.

**Fields the publisher writes.** `npm view <name> version description repository.url homepage`
reads what the publisher put in its own `package.json`. npm verifies none of it: a squatter closes
this check by copying two strings, and a legitimate package that sets no `homepage` reads like a
squat. Evidence, never proof.

```text
$ npm view biome version description repository.url homepage
version = '0.3.3'
description = 'A simple way to manage environment variables on a per-project basis'
$ npm view @biomejs/biome version description repository.url homepage
version = '2.6.3'
description = 'Biome is a toolchain for the web: formatter, linter and more'
repository.url = 'git+https://github.com/biomejs/biome.git'
homepage = 'https://biomejs.dev'
```

Those two answers are *consistent with* `@biomejs/biome` being the project and `biome` being
someone else's package. What settles it is biomejs.dev telling you to install `@biomejs/biome` —
the site you reached without npm's help.

**What a registry can attest.** npm verifies a provenance attestation when it is published, so it
is the one part of an entry a publisher cannot forge by writing a string:

```text
$ npm view @biomejs/biome dist.attestations
{
  url: 'https://registry.npmjs.org/-/npm/v1/attestations/@biomejs%2fbiome@2.6.3',
  provenance: { predicateType: 'https://slsa.dev/provenance/v1' }
}
$ npm view biome dist.attestations
```

Present for the project, empty for the squatter. Its limit runs the other way: most packages
publish no provenance at all, so an empty answer is not evidence of a squat.

PyPI publishes attestations only for projects that release through a trusted publisher, and widely
used tools do without: for `ruff 0.14.0`, `https://pypi.org/pypi/ruff/0.14.0/json` carries no
`provenance` on any of its files and `https://pypi.org/integrity/ruff/0.14.0/<file>/provenance`
answers 404. For a Python tool, the project's own documentation is the whole of the check.

A Go module path is a domain-scoped URL, so the coordinate *is* the repository path, and you check
it by going to that path. `sum.golang.org` pins a version's bytes once they are published, which
stops the content changing under you later and says nothing about whether you picked the right
module path in the first place.

Pinning the version alone does not save you: it pins the squatter at a version, and obeys "write
the version" to the letter while doing it.

```text
$ bunx biome --version
0.3.3
$ bunx biome@0.3.3 --version
0.3.3
$ bunx --package @biomejs/biome@2.6.3 biome --version
Version: 2.6.3
```

Only the third runs the formatter, and what separates it from the second is the *name*, not the
version. No runner exempts you from the coordinate half; each one's syntax just makes a different
way of getting it wrong easy to type.

- `bunx <binary>@<version>` resolves the token as an npm package name, which is why
  `--package <name>@<version> <binary>` exists: a bare `bunx biome@0.3.3` is a coordinate nobody
  checked.
- `uvx <command>@<version>` does the same on PyPI, and carries it one step further: uv resolves
  the token as the **project**, installs that project, and only then looks for a command of that
  name.

  ```text
  $ uvx pyyaml@6.0.2 --version
  Installed 1 package in 23ms
  Package `pyyaml` does not provide any executables.
  ```

  Nothing there is about the version — `pyyaml` is a real project at a real version, and it is not
  what you meant by typing a command's name. `uvx --help` is where the distinction lives:
  `--from <FROM>  Use the given package to provide the command`. Hence
  `uvx --from shellcheck-py@0.10.0.1 shellcheck --version`, which prints `ShellCheck - shell
  script analysis tool` and `version: 0.10.0`: the distribution and the command share no name at
  all.
- `go run <module>@<version>` takes the coordinate as that domain-scoped URL, so a path you
  invented usually fails to resolve rather than running someone else's code — but a module path
  guessed from a binary's name is still a guess, and the checksum database will pin the wrong
  module's bytes as faithfully as the right one's.

Nothing refuses an unpinned command: no guard, no lint and no check stops you typing a bare `bunx
biome`. What you are given is a correct form and a tool that is already there, not a gate — so both
halves are yours to type.

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
