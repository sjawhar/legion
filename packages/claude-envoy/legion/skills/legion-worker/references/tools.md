# Getting the tool your work needs

Part of `skill://legion-worker`. Read it the moment your phase needs a tool the worker image may
not carry — a database client, a linter, a one-off CLI — before you install, download, or vendor
anything. Every path it cites is in sjawhar/legion.

## The five clients the image already carries

Check this list first: the worker image carries these five from the change that added them onward,
and they are on a role's `PATH` once its deployment pins a digest at or after that change — a role
still on an older digest finds none of them. Run `command -v psql redis-cli sqlite3 nc dig` to see
which you have; those need no runner.

- `psql` — the Dispatch browser suite (`bun run e2e`) resets its database for every spec through
  `packages/dispatch/e2e/psql.ts`, which `seed.ts` calls, and the local stack (`bun run
  dispatch:local`, `packages/dispatch/e2e/local-instance.ts`) refuses to start without it on
  `PATH` — so a role whose change owes that suite can run it.
- `redis-cli` — no repository of the project speaks Redis today; it is carried anyway, at about a
  megabyte, because a second image roll costs more than a megabyte.
- `sqlite3` — a repository's own SQLite fixture or database has no other way to open from a shell,
  and Oh My Pi's own profile databases (`~/.omp/profiles/legion/agent/agent.db`, `models.db`,
  `history.db`) are SQLite too. Your session is not one of them: it is `results.jsonl`,
  `state.json` and `title` under `XDG_STATE_HOME`.
- `nc` — the reachability check you would otherwise write as an inline Python loop. The name comes
  from the alternatives system, so ask for `nc`, never `nc.openbsd`.
- `dig` — how a role whose connection to a service name fails tells "the name does not resolve"
  from "the packets are dropped".

## Why a sixth is an image change, not an install

Your container is an unprivileged user with no writable system prefix: `/usr/local/bin` and `/opt`
are read-only, `/home` is read-only too, and `apt-get` cannot take dpkg's lock even to download.
`$HOME` — `/home/legion`, the one writable directory under that read-only `/home` — is yours to
write.

```text
$ apt-get install -d -y postgresql-client
E: Could not open lock file /var/lib/dpkg/lock-frontend - open (13: Permission denied)
```

