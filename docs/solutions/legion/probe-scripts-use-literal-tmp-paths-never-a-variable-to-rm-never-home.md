---
title: "Probe scripts use literal /tmp paths, never pass a variable to rm, and never reference $HOME: the pane guard's word list pushes probes into files, and a file is where an edit can leave a cleanup line behind"
category: legion
tags:
  - pane-guard
  - probe
  - bash
  - rm
  - HOME
  - incident
  - jj
  - worker-safety
date: 2026-09-14
status: active
module: legion
related_issues:
  - "LEGION-84"
  - "sjawhar/legion#1080"
  - "LEGION-121"
  - "LEGION-122"
---

# Probe scripts use literal /tmp paths, never pass a variable to rm, and never reference $HOME

On 2026-09-13 at 17:52Z a LEGION-84 probe script ran `rm -rf "$work" "$HOME"` against the
operator's real home directory for 120 seconds (LEGION-121, LEGION-122). Nothing about shell
expansion went wrong: `set -u` was on and both variables were set exactly as written. The line was
*correct* in the script's first version, whose line 7 exported a scratch `HOME`; an edit removed
that override to fix an unrelated signing failure, and a one-line miscount in the same edit
appended the intended replacement (`rm -rf "$work"`) after the old cleanup line instead of over
it. The script ran without the edited tail being re-read.

## Why the probe was a file at all

The pi-envoy pane guard refuses any `bash`/`eval` command whose text mentions `jj` together with
`abandon`, `undo`, `restore`, or `revert` — including inside a quoted string, a heredoc, a commit
message, or a `--data` argument. The LEGION-84 setting is named `git.abandon-unreachable-commits`,
so every probe of it, every handoff JSON naming it, and every commit subject about it had to go
through a file written with the `write` tool. That is the guard working as designed: it protects
the shared jj operation log
(`docs/solutions/legion/shell-command-gates-derive-from-bash-word-splitting-not-example-forms.md`).
It also means a probe's destructive lines live in a file the guard never sees, edited by line
number, run by name. A file is exactly where a stale line survives an edit.

## The rules (the architect's standing orders since the incident)

1. **No command in bash, eval, or a script file may delete, move, truncate, or recursively change
   anything outside `$LEGION_WORKSPACE` and a `/tmp` directory the same script created by literal
   name.** Deleting `.legion/` inside the workspace at the reviewer's direction is the one
   intended removal, and it is done by path inside the workspace.
2. **Never pass a variable to `rm`.** `rm -rf "$anything"` is one edit away from `rm -rf
   /home/<user>`. If a script must clean up, name the literal path it created, or leave the
   scratch in place and say where it is (the operator sweeps `/tmp`).
3. **Never reference `$HOME` in a probe.** A scope control that needs a different home
   *assigns* one: `env HOME=/tmp/<literal>/foreign-home XDG_CONFIG_HOME=/tmp/<literal>/foreign-home/.config jj …`,
   and that directory is created by the script and left behind.
4. **Refuse to reuse a directory.** `if [ -e "$work" ]; then exit 3; fi` (bash) or
   `if (existsSync(root)) process.exit(3)` (bun) before `mkdir -p`: a probe that finds its literal
   path already present stops instead of deleting.
5. **Re-read the whole file after every edit and before every run.** Read it with the `read` tool
   at `:raw`, and scan it — `grep` for `\brm\b|rmSync|unlink|rename|mkdtemp|\$HOME` — before the
   run. The edit tool echoes the lines it touched; the line it *appended next to* instead of
   replacing is the one you did not look at.
6. **Give a scratch home its own variable when you must override `HOME` in a script's
   environment.** `probe_home=/tmp/<literal>/home; export HOME="$probe_home"` keeps the cleanup
   (`… "$probe_home"`) from ever naming the real home even if the override line is later removed —
   though under rule 2 there is no cleanup line at all.

## What "literal" buys

With every path spelled out (`/tmp/legion-84-probe-get-045`, `/tmp/legion-84-e2e-044-control`),
a reader — the reviewer, the operator, you after an edit — can see what a script can touch
without executing it in their head. The LEGION-84 driver and probes after the incident follow
this shape, leave their scratch in place, and name it in the PR body; the reviewer re-observed
the scratch read-only as part of its verification.

## If it happens anyway

Stop everything. Assess with read-only tools (a directory listing, `/proc/*/exe` and `/proc/*/fd`
for `(deleted)` handles, readdir order to bound what preceded the point of the kill). Report to
the architect over Envoy and to the humans on the Dispatch issue in plain words — what is gone,
who is locked out, what is broken box-wide (a deleted signing key breaks every jj snapshot on the
box) — and paste the exact script text as it ran. Then touch nothing until the operator confirms
repair; the session and its transcript are the incident record.
