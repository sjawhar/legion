# What the withheld capability is, and what it is not

Part of `skill://legion-worker`, read from `skill://legion-worker/references/tools.md` before you
unpack, extract or install anything the worker image does not carry. Every path it cites is in
cybermonklabs/legion.

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
  the sanctioned side, and it is the side the runner forms of
  `skill://legion-worker/references/tools.md` are on.
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
`packages/dispatch` is proved in the pod by driving the specs it touches, one spec a command
(`bun run e2e -- e2e/<one spec file>`); the whole browser suite runs in CI's `dispatch` job on the
pull request. One spec needs the same stack as all of them: a Postgres with pgvector. On a digest
carrying `legion-testpg`, use the pod's own server
(`skill://legion-worker/references/tools.md#the-database-your-tests-use`) and
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
