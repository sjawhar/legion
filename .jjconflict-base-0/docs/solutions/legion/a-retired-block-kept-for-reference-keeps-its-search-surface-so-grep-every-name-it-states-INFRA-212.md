---
title: "A retired block kept for reference keeps its search surface, so grep every name it states and mark it where readers enter"
category: legion
tags:
  - documentation-drift
  - retired-paths
  - census
  - runbook
date: 2026-10-24
status: active
module: docs/kubernetes.md
applies_when:
  - A change deletes an implementation but keeps its documentation section "for reference"
  - You are marking a section of a long document as superseded
  - A long document holds two generations of one system under near-identical headings
related_issues:
  - "INFRA-212"
  - "sjawhar/legion#2152"
---

# A retired block kept for reference keeps its search surface, so grep every name it states and mark it where readers enter

Extends docs/solutions/legion/a-change-that-retires-a-path-or-mechanism-greps-the-old-string-repo-wide-not-the-diff-LEGION-636.md.
That document sweeps the retired string out of the prose nobody touched. This is the case where
the prose is kept on purpose, so no sweep is coming, and the delta is what the keeping costs.

- **Grep the whole document for every identifier the kept block states, and compare the hit
  counts.** An identifier with exactly one valued match, inside the kept block, is the trap: the
  retired text is the file's only answer to that question, and a search lands on it with
  certainty rather than with a choice. Restate it live in the same change that retires the block,
  or the retirement creates the defect.
- **Mark the retirement at the scope readers enter, not at the scope you reorganised.** A reader
  arrives at a line by full-text search or by an anchor, and reads upward only to the nearest
  `###`. One sentence under the `##` is invisible from there. Put the marker on each subsection
  and directly above each table.
- **Remember that a retired block loses its link surface while keeping its search surface.** The
  absence of inbound links is not evidence that nobody reaches it — it is the reason nobody
  noticed it was wrong.
- **Marking it is a line; moving it is a code change.** Before proposing that the retired text be
  moved or deleted, count what points into it by *title* as well as by anchor: an error message,
  a skill, an agent definition or a deployment file naming the heading in prose is a pointer no
  anchor checker sees, and renaming the heading breaks it silently.

## Evidence (INFRA-212, sjawhar/legion#2152)

`docs/kubernetes.md` is one 4244-line runbook holding two generations of the same system:
`## Kubernetes runtime: the Go daemon on Agent Sandbox` and, below it,
`## Kubernetes runtime (TypeScript daemon)`, whose sources were deleted on 2026-10-03
(`ed00df8b25`, "Legion runs on the Go daemon alone") and whose text was kept for reference.

The two pod sections are `### Anatomy of a Sandbox pod` and `### Anatomy of a pod` — one word
apart, 1930 lines apart, with distinct slugs. At the fork point `9a0d549f47b6`, `LEGION_STATE_DIR`
had exactly **one** valued statement in the whole file, line 3736 of the retired table, and it
said `/legion`; the live value is `/var/run/legion/state`, and the live section stated none of the
eleven variables at all. So a grep for the variable had a 100% chance of returning the retired
answer, and the only marker saying so was 594 lines above it, under the `##`. Every row of that
retired ten-row table differed from a live pod when the issue was filed.

`](#anatomy-of-a-pod)` appears **zero** times anywhere in the repository: the retired section was
unreachable by navigation and reachable only by search, which is how an agent reads a
four-thousand-line runbook.

The no-deletion scope was chosen because of the pointer asymmetry. Seventeen places point at the
retired section's `### Operator configuration` — only **four** are in-document anchors; the rest
name the heading by title from a user-facing Go error string
(`packages/daemon/internal/config/kubernetes.go`, with a test asserting its wording), `AGENTS.md`,
both copies of the worker skill, six `packages/pi-legion/agents/*.md` and two files under
`deploy/kubernetes/operator-route/`. Renaming that heading breaks a daemon error message while an
anchor check stays green.

The fix marked each of the six retired subsections and each of the five tables inside them with
one line, and stated the eleven values live. It converts "wrong and unmarked" into "wrong and
marked within one screen"; the retired rows are still wrong line by line, which is the accepted
cost of keeping them.