So that writable `$HOME`, and the issue's disk, are where an install for yourself would go, and on
the mechanics the answer is not a flat no: `redis-cli`, `sqlite3` and `dig` did run from an
extraction prefix under `$HOME` with `LD_LIBRARY_PATH` set, and `psql` did not — Debian's
`/usr/bin/psql` is a Perl wrapper that loads `PgCommon.pm` through Perl's own search path, which no
extraction prefix is on. What rules the attempt out is not the filesystem but the next section's
ruling: an extraction prefix is the withheld `operator-setup` capability, for what it leaves every
later process resolving. That verdict is about resolution, so it reads the same in a container that
does hand you a writable `/usr/local/bin`, and the room on the issue's disk does not touch it
either. For everything the five do not cover, the one documented step is a pinned run through one
of the three package runners the image already carries — plus `uv run --with` when what you need is
a library rather than a command — and a tool no runner reaches is an image change ("When no runner
reaches it", below).

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
that created it, and that a later launch or another role's process then resolves *without asking
for it*. That last clause is the test to apply to a new case — after your command exits, what does
something else now resolve that it never named?

- A pinned runner run: nothing. `uvx --from httpie@3.2.4 http --version` leaves a cache entry
  addressed by that coordinate and that version, which the next command reaches only by writing the
  same pin; no `PATH`, no interpreter and no library search of any other process changes. That is
  the sanctioned side, and it is the side the forms below are on.
- A repository's own dependency set: nothing either, which is why the caches are provisioned at
  all. `uv sync` or `bun install` in your workspace materialises what that project already
  declares, into that project's own directory, which a process reaches by asking for that project
  and no other way — and the runtime puts `UV_PYTHON_INSTALL_DIR` and the workspace itself on the
  issue's volume exactly so a later pod reuses it.
- An extraction prefix: everything in reach of it. Unpacking Debian packages under `$HOME` and
  exporting `LD_LIBRARY_PATH` changes what every later command in that shell — and every subagent
  that inherits the environment — resolves `psql` or its libraries to, asked for or not, on a
  prefix nothing records, pins or rebuilds. That is a dependency the pod carries, configured by the
  role, which is why the extraction option is rejected (`docs/kubernetes.md`, the extraction row of
  the options table) even for the three clients that did run from it.

The test is about what a command leaves for other processes, never about getting the same bytes
twice — because a pinned line does not promise the same bytes. What a pin binds is the named
distribution at that version; its dependency closure is resolved when the command runs. With
`UV_CACHE_DIR` emptied, `uvx -v --from httpie@3.2.4 http --version` logs `Selecting:
requests==2.34.2 [compatible]` and resolves `urllib3` and `certifi` from ranges, where the warm run
prints `3.2.4` at once off the cache that first run filled. When your work needs the closure pinned
too — a comparison that has to hold across days or across pods — that is a project's own lockfile
with `uv run` or `bun install` in a workspace, not a one-off runner line.

A pinned run whose tool then installs something of its own — an interpreter, a browser, a second
toolchain — is on the sanctioned side when both of these hold: what it writes lands under the
issue's own volume, through one of the variables the runtime actually sets for you —
`XDG_CACHE_HOME` (`/legion/scratch/cache`), `XDG_DATA_HOME`, `XDG_STATE_HOME`, `TMPDIR`, `GOCACHE`,
`GOMODCACHE`, `BUN_INSTALL_CACHE_DIR`, `UV_CACHE_DIR` (`/legion/uv/cache`) and
`UV_PYTHON_INSTALL_DIR` (`/legion/uv/python/<issue>`) — and after the command exits nothing else
resolves differently. (`XDG_CONFIG_HOME` is set too and is not one of them: it is the pod's shared
in-memory volume.) `uv run --with pyyaml==6.0.2 --no-project` passes both, and so does the
interpreter `uv run` downloads for a project whose `.python-version` names one: the wheel lands in
`UV_CACHE_DIR`, the interpreter under `UV_PYTHON_INSTALL_DIR`, and the issue's next pod finds both
there. A browser download passes only if you point that tool's own cache variable at a directory
under one of those paths yourself — the runtime sets no browser path at all, so an unset one leaves
the download wherever the tool defaults, which is nowhere this runtime chose.

Where it lands is what you pay. Under one of those variables it is on the issue's volume: it
outlives the pod, every role of the issue shares it, and a later pod of the issue reuses it. Under
`$HOME` — `/home/legion`, which no volume is mounted on — it is the container's own filesystem: one
copy per role, per pod, gone when the role relaunches, and charged to the gigabyte of ephemeral
storage the container is admitted with. Moving exactly these writes off that filesystem is what
`roleEnvironment` (`packages/daemon/internal/runtime/sandbox/manifest.go`) exists for, after GKE
Autopilot evicted issue pods that charged their caches to it — so an install under `$HOME` fails
the first condition and is outside the sanctioned form, however little else it changes. It is the
withheld capability when either condition fails — anything that writes into a system prefix, and
anything that leaves a toolchain, shim or binary which a later launch or another role's process
will resolve without asking for it. Between two installs that both land in the right place, the
test is resolution, not disk: a second toolchain that only an explicit absolute path reaches is
sanctioned; the same toolchain put where `PATH` finds it is operator setup, whoever typed the
command. The three artefacts above are the whole of that line's ground. Disk is not: the issue's
volume is 49 GiB with 5.2 GiB used and every cache already relocated onto it, and a sanctioned
install is paid once per issue, on the issue's own disk, by the issue's roles together.

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

## When you need a library, not a command

The three forms above run somebody else's command. When what you need is a package available to
code of your own — PyYAML to read a file, not a CLI — the form is `uv run --with
<package>==<version> --no-project` ahead of the program you are running:

```text
$ uv run --with pyyaml==6.0.2 --no-project python -c "import yaml; print(yaml.__version__)"
Installed 1 package in 12ms
6.0.2
```

That whole run is 0.597 s wall, the install included. `--no-project` keeps it off whatever
`pyproject.toml` happens to sit above your working directory, so it behaves the same wherever you
type it, and `--with` takes a requirement specifier (`==<version>`), not a runner coordinate
(`@<version>`). It is on the sanctioned side for the same reason a `uvx` run is: it fills
`UV_CACHE_DIR` and leaves nothing another process resolves. Reaching for a command runner instead
is the mistake the next section's `uvx pyyaml@6.0.2 --version` shows — uv installs the project and
then finds no executable, because a library has none to find.

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

**A check whose evidence the subject controls is not a check.** Carry that past npm: every line
below the next prompt was written by the party whose identity is in question.

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
is the one part of an entry a publisher cannot forge by writing a string. What it proves is *build
origin*: the attestation binds the artifact to the repository and workflow that built it. Presence
is not identity — a squat published from its own public Actions workflow carries a genuine
attestation of its own repository — so the check is to read whose build it names, and compare that
with the repository the project's own documentation gave you:

```text
$ npm view @biomejs/biome dist.attestations.url
https://registry.npmjs.org/-/npm/v1/attestations/@biomejs%2fbiome@2.6.3
$ curl -s "$(npm view @biomejs/biome dist.attestations.url)" \
    | jq -r '.attestations[] | select(.predicateType=="https://slsa.dev/provenance/v1") | .bundle.dsseEnvelope.payload' \
    | base64 -d | jq -r '.predicate.buildDefinition.externalParameters.workflow.repository'
https://github.com/biomejs/biome
```

That field is the SLSA v1 predicate's `buildDefinition.externalParameters.workflow`, which here
reads `{"ref": "refs/heads/main", "repository": "https://github.com/biomejs/biome", "path":
".github/workflows/release.yml"}` — the repository biomejs.dev sent you to, building on its own
main branch. The comparison is what makes the attestation worth anything: on its own it says only
that *some* repository built these bytes. (`jq` in your bash tool is the harness's builtin, and a
script gets `/usr/bin/jq`; both print this.)

Its limit runs the other way: most packages publish no provenance at all — `npm view biome
dist.attestations` prints nothing — so an absent attestation is no evidence of a squat.

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
change: a pull request against sjawhar/legion that carries the same name through five files,
because its own tests hold them to each other. The apt line in
`packages/daemon/docker/worker.Dockerfile`, with the comment saying why that package is carried,
installs it; the `toolchain` name list in `packages/daemon/internal/capabilities/image.go` makes
the probe require it; that row's summary in `packages/daemon/internal/capabilities/capabilities.go`
has to name it, a test holding the summary to the list; the stub binaries at
`packages/daemon/internal/capabilities/capabilitiestest/capabilitiestest.go:14` have to include it,
or the capability and `probe-image` tests fail on an image that does not carry it; and
`packages/daemon/internal/capabilities/image_test.go` spells the expected detail string out again.
A pull request that stops at the first two opens red. It reaches a role only once a deployment pins
the new image digest, as every Legion change does — never within your own phase.

So when your work needs such a tool, say so to the architect that owns your issue (`envoy_publish`
to its role topic), with the tool, what your phase needs it for, and the runner forms you already
ruled out. Improvising — an extraction prefix, a vendored binary, a hand-rolled substitute — is not
the step: the architect decides whether the image change is work to schedule.

The full decision, with its measurement table and every number behind the forms above, is in
`docs/kubernetes.md`, the subsection "The tools a role's own work needs".
