---
title: "A jj-only driver with main's file as the negative control is the pre-merge proof shape for provisioning changes now that the smoke rig is withdrawn"
category: testing
tags:
  - e2e
  - negative-control
  - jj
  - jj-0.44
  - jj-0.45
  - provisioning
  - real-binary
  - driver-script
  - smoke-rig
date: 2026-09-14
status: active
module: packages/workspace
related_issues:
  - "LEGION-84"
  - "sjawhar/legion#1080"
  - "LEGION-53"
  - "LEGION-104"
  - "sjawhar/legion#1087"
---

# A jj-only driver with main's file as the negative control is the proof shape without the rig

On 2026-09-13 Sami withdrew the Legion smoke rig ("Please shutdown the goddamn legion smoke. It's
pointless and it has led to destructive actions twice now."). No scratch daemon, tmux server, or
NATS of any kind may be started for a proof. The `E2E (implementer)` line still has to name a real
surface, a command, an observation, a head, and a negative control (LEGION-53), and a green unit
suite is not it. For a change to `packages/workspace` provisioning, the accepted shape is a
**driver script that imports `provisionIssueWorkspace` and runs it against real jj on a real
scratch remote, clone, and workspace — on every jj binary the fleet runs — with `main`'s copy of
`workspace.ts` as the negative control.** The tester repeats it; the reviewer re-reads the
scratch it leaves. The live-daemon resume is *not* run, and the PR body says so in plain words.

## The driver

One file in `/tmp`, written with the `write` tool (never committed), run from `packages/workspace`
so `import("src/workspace.ts")` resolves. It:

1. takes `<045|044> <fix|control> [control-source]` — the binary (`jj` vs `mise x
   github:jj-vcs/jj@0.44.0 -- jj`) and which `workspace.ts` to load (the branch's, or a copy of
   main's extracted with `jj file show -r main@origin packages/workspace/src/workspace.ts >
   /tmp/…-old-workspace.ts`);
2. creates a **literal** root `/tmp/legion-<KEY>-e2e-<binary>-<mode>` and refuses to run if it
   already exists (it never reuses or removes a directory);
3. builds the rig `workspace.test.ts`'s `realJjRig` builds — `jj git init --colocate` a remote and a
   clone at the daemon's repo path, `bookmark set main`, `git remote add origin <remote>` — with
   `JJ_USER`/`JJ_EMAIL` on every spawn (the CI runner has no jj user config);
4. calls the imported `provisionIssueWorkspace` with a `deps.run` that records every argv, does the
   scenario (write a file in the workspace, `jj status` to snapshot, `jj git push --bookmark`,
   delete the remote branch with `git --git-dir=<remote>/.git branch -D` — what GitHub does at
   merge), and calls it again;
5. prints what the resumed worker would find — `jj status` exit code and text, file on disk,
   pushed commit in `ancestors(@)`, `jj bookmark list`, the setting's value with `-R <clone>`, from
   inside the workspace, and under `env HOME=<literal fresh dir>` — and the clone's
   `jj op log --op-diff --limit 1 --ignore-working-copy`.

Run all four combinations (`045 fix`, `045 control`, `044 fix`, `044 control`) and quote the two
op-diffs per binary in the PR body. For LEGION-84 the fix's op-diff shows only `Changed local
bookmarks` / `Changed remote bookmarks`; the control's adds `Changed commits` and `Changed working
copy widgets-42@` and `jj status` exits 1 with `The working copy is stale`.

## Why this shape counts

- **It is the production surface.** The daemon's `ProcessManager.provisionWorkspace` calls
  exactly this function with exactly these argv against exactly these binaries; the driver removes
  the daemon, not the behaviour under test. A fake command runner proves the *sequence* of
  commands; only real jj proves what the commands *do*, and 0.44.0 and 0.45.1 differ enough
  (`docs/solutions/legion/jj-bookmark-facts-verified-on-0-44-0-and-0-45-1.md`) that both must run.
- **The negative control is the same driver with the old code**, not a broken input: it shows the
  defect on the same binaries, so the fix's output is a difference, not an assertion. Add a scope
  control where the change has one (a foreign `HOME` reading the default proves the setting lives
  in the user's config directory, not the clone).
- **It leaves inspectable scratch.** The reviewer re-observed the four `/tmp/legion-84-e2e-*`
  directories read-only and matched the op-diff ids the PR body quoted. Leave them; name them.

## What it does not prove, and how to say so

It does not exercise the daemon's spawn path, the pane environment, or a live resume. Write
exactly that in the `E2E (implementer)` line — "the live-daemon resume was not run: Sami withdrew
the smoke rig on 2026-09-13" — and carry the remaining proof to the post-merge production check
(the first resume of a merged issue on the live daemon, `jj op log --op-diff` of that fetch).
Do not invent a stand-in daemon; a report to the architect that a surface is missing is the
correct output when the driver cannot reach the changed path.

## The same shape for a new function (LEGION-104): the control fails at the call

When the change *adds* a function rather than altering one, `main`'s copy of the module has no
such export. The LEGION-104 driver (`removeIssueWorkspace`) imports the module by an argv path —
the one place a dynamic `import()` is the right tool, since the path is runtime-selected — pulls
`{ provisionIssueWorkspace, removeIssueWorkspace }` out of it, provisions two workspaces, drives
the merged-branch resume, prints BEFORE (`jj workspace list`, `git worktree list`, the other
workspace's `jj log -r 'all()'`, whether the directory exists), and then calls the new function.
Against `main`'s file the BEFORE block prints today's leftover unchanged and the call throws
`TypeError: removeIssueWorkspace is not a function`, exit 1 — one run that shows both the defect
(the leftover is real on both binaries) and that today's code has no path that removes it. Against
the branch the same driver prints AFTER (the workspace gone from every listing and log, the second
call `removed: false`, the re-provisioned workspace at `main` with the bookmark on its fresh
working copy). Quote both in the PR body with the head SHA; re-run at every head the E2E line
names, since the driver is cheap (a few seconds per binary).

Three layers proved LEGION-104, and each proved only its own thing: the daemon's fake-runtime tests
pinned *what the close asks for* (the argv, their order after the last stop, the kept/removed/failed
log lines, the two guards); the package's real-jj tests pinned *what the commands do* on both
binaries (the abandoned set, the other workspace's log, the worktree list, the crash-shape repair);
the driver tied the two together outside the test runner. The tester's negative control was the
call site neutralised (the `await this.removeTreeWorkspaces(…)` line removed from a scratch copy of
the daemon): `jj workspace list` still names the closed tree's workspaces after a real `closeTree`.

## Incident rules that apply to the driver (LEGION-121)

Every path a literal under `/tmp` the script creates by name; nothing deleted, moved, or
truncated; `HOME` never read, only *assigned* to a literal fresh directory for the scope control.
See `docs/solutions/legion/probe-scripts-use-literal-tmp-paths-never-a-variable-to-rm-never-home.md`.
