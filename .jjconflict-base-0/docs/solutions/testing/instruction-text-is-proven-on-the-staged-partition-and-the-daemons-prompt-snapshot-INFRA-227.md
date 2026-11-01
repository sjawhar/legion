---
title: "Prove a skill or role-prompt change on the staged npm partition and the daemon's own prompt snapshot, not on the file you edited"
category: testing
tags:
  - skills
  - role-prompts
  - proof
  - negative-control
  - prepack
  - skills-guard
date: 2026-10-25
status: active
module: skills
related_issues:
  - "INFRA-227"
  - "sjawhar/legion#2155"
---

# Prove a skill or role-prompt change on the staged npm partition and the daemon's own prompt snapshot, not on the file you edited

Extends docs/solutions/legion/text-only-skill-pr-mechanics.md, which covers the commit mechanics of
a prose PR and the rule that a grepped phrase stays on one line. The delta is what counts as a
production-like surface for instruction text, and how to make the proof non-vacuous.

Instruction text has one source and three shipped surfaces: the npm skills partitions a pane loads,
the role prompts the `legion` binary embeds, and the generated Claude Code plugin tree. Grepping the
file you edited proves none of them.

- **The pane's surface** is the staged partition, reproduced with no pack and no install:
  `bash scripts/pi-plugin-prepack.sh --stage-skills @sjawhar/pi-legion <new dir>`. That directory is
  what `skill://legion-controller` resolves to in a pane. Stage the other plugin too
  (`@sjawhar/pi-envoy`) when the sweep is meant to cover every skill a session loads; the
  destination must be new or empty.
- **The daemon's surface** is the snapshot it writes before any pane starts, not the bytes of the
  binary: drive `prompts.New(stateDir)` and `ControllerPromptPaths(true)` from a throwaway `go run`
  program inside `packages/daemon` and grep the files it wrote. A `grep -a` over a fresh
  `go build ./cmd/legion` is the corroborating second observation. Delete the throwaway package
  before you finish — anything left under `packages/daemon` joins the module build — and say in the
  proof that you did.
- **A negative control for prose is the old text put back.** Re-add the sentence the change
  removed, re-run the same command on the same surface, and show it turn: that is what separates a
  real check from a grep that would pass on an empty tree. A grep for a common word (`ruling`) or
  for the new sentence alone is vacuous; use `grep -c "<the exact new sentence>"` in every copy,
  plus the absence of the old shape.
- **Run the Oh My Pi-gated guard rules locally** with `LEGION_TEST_OMP=/opt/omp/bin/omp` before
  relying on CI: without it they skip silently, and on Actions they cannot skip
  (`.github/actions/install-omp/action.yml` exports the variable, and the guard throws when it is
  unset there).
- **Budgets that constrain the rewrite:** a skill body is refused at 500 lines or more, and a
  staged file over 50 KiB is cut when a pane reads it through `skill://`
  (`packages/pi-shared/test/skills-guard.ts`). A rewrite that lengthens a paragraph may legitimately
  need one more line; say which reading of "no other line moves" you took, and never write an
  acceptance criterion that pins line numbers of lines the change does not touch.

## Evidence

INFRA-227 (sjawhar/legion#2155) rewrote ten lines so Legion's controller skill, its reference and
the daemon's controller role prompt name a role instead of an individual.

- Staged partition: `grep -rni <name> /tmp/stage-head` exited 1 over the whole staged tree at the
  change's head and printed four lines at the base commit; the staged `legion-controller/SKILL.md`
  carried the new ruling definition exactly once. The tester's control re-added the old sentence and
  found it at `legion-controller/SKILL.md:490` of a freshly staged partition.
- Daemon snapshot: a throwaway `go run ./.tester-probe` wrote
  `<state>/prompts/shared/controller-root.md` and `<state>/prompts/go/controller-headless.md`, the
  two files a headless controller is handed; the name was absent there and present again when the
  old line was restored, proving the snapshot tracks the source rather than a cache. The binary
  check agreed: `grep -aci <name>` 0 and `grep -ac "a person's rulings"` 1, against 1 and 0 at the
  base.
- Guard control: appending `Legion never merges a pull request.` to the rewritten skill made
  pi-legion's guard fail (`no skill or prompt says Legion never merges`, 9 pass / 1 fail), which is
  how we knew the guard reads the file the change rewrites rather than a copy.
- Blindness measured, not assumed: with the removed name put back in the controller skill, both
  guard suites still passed (pi-legion 10/0, pi-envoy 9/0 with `LEGION_TEST_OMP` set) and the name
  reached the staged partition — the issue shipped no check by the human's decision, so that
  measurement is the statement of what a reviewer is now the only barrier against.
- Line coupling: the controller skill stayed at 488 lines (bound 488, guard refusal 500) while the
  reference's paragraph went from three lines to four, because it holds 302 characters at the
  repository's 100-column wrap; the acceptance criterion pinned two unrelated dependency-version
  lines at `packages/envoy/AGENTS.md:529` and `:605`, which forced the edit at line 280 to be
  exactly two lines.
- A merged instruction change is not deployed: a pane loads the released `@sjawhar/pi-legion` and a
  prompt compiled into the deployed daemon, so the production check after the merge reads the
  installed plugin in a pod and reports `pending` until a plugin and daemon built after the merge
  are installed.
