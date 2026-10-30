---
title: "A base gate arrives in the merge, and the job your branch alone adds is the line it refuses"
category: github
tags:
  - github-actions
  - forward-merge
  - ci-gates
  - merged-tree
  - proof
date: 2026-10-30
status: active
module: .github
related_issues:
  - "sjawhar/legion#2517"
---

# A base gate arrives in the merge, and the job your branch alone adds is the line it refuses

Extends docs/solutions/github/a-gate-that-forbids-a-pattern-tree-wide-is-a-gate-on-the-merged-tree-pr2512.md.

That page is written for the branch that **owns** a tree-wide gate and meets the base's violation.
This is the other side: the gate arrives **in** the merge, and the violating line is one your
branch wrote and nobody else can see.

- A forward merge resolves the lines that conflicted. A gate the merge brought judges every line
  of the merged tree, your branch's own among them. The base converted every site it could see;
  the site only your branch has was invisible to it, so it is still in violation and no conflict
  marked it.
- Expect the file to **read as converted**. The base's edit sits a few hundred lines above yours
  in the same file, in the sibling job your job was copied from, so a reader scanning the diff
  sees the new form and stops looking.
- After any merge that adds scripts under `.github/`, take the gate inventory from the merged
  tree, never from memory: the `lint` job's step list and the base's delta under
  `.github/scripts/`. Run every gate your branch has never run, over the whole tree, **before**
  you re-run the ones you already pass. A proof made only of your branch's own checks says
  nothing about the gate that arrived, and your reviewer or tester will say so.
- Classify each red at the merged head by its owner before you edit anything. A gate's red is
  yours; a hand-kept pin or count that the base moved under you is the base's, and the base's own
  commit usually carries the value with the change that needed it, so the merge closes it and an
  edit on your branch would be work against a conflict.

## Evidence

sjawhar/legion#2517 adds the race-lane job `envoy-go-race-api`, copied from `envoy-go-race`,
each with a Postgres `services:` entry. Its round-6 forward merge brought in #2512's
`.github/scripts/check-ci-images.sh`, a step of the `lint` job (`.github/workflows/ci.yaml:1257`),
which refuses any CI-reachable reference to a registry outside this repository's own package and
requires each image to be a row of `.github/ci-images.yaml`; its sweep rule is at
`check-ci-images.sh:104-111`. The merge carried main's conversion of `envoy-go-race`'s service
(now `ci.yaml:2487-2488`) and left the identical line of `envoy-go-race-api` (now `:2551-2552`)
unconverted, 64 lines apart in one file.

Two **required** checks were red at the merged head for that one line: `lint` (run 41104365691,
job 124233792975, naming `.github/workflows/ci.yaml:2551` and `2 problem(s)`) and `test` (job
124233792837, the harness case in `check-ci-images.test.sh`, `97 passed, 2 failed`). The
implementer's merge proof had re-driven the branch's own gates — the filter drift check, the two
flake loops, the route-pin test — and never the gate the merge had just brought, so the tester
rejected the proof and failed the round on this finding. The fix was one commit, `c92ffe58`: the
comment the base uses and the same digest under this repository's own package, after which
`check-ci-images.sh tree`, its suite (99 of 99), `check-image-trigger-paths.sh` (991 of 991) and
the rest of the `lint` job's scripts were run before the push.

The counter-case is in the same merge. `ci-ok` had been red at the previous head on
`TestRoutesTablePinsEveryRegisteredRoute` (`routes_table_test.go:111`, `routes() has 147 rows,
want 146`): the base had added a route. The merge brought the base's own pin with it
(`routes_table_test.go:108`, `const registeredRoutes = 147`), the test passed at the merged head
with no edit, and the branch corrected nothing.

This page names no registry outside this repository, because the gate it describes refuses any
file of CI's reach that does, and a page is in that reach.
