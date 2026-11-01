# Getting the tool your work needs

Part of `skill://legion-worker`. Read it the moment your phase needs a tool the worker image may
not carry — a database client, a linter, a one-off CLI — before you install, download, or vendor
anything; before you measure Oh My Pi's binary; and when a test fails in your pod. Every path it
cites is in sjawhar/legion.

## The database your tests use

- Never give a test, a migration or anything that creates or drops a database the URL in the
  file `OMP_SESSION_SQL_DSN_FILE`: that is the live sessions database every pod shares, and its
  login can create and drop databases there.
- Give every Postgres variable a test reads `$LEGION_TEST_PG_DSN`, your pod's own server:
  `LEGION_TEST_PG_DSN`, `DISPATCH_TEST_DATABASE_URL` and `BROKER_TEST_DATABASE_URL`. Each makes
  and drops a database of its own per test, so your pod's roles can share the server.
- Anything that migrates into the database its DSN names, rather than making one per test, needs
  a database of your own, because a sibling role writing the same one collides with you: the
  Dispatch browser suite's `DATABASE_URL`, which resets what it is given, and the stage scripts'
  `LEGION_E2E_PG_DSN`, which a daemon's `postgres_dsn` becomes and migrates Legion's schema into.
  Make one and point the variable at it:
  `createdb -h 127.0.0.1 -p 5433 -U legion_test "e2e_$(date +%s)"`.
- The server is empty in every new pod and dies with it. Every database made from `template1`
  has pgvector.
- `LEGION_TEST_PG_DSN` set and the connection refused: that is not yours to repair. Starting the
  server is the roles container's launcher's job — it runs `legion-testpg ensure` before any role
  starts and again every 15 seconds — so wait about that interval and try again. Never start a
  server of your own beside it, and never reach for another DSN. Still refused after that wait:
  report it, with the error you got and the port you used, as a fault of the pod rather than of
  your change.
- `LEGION_TEST_PG_DSN` unset: your deployment has not turned the pod's server on, so no launcher
  is starting one and nothing will appear while you wait. With `legion-testpg` on `PATH`, start
  one yourself on the issue's volume: the directory it defaults to is a 1 MiB filesystem your
  role's own mounts cover, so `export LEGION_TESTPG_DIR=$XDG_DATA_HOME/testpg` first, then
  `export LEGION_TEST_PG_DSN=postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable`
  and `legion-testpg ensure`, which makes the cluster, the role and `vector` in `template1`. That
  server lives in your generation: it dies when your generation ends, which takes a sibling
  role's connections and databases with it mid-test, and the next `ensure` of either role finds
  the same cluster on disk and serves it again. With no
  `legion-testpg` either, stand up one with the harness below (**The worked case**) and set the DSN.

## The five clients the image already carries

Check this list first: the worker image carries these five from the change that added them onward,
and they are on a role's `PATH` once its deployment pins a digest at or after that change — a role
still on an older digest finds none of them. Run `command -v psql redis-cli sqlite3 nc dig` to see
which you have; those need no runner. `command -v legion-testpg` says whether your digest also
carries the pod's own PostgreSQL server (the section above).

- `psql` — the Dispatch browser suite (`bun run e2e`) resets its database for every spec through
  `packages/dispatch/e2e/psql.ts`, which `seed.ts` calls, and the local stack (`bun run
  dispatch:local`, `packages/dispatch/e2e/local-instance.ts`) refuses to start without `go`,
  `psql`, `flock`, `openssl` and `python3` on `PATH`, and without `docker` too unless a
  `DATABASE_URL` is given. The image carries `go`, `flock`, `openssl` and `python3`, a
  `DATABASE_URL` stands in for `docker`,
  and `psql` was the one a role had no way to supply — so a role whose change owes that suite can
  run it.
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

