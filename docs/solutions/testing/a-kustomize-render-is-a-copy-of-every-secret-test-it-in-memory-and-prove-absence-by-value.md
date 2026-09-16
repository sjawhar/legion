---
title: "A kustomize render is a base64 copy of every Secret: test it in memory (never through `| grep -q` under pipefail), never write it, and prove absence by value in every encoding"
category: testing
tags:
  - secrets
  - kustomize
  - pipefail
  - sigpipe
  - bash
  - smoke-rig
  - kind-smoke
date: 2026-09-15
status: active
module: scripts/kind-smoke (up.sh write_overlay, up.test.sh, down.test.sh)
applies_when:
  - A script renders a kustomize overlay or Helm chart that carries `secretGenerator`s / `kind: Secret`
  - A `set -o pipefail` script tests large command output with `producer | grep -q`
  - A harness has to prove that a secret is nowhere on disk or argv after a run
related_issues:
  - "LEGION-26"
  - "sjawhar/legion#1114"
  - "LEGION-184"
---

# A kustomize render is a base64 copy of every Secret

`kubectl kustomize <overlay>` on `deploy/kubernetes/daemon/overlays/kind` emits the providers
Secret (every provider key, the Dispatch and Envoy tokens) and the GitHub App PEMs, base64-encoded,
in one document. The first heads of the kind smoke wrote that render to `<state>/rendered.yaml` to
check the image digest before applying it. The reviewer found five such files under
`~/.local/state/legion-smoke/*/` — one per instance ever started, each holding every secret of its
run, outside the `secrets/` directory that `down.sh` shreds. They were found **by value**
(`grep -rlF` on the PEM body and the token strings), not by name.

## Render in memory, apply from the overlay

```bash
render="$(kubectl kustomize "$o")" || fail "kubectl kustomize $o failed"
case "$render" in *"$zero"*) fail "the rendered overlay still carries the placeholder digest" ;; esac
kubectl apply -k "$o"     # renders again itself; nothing is written
```

One substitution, one `case` glob. The rendered text exists only in the shell's memory.

## Why not `kubectl kustomize "$o" | grep -Fq -- "$zero"`

That was the round-1 fix, and the reviewer showed it is not a gate under `set -o pipefail`:
`grep -q` exits on its first match; anything the producer writes after that gets EPIPE, the
producer dies of SIGPIPE, the pipeline's status is 141, and the `||`/`if` around it reads "no
match". So a **surviving placeholder passes** whenever the producer still has output to write
after the matching line — a real render puts the Secret documents after the Deployment, and
`kubectl kustomize` writes in pieces, so that is the normal case, not a corner. The harness case
that pins it: the fake `kustomize` honours `FAKE_KUSTOMIZE_PLACEHOLDER=1` and emits the zero
digest after four hundred image lines, with the Secret documents after it; the `case` form fails
with the error and no `apply -k` in the call log, the pipe form lets it through
(`FAIL up.test.sh:337`).

Rule: under `pipefail`, never use `cmd | grep -q` as a pass/fail test of large output. Capture once
and test with `case … in *"$needle"*)` or `[[ $x == *"$needle"* ]]`; if it must be a pipe, drop
`-q` so grep drains its input.

## How secrets travel instead

- **Environment on a function call**: `ENVOY_API_TOKEN="$token" start_process listener …` — a
  prefix assignment on a bash *function* call is exported to the children the function spawns,
  so the value lands in the process environment and never on an argv. The harness proves it with
  the fake binary's environment dump (`FAKE_ENV/<name>`, names only) and a by-value refute on the
  argv log.
- **`curl -H @file`** for bearer headers (`auth_header_file`), `docker run --env-file` for
  Postgres, 0600 files under a 0700 directory for everything else, and a `<NAME>_FILE` pointer
  where the consumer supports one.
- **`env -u NAME…` as a real argv** in front of `start_process` (`scrub_argv`), so the launcher's
  own `GH_*_B64` App keys and provider keys are unset for the listener, Dispatch, the bridge, and
  the loops. A shell *function* wrapper does not work here: the recorded pid is then a bash
  subshell whose `/proc/<pid>/environ` is the launcher's, and `down.sh` kills the wrapper and
  leaves the real binaries bound to their ports (seen on the `legion26d` run).

## Prove absence by value, in every encoding, and cross-check up-then-down

`up.test.sh` builds `secret_values`: each secret raw **and** base64 (what a `kind: Secret` carries),
each PEM's base64 prefix **and** a body line. It refutes every value in the fake argv log, the
script's output, and every file under the state directory outside `secrets/`; then runs `down.sh`
and refutes every value anywhere under the state directory. The live runs repeat the last step
against the real state dir (`grep -rlF -f <values> <state>` → 0 files after `down.sh`; before it,
exactly the files under `secrets/` and `overlay/secrets/`). A name-based check (`find -name
'rendered*'`) would not have found a copy under another name; the by-value scan did.

## Related

- `docs/solutions/daemon/spawned-process-environment-is-an-allow-list-and-the-tmux-tables-are-part-of-it.md`:
  the daemon's own rule for what a pane may inherit.
- `docs/solutions/testing/set-e-exempts-a-negated-command-and-non-final-list-operands-a-refute-accepts-exactly-exit-1.md`:
  the `refute` these scans rely on, and why it accepts exactly exit 1.
