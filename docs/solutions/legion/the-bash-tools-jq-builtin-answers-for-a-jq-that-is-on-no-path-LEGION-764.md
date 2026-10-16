---
title: "The bash tool's jq builtin answers `command -v jq` for a jq that is on no PATH"
category: legion
tags:
  - bash-tool
  - jq
  - builtin
  - worker-image
  - path
  - proof
date: 2026-10-15
status: active
module: packages/daemon/docker
related_issues:
  - "LEGION-764"
  - "sjawhar/legion#1967"
---

# The bash tool's jq builtin answers `command -v jq` for a jq that is on no PATH

Extends docs/solutions/legion/worker-pane-shell-gotchas.md (§14, the builtin is jaq and a script runs the
PATH's jq). §14 assumes a `jq` exists on PATH behind the builtin; in a worker pod none may. The builtin then
answers for existence, not only for engine: in the bash tool, `type jq` → `jq is a shell builtin`,
`command -v jq` → `jq`, exit 0, and `jq` runs — on an image with no `jq` binary anywhere. Everything a
worker starts as a process (`bun test`, a Go test that shells out, a script, a subagent's shell) gets
`jq: command not found` or Bun's `Executable not found in $PATH: "jq"`.

- Never decide "`<name>` is on PATH" from `command -v`, `type` or `which` typed into the bash tool. Decide
  it from a child process: `bash -c 'type -P jq ssh-keygen'` (one path per name, exit 0),
  `env -i PATH=/usr/bin:/bin sh -c 'command -v jq'`, `/usr/bin/env jq --version`, or `test -x /usr/bin/jq`.
  The same holds for a spec or plan that writes `command -v jq` as its check: run the subprocess form and
  record the departure.
- A `jq` a worker fetched into `/tmp` and put first on PATH satisfies a script's `jq` but not a harness that
  pins `PATH` to `/usr/bin:/bin` (`scripts/e2e/lib/ensure-project.test.ts`, `PATH: ${dir}:/usr/bin:/bin`);
  only an installed `/usr/bin/jq` does. Debian trixie's `jq` 1.7.1-6 prints `jq-1.7` for `--version`, not
  `jq-1.7.1`: match the prefix.

## Evidence

Measured in the LEGION-764 planner's and implementer's pods on release legion-v13.2.0 (worker image
`sha256:c69e8c5d…`, before the change): `command -v jq ssh-keygen` in the bash tool printed `jq` and exited 0;
`bash -c 'type -P jq ssh-keygen'` exit 1, `env -i PATH=/usr/bin:/bin sh -c 'command -v jq ssh-keygen'` exit 127,
`/usr/bin/env jq --version` → `env: 'jq': No such file or directory`. `bun test scripts/e2e/lib/` there: 105 pass,
212 fail of 317, every failure naming `jq`; with a `/tmp` jq first on PATH 307 pass, the remaining 8 non-docker
failures all in `ensure-project.test.ts`. After sjawhar/legion#1967 installed `jq` and `openssh-client` in the
runtime stage, the same checks inside the published image's rootfs answered `/usr/bin/jq`, `/usr/bin/ssh-keygen`
and `jq-1.7`, and the suite ran 314 pass, 3 fail (two `docker` spawns, one chroot uid-0 artefact).