Your role runs as an unprivileged user with no writable system prefix: `/usr/local/bin` and `/opt`
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
extraction prefix is on. What rules such an attempt out is not the unpacking: it is the next
section's test ("The test: where it lands, and what it leaves resolving"), and an extraction under
`$HOME` fails that test's first condition whatever you scope it to, because `/home/legion` is the
pod's disk rather than the issue's volume — the placement `roleEnvironment`
(`packages/daemon/internal/runtime/sandbox/manifest.go`) exists to move writes off. The sanctioned
extraction is the harness's: onto the issue's volume, and exported around its own commands and no
further (the Dispatch e2e stack, below, is the worked case). For everything the five do not cover,
the one documented step is a pinned run through one of the three package runners the image already
carries — plus `uv run --with` when what you need is a library rather than a command — and a tool
no runner reaches is an image change ("When no runner reaches it", below).

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
- **What the pod configures for those runners to write.** A role's environment relocates
  `UV_CACHE_DIR` and `UV_PYTHON_INSTALL_DIR` (`podkind.go`) and `BUN_INSTALL_CACHE_DIR`, `GOCACHE`
  and `GOMODCACHE` (`roleEnvironment`, `packages/daemon/internal/runtime/sandbox/manifest.go`)
  onto the issue's volume, shared by the issue's roles. Legion provisions the caches a pinned run
  fills; filling them is what they are for.
- **What Legion itself does at launch.** `installBlock`
  (`packages/daemon/internal/runtime/sandbox/plugins.go`) renders a `bun add` under
  `/usr/bin/flock` into that shared bun install cache on every installing launch. Fetching a pinned
  package at run time is something Legion does, not something it withholds.

So the withheld row is about a dependency **the pod carries**: something that outlives the command
that created it, and that a later launch or another role's process then resolves *without asking
for it*.

### The test: where it lands, and what it leaves resolving

