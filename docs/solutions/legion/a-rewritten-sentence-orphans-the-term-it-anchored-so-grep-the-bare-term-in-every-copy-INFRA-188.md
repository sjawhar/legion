---
title: "A rewritten sentence orphans the term it anchored, so grep the bare term in every copy of the text"
category: legion
tags:
  - documentation-drift
  - census
  - agent-facing-text
  - dispatch-tools
date: 2026-10-25
status: active
module: skills/dispatch/references/issues.md, packages/contracts/src/dispatch-tools.ts
related_issues:
  - "INFRA-188"
  - "sjawhar/legion#2174"
---

# A rewritten sentence orphans the term it anchored, so grep the bare term in every copy of the text

Extends docs/solutions/legion/a-change-that-retires-a-path-or-mechanism-greps-the-old-string-repo-wide-not-the-diff-LEGION-636.md.
That document sweeps a **retired string** out of prose the diff never touched. The delta is the
case where nothing is retired: the term survives, and so does every sentence using it, but the
one sentence that told a reader what the term *was* has been rewritten by this very diff. The
census for it greps the bare term, not the retired phrase, and judges each hit against the new
definition.

- List the terms whose meaning your diff changes — not the sentences you edited — and grep each
  bare term across `skills/`, every `AGENTS.md`, every package `README.md` and the published
  docs. Judge each hit: a hit that is still true under the new definition stays; one that was
  true only because of the sentence you rewrote is the finding.
> Since INFRA-140, `skills/dispatch/references/` no longer exists: a rule about one command lives in its
> `--help` (`packages/contracts/src/dispatch-tools.ts`). The lines below record the files as they were.

- An agent-facing claim about a tool lives in up to seven places in this repository:
  `skills/dispatch/references/`, `skills/legion-controller/references/`,
  `packages/contracts/AGENTS.md`, `packages/envoy-client/README.md`,
  `packages/pi-envoy/AGENTS.md`, `docs/site/src/content/docs/dispatch/`, and the committed
  `packages/claude-envoy/{dist,legion}/` mirror, which is regenerated and never hand-edited. A
  spec that says "four texts change" is a count to re-derive, not a scope.
- Read the sentence a reader meets in sequence, not the paragraph you wrote. A clause that is
  true of its own subject can be composed with the clause beside it into a claim the server
  refuses.

## Evidence (INFRA-188, sjawhar/legion#2174)

The change gave `dispatch issue-update` the board move and rewrote the bullet that had taught the
move as a hand-built `PATCH /api/v1/issues/{key}` with a `{"rank": …}` body
(`skills/dispatch/references/issues.md:46`). Six lines above it, `:40` read "A closed issue takes
only `rank`, `components`, a reopening status, …" — untouched by the diff, and now the only text
naming a field for which the tool has no argument. It was `rank`'s sole remaining definition that
the same diff had removed. Review round 1 found it; a grep for the bare word `rank` over
`skills/` at implementing time would have. The two other surviving hits are true under the new
definition and stay: `:45` (reads carry `rank`) and `:7` (the order an agent chooses its own next
issue by, priority then board rank, deliberately left).

The same round's second finding has the same shape one level down: the spec's `example`
(`packages/contracts/src/dispatch-tools.ts`) is a copy of the tool's argument shape that
`--help` and every refusal print, and `packages/contracts/AGENTS.md:62` requires it to change with
the shape. It was not in the diff's file list because it is not prose.

One sentence in the shipped text is still composable into a claim the server refuses, judged a
non-defect in review and recorded here so a later edit can close it:
`skills/dispatch/references/issues.md:46` ends "The move rides along with any other change in the
same call, and a closed issue takes it." Read straight through, that licenses
`--before X --priority 1` on a closed issue; `issue_patch.go:201-215` answers `409 ISSUE_CLOSED`
and rolls the whole PATCH back, because any other field beside the move on a closed issue is
refused. The rule is stated correctly at `:40` and in the tool description; "…and a closed issue
takes the move alone" costs two words.
