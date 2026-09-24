---
title: "Bumping the OMP fork pin: one line in omp-pin.ts, proven by behavior rather than by a test of the string"
category: daemon
tags:
  - omp-pin
  - oh-my-pi
  - dependency-bump
  - boot-probes
  - probe-image
  - rpc
  - worker-image
  - verification
date: 2026-09-13
status: active
module: omp-pin
related_issues:
  - "LEGION-32"
  - "sjawhar/legion#983"
  - "LEGION-23"
---

# Bumping the OMP fork pin: one line in omp-pin.ts, proven by behavior rather than by a test of the string

LEGION-32 moved the daemon's default Oh My Pi build from `18.1.15-sami.20260908-220934` to
`18.1.18-sami.20260912-104423`, the first fork release whose history includes `0e57a6ca`
("fix(rpc): observe extension send rejections when no prompt scope is active"). The code change was
one line. The work was the proof.

## One constant, its consumers, and one derivation

`OMP_FORK_PIN` in `packages/daemon/src/daemon/omp-pin.ts` is the only literal version code reads
(since LEGION-23). Everything that runs Oh My Pi derives from it:

| consumer | how it reads the pin |
| --- | --- |
| `packages/daemon/src/daemon/config.ts` | imports `DEFAULT_OMP_INVOCATION` (`mise x ${OMP_FORK_PIN} -- omp`) |
| `packages/daemon/docker/worker.Dockerfile` | the `cli` stage runs the same `bun` command into `/out/omp-pin`; the `tools` stage `mise x "$pin" -- omp --version` / `mise where`; the runtime stage's probe step runs `legion probe-image` |
| `.github/workflows/worker-image.yaml` | `pull_request.paths` names `omp-pin.ts`, so a PR that touches it builds the image |
| `.github/workflows/envoy-and-contracts.yaml` | the `daemon-go` job installs the pin and exports `LEGION_TEST_OMP`, so the Go daemon's real-binary tests run on it |

One thing is derived *from the release* rather than read from the constant: the closed provider set
a Sandbox pod's Oh My Pi runs under (`packages/daemon-go/internal/modelroute/config.yml`'s
`disabledProviders`, every provider id the pinned catalog and auth rules know but anthropic) and its
test input `testdata/provider-keys.txt`. A release can add providers, so `config.yml` records the
release it was derived at (`# derived at:`), and `TestTheClosedProviderSetWasDerivedAtThePin` holds
that line to `omp-pin.ts`. That test is the one deliberate check of the version string: it exists
to make a bump re-derive the list (the commands are in both files' headers). Whether the list closes
the set is checked on the binary by `internal/modelroute/omp_test.go`.

So a bump is `grep -rn "<old version>" .` (excluding `.jj`, `.git`, `node_modules`) to confirm
nothing else names it, edit line 4, re-derive the closed set and the key list at the new tag, and
run the Go daemon's real-binary tests with `LEGION_TEST_OMP` at the new pin (the 18.1.21 → 18.2.9
bump, LEGION-208 4b.16, found a changed key-failure message and a changed catalog cost that way).
Do not add any other test that asserts the new string: it repeats the constant, fails on every
legitimate bump, and defends no behavior.

## The proof set