An install of your own — an interpreter, a browser, a second toolchain, an extraction prefix — is
on the sanctioned side when both of these hold: what it writes lands on the issue's own volume,
under `/legion`, which outlives a role's relaunch in its pod and is shared by the issue's roles —
whether through a variable the runtime sets for you there or under a path on that volume your
harness names itself — but outside the issue's workspace (`$LEGION_WORKSPACE`, under
`/legion/workspaces`) and the runtime's own directories (`/legion/repos`, `/legion/feed`); and
after the command exits nothing else resolves differently. The workspace is excluded because its jj
working copy tracks every new file under jj's `snapshot.max-new-file-size` (1 MiB for a role), so
a root written there is added to the change every role shares, and a larger new
file is left untracked, every jj command in the workspace then warning `Refused to snapshot some
files`. The one write into it that is sanctioned is a repository's own dependency install into
its ignored directories, below. The variables the runtime sets on the volume, where a tool's own
cache already goes, are
`XDG_CACHE_HOME` (`/legion/scratch/cache`), `XDG_DATA_HOME`, `XDG_STATE_HOME`, `TMPDIR`, `GOCACHE`,
`GOMODCACHE`, `BUN_INSTALL_CACHE_DIR`, `UV_CACHE_DIR` (`/legion/uv/cache`) and
`UV_PYTHON_INSTALL_DIR` (`/legion/uv/python/<issue>`). (`XDG_CONFIG_HOME` is set too and is not
one of them: it is your role's own config home, a copy of the pod's made for your role on the
pod's disk, which no other role reads.) `$HOME` and `/tmp` are not on the volume:
both are your role's own, on the pod's disk.

It is the withheld capability when either condition fails — anything that writes into a
system prefix, and anything that leaves a toolchain, shim or binary which a later launch or
another role's process will resolve without asking for it. Between two installs that both land in
the right place, the test is resolution, not disk, and it reads as scope: a second toolchain
exported for the commands of your own run — by absolute path, or on a `PATH` your harness's own
script sets around its own invocations — is sanctioned; the same toolchain left where a later
launch, another role or a subagent picks it up without naming it is operator setup, whoever typed
the command.

What each shape leaves resolving:

- A pinned runner run: nothing. `uvx --from httpie@3.2.4 http --version` leaves a cache entry
  addressed by that coordinate and that version, which the next command reaches only by writing the
  same pin; no `PATH`, no interpreter and no library search of any other process changes. That is
  the sanctioned side, and it is the side the forms below are on.
- A repository's own dependency set: nothing either, which is why the caches are provisioned at
  all. `uv sync` or `bun install` in your workspace materialises what that project already
  declares, into that project's own directory, which a process reaches by asking for that project
  and no other way — and the runtime puts `UV_PYTHON_INSTALL_DIR` and the workspace itself on the
  issue's volume exactly so every role of the issue's pod reuses it.
- An extraction prefix: whatever you leave resolving through it, which is yours to decide. Unpack
  Debian packages under `$HOME`, export `LD_LIBRARY_PATH` in the shell you go on working in, and
  you change what every later command in that shell — and every subagent that inherits the
  environment — resolves `psql` or its libraries to, asked for or not, on a prefix nothing records,
  pins or rebuilds: that fails both conditions, and it is the withheld capability. The harness
  below is the shape that passes. The opposite shape is in the tree too, and superseded: telling
  every subagent to prepend a directory to `PATH` because a subagent's shell starts without it
  hands over an environment to processes that never named it. That is the difference between
  standing a harness up and getting yourself a CLI.

The test is about what a command leaves for other processes, never about getting the same bytes
twice — because a pinned line does not promise the same bytes. What a pin binds is the named
distribution at that version; its dependency closure is resolved when the command runs. With
`UV_CACHE_DIR` emptied, `uvx -v --from httpie@3.2.4 http --version` logs `Selecting:
requests==2.34.2 [compatible]` and resolves `urllib3` and `certifi` from ranges, where the warm run
prints `3.2.4` at once off the cache that first run filled. When your work needs the closure pinned
too — a comparison that has to hold across days or across pods — that is a project's own lockfile
with `uv run` or `bun install` in a workspace, not a one-off runner line.

A pinned run whose tool then installs something of its own — an interpreter, a browser, a second
toolchain — is where the first condition does the work. `uv run --with pyyaml==6.0.2 --no-project`
passes both: the wheel lands in `UV_CACHE_DIR`, under `wheels-v6/pypi/pyyaml/6.0.2-…`, and
`UV_PYTHON_INSTALL_DIR` is never created, because the image's own `python3` served the run. The
interpreter is the other run: a project whose `.python-version` names an interpreter uv must fetch
puts about 70 MB under `UV_PYTHON_INSTALL_DIR`, where every role of the issue's pod finds it, while
the cache holds no wheel. A browser download passes too, and for Playwright — the browser driver this
repository's own Dispatch suite installs (`playwright install`, `packages/dispatch`'s
`e2e:install`) — it passes as it stands, with nothing pointed anywhere: playwright-core resolves
its browser registry under `XDG_CACHE_HOME`, which the runtime has already put on the issue's
volume, so the issue's roles share one registry under the exclusive install lock that
`docs/kubernetes.md` writes down in "Anatomy of a Sandbox pod", the paragraph on sharing the cache
home — worth reading before an install, for the two things that follow from sharing. Pointing
`PLAYWRIGHT_BROWSERS_PATH` elsewhere is what costs you: that shared registry, and the lock that
makes a second role installing at the same time wait rather than race. The condition is for a tool
whose cache the runtime does not already place — Puppeteer is one, resolving its browser cache to
`join(homedir(), '.cache', 'puppeteer')` and so to your role's own home on the pod's disk whatever
`XDG_CACHE_HOME` says. For that one, point the tool's own cache variable (`PUPPETEER_CACHE_DIR`
here) at a directory under one of the paths above yourself, since an unset one leaves the download
wherever the tool defaults, which is nowhere this runtime chose.

Where it lands is what you pay. Under one of those variables it is on the issue's volume: it
outlives a role's relaunch in its pod, and every role of the issue is told the same path — under
every one of those variables but
`XDG_STATE_HOME`, which is `/legion/scratch/state/<role>`, a directory named for your role
(`roleEnvironment`) because Oh My Pi names its browser broker's lock from the state root and an
abstract socket name is one name across every role of the pod, which share its network. It is not
private: every role of the issue can write it, as it can every path above — though it does not
outlive the pod: the issue's disk is the pod's own and is deleted with it, so a later pod of the
issue starts without it. Under
`$HOME` — `/home/legion`, which no volume is mounted on — it is your role's own home, an overlay
over the image's whose writes land on the pod's disk (an `emptyDir` every role's home and `/tmp`
share): one copy per role, per pod, kept across the role's relaunches in that pod and gone when
the pod is replaced, and charged to the pod's ephemeral-storage limit, which every role of the pod
shares and which is smaller than the volume. Moving exactly these writes off that
filesystem is what `roleEnvironment` (`packages/daemon/internal/runtime/sandbox/manifest.go`)
exists for, after GKE Autopilot evicted issue pods that charged their caches to it — so an install
under `$HOME` fails the first condition and is outside the sanctioned form, however little else it
changes. The three artefacts above are the whole of that line's ground. Disk is not: every cache is
already relocated onto the issue's volume, which has the room for them (`docs/kubernetes.md`, "The
tools a role's own work needs", carries the figure), and a sanctioned install is paid once per
issue pod, on the issue's own disk, by the issue's roles together.

