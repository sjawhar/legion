---
title: "Every worker.Dockerfile RUN must survive shlex: CI lexes it for context sources, and quotes nested in a quoted substitution break it"
category: daemon
tags:
  - worker-image
  - dockerfile
  - check-image-trigger-paths
  - shlex
  - ci-lint
date: 2026-10-16
status: active
module: packages/daemon/docker/worker.Dockerfile, .github/scripts/check-image-trigger-paths.sh
related_issues:
  - "LEGION-636"
  - "sjawhar/legion#1984"
---

# Every worker.Dockerfile RUN must survive shlex: CI lexes it for context sources, and quotes nested in a quoted substitution break it

- The required `lint` job's "Check image triggers cover every image input" step
  (`.github/scripts/check-image-trigger-paths.sh`) joins each Dockerfile instruction's
  continuation lines and runs Python's `shlex.split` over every `RUN`, `COPY` and `ADD` to find
  the build-context paths it reads. A `RUN` whose text shlex cannot split fails the job with
  `unparseable: No closing quotation; balance its quotes so a shell could read it`, whatever the
  shell would have made of it.
- shlex does not know `$( … )`. Inside a double-quoted command substitution, the first `"` of a
  nested string closes the outer quote for the lexer:
  `v="$(sed -n 's/^ *"typescript": "\([^"]*\)".*/\1/p' "$f")"` is valid sh and unlexable. Write
  such a line so each quoted region is flat: assign the substitution unquoted
  (`v=$(sed -n '…' "$f")` — an assignment does no word splitting), or hoist the quoted command
  into a function and call it with plain words
  (`pin() { sed -n "s/^ *\"$1\": \"\([^\"]*\)\".*/\1/p" "$f"; }; v=$(pin typescript)`).
- Run the check in the pod before pushing a Dockerfile change; it needs no Docker, only PyYAML,
  which the pod's `python3` lacks:
  `uv run --with pyyaml --no-project -- bash .github/scripts/check-image-trigger-paths.sh`
  must end `every image trigger covers every input its build reads`. A one-line pre-check while
  drafting: `python3 -c 'import shlex,sys; shlex.split(sys.stdin.read())' < run-body.txt`.
- The same script also reads every `COPY`'s sources against the image workflow's `paths:`
  filter, so a new build input (a lockfile directory) is covered only when a trigger path matches
  it; `packages/daemon/**` covers anything under the daemon.

## Evidence (LEGION-636, sjawhar/legion#1984)

The notices stage read the three language-server pins from the copied `package.json` with three
`version="$(sed -n '…"typescript": "\([^"]*\)"…' "$ls_pkg")"` assignments. The build would have
run them fine; the lint job failed twice (heads 4035e7c0 and 53cb8bdf) with `ValueError: No
closing quotation` from `shlex.split`, because the `"` inside the single-quoted sed program
closed the outer `"$(` for the lexer. The fix was one `pin()` function with a double-quoted sed
and three unquoted assignments; the script then passed in the pod under `uv run --with pyyaml`
and in CI.