Each acceptance line got a command whose output was quoted verbatim in the PR body, and the tester
re-ran every one rather than trusting the quote. Run them from a shell with every
`LEGION_*`/`DISPATCH_*`/`ENVOY_*` variable removed (a worker pane carries the daemon's own) and
`OMP_PROFILE=legion` (the profile whose `plugins/` holds `@sjawhar/pi-legion-envoy`).

**1. The tag carries the fix.** On the knives-managed fork checkout (`/home/ubuntu/oh-my-pi`; read
`skill://fork-work` first — read-only queries only, never a fresh clone):

```
git rev-parse --verify -q v<tag>                       # the tag exists locally
git tag --points-at v<tag>                             # and nothing else shares its commit
git merge-base --is-ancestor <fix-sha> v<tag> && echo ancestor
```

The reviewer re-derived the same fact from GitHub (`compare/<fix>...<tag>` → `ahead`, `behind_by: 0`),
which also proves the *remote* release exists with its asset — a local tag can point somewhere the
published release does not.

**2. The constant prints and the release installs.**

```
bun packages/daemon/src/daemon/omp-pin.ts              # github:sjawhar/oh-my-pi@<tag>
mise x github:sjawhar/oh-my-pi@<tag> -- omp --version  # omp/18.1.18  (semver only; the -sami suffix is the tag)
mise where github:sjawhar/oh-my-pi@<tag>               # .../installs/github-sjawhar-oh-my-pi/<tag>
```

**3a. The daemon's own boot probes pass on the build.** `legion probe-image` is `cmdProbeImage`
(`packages/daemon/src/cli/index.ts`), which runs `verifyOmpAgentsCapability` and
`verifyLegionPluginLoaded` from `boot-probes.ts` — the same code `startDaemon` runs and the image
build's probe step runs:

```
bun install   # a fresh issue workspace has no node_modules: "Cannot find module '@legion/contracts'"
bun packages/daemon/src/cli/index.ts probe-image --omp "$(mise where github:sjawhar/oh-my-pi@<tag>)/bin/omp"
# probe-image: OK (/home/ubuntu/.mise/installs/github-sjawhar-oh-my-pi/<tag>/bin/omp)
```

**3b. The bug the bump exists for, reproduced against both builds.** No smoke rig was needed.
The fix commit's own message names the trigger: an extension `pi.sendMessage` with
`deliverAs: "aside"` or `triggerTurn: true` whose `sendCustomMessage` resolves `false` (a send that
provably starts no turn) rejects `invokingTask` with `send did not invoke the agent`; in `--mode rpc`
that rejection is only observed while an RPC `prompt` scope is active. The cheapest idle path that
returns `false` without a model call is a user interrupt: the RPC `abort` command runs
`session.abort({ reason: USER_INTERRUPT_LABEL })` even at idle, which sets
`#advisors.autoResumeSuppressed`, after which an idle aside is folded into context.

The rig is a 15-line extension plus a driver (kept at `/tmp/legion-32-repro/` while it existed):

```js
// repro-extension.mjs — what pi-legion-envoy's `deliver` does for an inbound Envoy envelope,
// fired from a timer so it lands while no RPC prompt scope is active.
export default function legion32Repro(pi) {
  pi.on("session_start", () => {
    setTimeout(() => {
      process.stderr.write("REPRO: sendMessage outside any prompt scope\n");
      pi.sendMessage(
        { customType: "legion-32-repro", content: "message that starts no turn", display: true },
        { deliverAs: "aside", triggerTurn: true }
      );
    }, 4000);
  });
}
```

Driver: `Bun.spawn([omp, "--mode", "rpc", "--no-session", "--no-extensions", "-e", extension])`,
wait for the `{"type":"ready"…}` frame, then write to stdin
`{"id":"n","type":"negotiate_protocol","protocolVersion":2}` and `{"id":"a","type":"abort"}`; one
second after the extension's send, write `{"id":"s","type":"get_state"}`; then kill. Verdict per build:

| build | stderr | `get_state` after the send | exit |
| --- | --- | --- | --- |
| old pin (`18.1.15-sami.20260908-220934`) | `[Unhandled Rejection] Error: send did not invoke the agent` | never answered | 1 |
| new pin (`18.1.18-sami.20260912-104423`) | no crash string | `"success":true` | alive until killed (143) |

Pair the negative signal (crash string absent) with a positive one (a request answered *after* the
risky send); "no crash in N seconds" alone conflates a slow crash with a fix. This is still a
black-box proof — the fix's code path is inferred from the commit, not traced — which is as strong
as it gets without instrumenting the release binary. Two things that cost time: `shutdown` is a frame
`legion worker-shim` consumes, not an OMP RPC command (OMP answers `Unknown command: shutdown`), and
the process stays alive until the driver kills it.

**3c. The worker image builds and probes the pin.** The `Worker Image` run on the PR is the check
that actually installs the release inside the image. Read its `Build and push` log for
`mise github:sjawhar/oh-my-pi@<tag> ✓ installed`, `omp/18.1.18`, and `probe-image: OK
(/opt/omp/bin/omp)`. A later handoff-only commit re-runs the workflow fully `CACHED` — the first run
is the evidence. Why the check attaches to every PR head is
`docs/solutions/github/pull-request-trigger-paths-follow-the-pr-head.md`; the image's probe ordering
is `docs/solutions/infra/omp-in-a-container-image.md`.

**4. Nothing else names the old version** — see the grep above; a hit in a file another open PR owns
is a scope decision, not a silent edit (`docs/solutions/legion/sibling-pr-claims-need-the-plus-lines.md`).

**5. The deployment's `legion.yaml`** — quote its `omp_invocation:` line and say whether the new
default equals it. In LEGION-32's case it did at the time, so the explicit setting became deletable;
it stopped being equal within the day when the operator moved the deployment for an unrelated
plugin contract (`docs/solutions/legion/grant-delivery-plugin-omp-contract.md`), so date the quote.

## One operator observation worth knowing

The daemon's private tmux server (`tmux -L legion-<project>`) keeps the global environment of the
daemon generation that forked it. On this box it still carried
`LEGION_OMP_PATH=.../omp-18.1.15-sami.9bff2014-rpcfix` from an earlier `run-daemon.sh`, and every
pane inherited it. Nothing the daemon spawns reads that variable (the daemon resolves the binary
itself and passes the path), and `readlink /proc/<omp pid>/exe` on every running `omp --mode rpc`
showed the mise install, not the hand-built binary. A worker does not kill a tmux server to clear
it; it goes away when the server is next recreated.