**The worked case, and the one a role actually meets: the Dispatch e2e stack.** A change to
`packages/dispatch` is proved by driving its browser suite, which needs a Postgres with pgvector.
On a digest carrying `legion-testpg`, use the pod's own server (the first section above) and
unpack nothing. On a digest without it, the way
that stack is stood up here is Debian packages unpacked with `dpkg-deb -x` into one root on the
issue's volume — the five packages and the start sequence are in
`docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md`,
and
`docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md`
puts it on the volume, moves the caches with it, and writes it as one idempotent script the next
role re-runs. That passes both conditions, and the second is the one to get right: export the
root's `PATH` and `LD_LIBRARY_PATH` around the harness's own commands — `initdb`, `pg_ctl`, the
server, the Playwright run — rather than into the shell you go on working in. Handing the harness
to the next role is the same test passed again, and
`docs/solutions/legion/a-port-held-in-an-issue-pod-is-a-sibling-roles-harness-move-your-run-with-the-five-port-variables-and-your-own-database-and-never-stop-the-holder-LEGION-695.md`
is the illustration: it leaves the server running and records the `LD_LIBRARY_PATH`, `PATH` and
`DATABASE_URL` in its handoff's `trickyParts`, so what crosses to the next role is a note that
role sets deliberately rather than an environment it inherits. The suite shells out to `psql`,
and `psql` being in the image is what makes that harness work now without your extracting a
client for yourself at all.

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
change: a pull request against sjawhar/legion that carries the same name through every site that
spells it out, and those sites are not all held by a test.

Do not work from a list of those sites. This page has carried one twice and it was wrong both
times, so take them from the repository instead: grep for a client the image already carries,
under both of its names — the command and the Debian package that provides it are spelled
differently, and different sites spell each — and carry the new name through everything the two
find.

```text
$ grep -rn redis-cli
$ grep -rn redis-tools
```

One kind of hit wants more than a substitution: the toolchain smoke step in
`packages/daemon/docker/worker.Dockerfile` runs each tool once, so a new one needs its own
invocation there (`nc` and `dig` take no `--version`). The licence file the image carries at
`/usr/share/doc/legion/THIRD_PARTY_NOTICES` needs no edit: its list of Debian packages is written at
build time from the image's own dpkg database.

Four of the files the grep finds are in Go, and those four are the gated ones: the `toolchain`
name list in `packages/daemon/internal/capabilities/image.go` makes the probe require it; that
row's summary in `packages/daemon/internal/capabilities/capabilities.go` has to name it, a test
holding the summary to the list; the stub binaries in
`packages/daemon/internal/capabilities/capabilitiestest/capabilitiestest.go` have to include it, or
the capability and `probe-image` tests fail on an image that does not carry it; and
`packages/daemon/internal/capabilities/image_test.go` spells the expected detail string out again.

Miss one of those four and the suite is red, which is what makes them easy to find. Nothing holds
a Dockerfile comment or the smoke step to anything, so a change that stops at the gated sites opens
green with comments that no longer describe the image and a smoke step that never runs the new
tool.

So when your work needs such a tool, say so to the architect that owns your issue (`envoy_publish`
to its role topic), with the tool, what your phase needs it for, and the runner forms you already
ruled out. What is not the step is improvising a CLI for your own convenience — a vendored binary,
a hand-rolled substitute, or an extraction prefix dropped on your own `PATH`: the architect decides
whether the image change is work to schedule. An extraction a harness builds for its own runs is
not improvising and is not what this rules out (the Dispatch e2e stack, above): the two cases part
on "The test: where it lands, and what it leaves resolving", which the convenience case fails.

