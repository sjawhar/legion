---
title: "A worker pod ships no go or tmux: provision them under $HOME on every relaunch, bind tmux's libraries with a wrapper, and record every figure in the handoff"
category: testing
tags:
  - worker-pod
  - daemon-go
  - tmux-runtime
  - go-toolchain
  - proof-environment
  - gvisor
  - load-testing
date: 2026-10-02
status: active
module: packages/daemon-go
applies_when:
  - A proof needs `go test` on `packages/daemon-go` from a Legion worker pod (Agent Sandbox runtime)
  - A real-tmux test prints `SKIP` in the pod because `requireTmux` finds no `tmux` on PATH
  - A phase worker is relaunched and the previous assignment's tools and output files are gone
  - A load recipe written for the devbox reports `load=0.00` in a pod
related_issues:
  - "LEGION-370"
  - "sjawhar/legion#1678"
---

# A Worker Pod Ships No go or tmux

- The worker image has no `go`, no `tmux`, no `curl`, no sudo. Provision both under `$HOME` in
  user space with the recipe below, pinned to the `go` line of `packages/daemon-go/go.mod` and
  the image's Debian release; a proof whose output says `SKIP` proved nothing.
- Bind tmux's shared libraries with a wrapper script on `PATH`, never `LD_LIBRARY_PATH` in the
  environment: the tmux runtime starts its server under `paneEnvAllowList`
  (`internal/runtime/tmux/environment.go:22-41`), which passes `PATH` and drops `LD_LIBRARY_PATH`.
- `$HOME` does not survive a relaunch. Each assignment of the same role can land in a fresh pod;
  re-run the recipe (about a minute) and never cite a file under `$HOME` as evidence. Every
  figure a later phase needs — the `RESULT` line, each failure's `Kind` and `Detail`, the exact
  commands, the head — goes into the handoff and the PR body the moment it is measured.
- Run the recipe from a plan task with exact URLs and versions, so the tester reproduces the
  environment rather than the implementer's memory of it.

## The recipe (worker image at Debian 13 trixie, `go 1.26.1`)

Download with `bun` (the image has it; it has no `curl`). Write the downloader with the `write`
tool if the pane refuses a heredoc.

```ts
// $HOME/tools/fetch.ts — argv: url path url path …
const a = process.argv.slice(2);
for (let i = 0; i + 1 < a.length; i += 2) {
  const r = await fetch(a[i], { signal: AbortSignal.timeout(300000) });
  if (!r.ok || !r.body) throw new Error(`${a[i]}: ${r.status}`);
  const w = Bun.file(a[i + 1]).writer();
  for await (const c of r.body) w.write(c);
  await w.end();
}
```

```bash
mkdir -p "$HOME/tools" "$HOME/proof" && cd "$HOME/tools"
P=https://deb.debian.org/debian/pool/main
bun fetch.ts https://go.dev/dl/go1.26.1.linux-amd64.tar.gz go.tgz \
  $P/t/tmux/tmux_3.5a-3_amd64.deb tmux.deb \
  "$P/libe/libevent/libevent-core-2.1-7t64_2.1.13-stable-1~deb13u1_amd64.deb" libevent.deb \
  $P/libu/libutempter/libutempter0_1.2.1-5_amd64.deb libutempter.deb \
  $P/j/jemalloc/libjemalloc2_5.3.1-2_amd64.deb libjemalloc.deb
tar -xzf go.tgz && mkdir -p root bin && for d in tmux libevent libutempter libjemalloc; do dpkg -x $d.deb root; done
printf '%s\n' '#!/bin/sh' \
  "LD_LIBRARY_PATH=$HOME/tools/root/usr/lib/x86_64-linux-gnu exec $HOME/tools/root/usr/bin/tmux \"\$@\"" > bin/tmux
chmod +x bin/tmux
printf '%s\n' 'export PATH="$HOME/tools/go/bin:$HOME/tools/bin:$PATH" GOTOOLCHAIN=local' \
  'unset LD_LIBRARY_PATH' 'export GOWORK=off' > "$HOME/proof/env.sh"
. "$HOME/proof/env.sh" && go version && tmux -V   # go1.26.1 linux/amd64, tmux 3.5a
```

Every later `go` or `tmux` command begins `. "$HOME/proof/env.sh" &&`; a bash call starts without
it. `GOTOOLCHAIN=local` holds only when the tarball matches `go.mod`'s `go` line exactly.
`GOWORK=off` lets a scratch copy of the package build outside the repository's `go.work`
(`packages/daemon-go` has no intra-repository module dependency) and is harmless for the
workspace. Never set `GOFLAGS=-mod=mod`. With `LD_LIBRARY_PATH` exported instead of the wrapper,
the runtime's own `tmux new-session` failed with `libevent_core-2.1.so.7: cannot open shared
object file`.

## What the pod costs

- First `go test` of the tmux package: 25-50 s downloading modules and building the `legion` and
  `omp` stand-ins; later runs ~1.3 s per `TestRealTmuxObserveReportsGoneOnce` iteration at idle.
- `-count=200` of a test with a mandatory 1 s silence wait is 256-281 s idle and 406-432 s
  loaded, so a repetition harness carries `-timeout 90m`; the test's own deadline is untouched.
  See `a-container-build-inside-go-test-spends-the-tests-deadline.md` for what a `go test`
  timeout bounds.
- `/proc/loadavg` reads `0.00 0.00 0.00` throughout under gVisor. Record load as `busy=<n>` plus
  wall seconds, never paste the zeros as a load figure; the devbox recipe's `load=` field does not
  transfer.

## Evidence (LEGION-370)

The planner wrote the recipe as plan task T2 after finding `command -v go tmux` empty in its pod,
`/opt/legion/go/bin` holding only `legion` and `agent-secrets`, and egress to `go.dev` and
`deb.debian.org` open. The implementer and tester each ran it in their own pods; both were
relaunched into fresh pods for review round 1 (`$HOME` empty, `env.sh` gone) and re-ran it in
under a minute. Each pod reproduced the full proof set independently (200 idle, 200 loaded,
negative controls) because every figure from the earlier round was already in
`.legion/implement.json`, `.legion/test.json`, and the PR body rather than in `$HOME/proof`.

## Related

- `docs/solutions/testing/a-stream-consumer-ends-on-the-producers-terminal-set-and-its-deadline-names-the-last-observation.md`
  — the change this environment proved.
- `docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md` —
  the load recipe, with its pod variant.