The operator's side of this — why the image carries those five clients rather than anything else,
the five priced options with their measurements, and what the clients cost the image — is in
`docs/kubernetes.md`, the subsection "The tools a role's own work needs".

## Before you measure Oh My Pi's binary

Read Oh My Pi's source first, from the fork (`sjawhar/oh-my-pi`) at the tag `.omp-pin` names, not
from upstream's version tag: the fork's release carries code upstream's does not. `.omp-pin` reads
`github:sjawhar/oh-my-pi@<version>`, and the git tag adds a `v` in front of that text, so find it
rather than type it:

```text
$ git ls-remote --tags https://github.com/sjawhar/oh-my-pi.git 'v<version>'
```

Search that source by the name of a construct or a setting, never by its value: one value is
spelled in many places, and the code that sets it may spell it in none. If the module the binary
names is absent at that tag, the binary is the ground truth: measure it, and say the source did not
carry it. In a pod the binary (`$LEGION_OMP_PATH`) is the build the worker image was made from,
and your workspace's `.omp-pin` can name another, so the tag you read may not be the build you
measure: compare `"$LEGION_OMP_PATH" --version` with the pin first, and say which build each
figure came from
(`docs/solutions/testing/a-worker-pods-oh-my-pi-is-the-fleets-pin-not-the-branchs-fetch-the-branchs-build-and-say-which-pin-each-figure-came-from-LEGION-247.md`).

## The tests your pod cannot run

Some tests fail in a Kubernetes issue pod whatever your change, because of the pod, not the branch.
A tmux pane runs on its host, and none of this holds there. Their proof is CI's run at the head:
cite that run, and name in your handoff each package the pod could not run. The cases below are
the known ones, not every one: a failure they do not name is yours to explain. Several of them
reach any work a pod runs, a test or a tool of your own, not a test suite alone.

- **Check your own output, never the job's phase.** A Kubernetes Job that reports `Succeeded`
  says its container exited 0, and nothing more: read the artefact your work was to produce, or
  the log line that proves it ran, before you believe any result. A lane measured this on
  2026-11-01: two pods running a native toolchain died with `Error: Stack overflow.` after 23
  seconds, and their Job still reported `Succeeded`, because the process they ran (`rocq repl`)
  exits 0 when a command inside it fails. The toolchain is incidental; any interpreter, driver or
  wrapper that swallows an inner failure gives the same clean phase over a silent no-result, and
  a lane that trusts the phase records it as a run that worked.

  Two habits make that phase true. Detect the failure in the text your tool prints and exit
  non-zero yourself, rather than passing the wrapper's exit code on. And write a verdict that
  matters to durable storage as it is produced, not at the end: a node can vanish within a minute
  of a pod ending, so an unpersisted log is gone even when nothing killed anything, and a
  terminating pod's grace period can be as short as 25 seconds, which is no time to flush.
- **gVisor.** Every pod runs under gVisor, which does not give a test every interface the Linux
  kernel does. In `packages/envoy` the tests of `cmd/agent-secrets`, `cmd/agent-secrets-helper`
  and `internal/broker/helper` fail: the helper identifies its peer with `SO_PEERPIDFD`, which
  gVisor answers `protocol not available` (`PEER_UNIDENTIFIED` in the test output). The
  terminal-prompt tests of `cmd/agent-secrets` also need terminal job-control ioctls
  (`inappropriate ioctl for device`), and the `SigCgt` line of `/proc/<pid>/status`, which the
  pods' gVisor lacked on 2026-10-28; upstream gVisor added that line on 2026-09-24, so a node on a
  later release may have it. A timing test can fail too: `internal/dispatch/pmdoc`'s linearity
  tests time a call by the process's CPU time, which gVisor counts in 10 ms ticks, so a call under
  10 ms can measure 0 s and the test reports `+Inf times`.
- **`O_NOFOLLOW` guards a leaf and not a component under gVisor.** A lane measured this on
  2026-11-01, in a pod under gVisor (`uname` 4.19.0-gvisor) against the same probe on an ordinary
  kernel, on two filesystems. Almost everything matches: `O_RDONLY|O_NOFOLLOW` on a symlink to a
  file refuses `ELOOP`, `O_CREAT|O_EXCL|O_NOFOLLOW` on one refuses `EEXIST`, `O_PATH|O_NOFOLLOW`
  opens the link itself, and the controls (a regular file, a real directory, a free name) succeed
  on both. One call differs: `openat(dirfd, <a link to a directory>,
  O_DIRECTORY|O_NOFOLLOW|O_RDONLY|O_CLOEXEC)`, which the ordinary kernel refuses `ENOTDIR` and
  gVisor follows with no error. So code that walks a path one component at a time and opens each
  with `O_NOFOLLOW` is defeated there while every leaf check still passes, and its tests stay
  green on a developer's kernel. A whole block of such tests failing in a pod is that one
  property of the environment, not one defect per test. The fix shape holds on both kernels: the
  walk cannot trust the flag, so `lstat` the component, `fstat` the descriptor it opened, compare
  `st_dev` and `st_ino`, and refuse when they differ or when the `lstat` says `S_IFLNK`. The
  probe used plain `openat` with `O_NOFOLLOW`; `openat2` with `RESOLVE_NO_SYMLINKS` was out of
  scope and is untested.
- **Set `ulimit -s` to a finite 131072 or more under gVisor, never `unlimited`.** gVisor maps the
  whole stack at exec and caps it at a constant, `maxStackSize = 128 << 20` in `MapStack`
  (`pkg/sentry/mm/syscalls.go`), so 128 MiB is the main thread's ceiling and no setting raises
  it; `RLIM_INFINITY` falls back to the 8 MiB default. A lane measured both on 2026-11-01 in
  pods under gVisor (4.19.0-gvisor), with a C probe and a second probe in another language: under
  `unlimited`, which `/proc/self/limits` reads back as `unlimited`, the fault came at 7 MiB and
  524,150 frames; under two very different finite settings at or above 131072 (KiB), both probes
  measured the same ceiling, a fault at 127 MiB and 8,388,470 frames. The same probes on an
  ordinary kernel track the setting as expected, so this is the sandbox and not the probes. One
  trap follows from it: a helper that raises the stack by setting the soft limit to the hard
  limit gets 8 MiB in a pod, since the pod's hard limit is `unlimited`, and setting a finite
  `ulimit -s` first makes that helper's own check pass.
- **Compare durations within one pod, never across pods.** A gVisor sandbox runs syscall-heavy
  work 2.2 to 3.8 times slower than the same work outside it, and a pod lands on whatever node
  the autoscaler provisions, so two pods are two machines, two sets of neighbours and two
  moments. Three parts, all measured: within one pod, comparable — run both arms there, as two
  containers on one node started together, and a difference between them is a difference between
  the arms; across pods, not comparable; across platforms, not until something measures the gap.
  Label every figure taken there a gVisor figure. That factor is the cost of syscalls and no
  budget for a whole job: a lane measured two real builds of the same work faster in a pod than
  on a virtual machine (1,014 s against 1,546 s, and 620 s against 1,117 s), with system time
  roughly doubled and user time lower, so compute-bound work can run as fast or faster there.
- **A `subPath` mount of the pod's own volume can come up empty.** A lane measured this on
  2026-11-01 under gVisor: the mount raised no error and refused nothing, and the directory was
  simply empty, so a job that reads it, finds nothing and carries on produces a clean-looking
  wrong answer and a green exit. Mount the volume root instead and reach the subdirectory
  through a symlink. This reaches a role that starts a workload of its own in the cluster
  (`docs/kubernetes.md`, "Running a workload in the cluster from a role").
- **No Docker.** The image carries no container runtime, so a test that starts one through
  testcontainers panics `rootless Docker not found`: in `packages/envoy`, the tests that use
  `internal/tests3` (`internal/dispatch/files`, which `DISPATCH_TEST_NO_CONTAINERS=1` skips) and
  `internal/testpg` (unless `DISPATCH_TEST_DATABASE_URL` names a Postgres).
- **The rest, by package.** For the Dispatch server's suites (a Postgres whose only collation is
  byte order, which `internal/dispatch/api` refuses, and `@legion/proof-editor` missing until
  `bun install` has run):
  `docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md`.
  For the daemon's:
  `docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md`.
