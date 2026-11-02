---
title: "A path query answers the commit that deleted the path too"
category: best-practices
tags:
  - revsets
  - git-log
  - path-queries
  - deletions
  - prose-and-code
date: 2026-10-31
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A path query answers the commit that deleted the path too

`files(<path>)` in a jj revset, and `git log -- <path>`, select every commit whose diff **touches**
the path: the one that created it, the ones that edited it, and the one that deleted it.

- **A value read with a path query is "the newest commit that changed this file", and nothing
  more.** Describing it as "the commit that produced X" or "that carried X" is wrong the first
  time anything deletes X — and in a workflow where a documented step is "delete this directory",
  something always does.

- **When the query's answer and the prose describing it disagree, name the consumer and read what
  it depends on.** Fix the prose when the consumer needs only a property the answer still has, such
  as *it differs from last time*. Fix the query — exclude deletions, or select by content — when
  the consumer resolves the identity the prose claimed, such as reading the previous version's
  contents.

- **Say which one you did and why**, because the two fixes look alike in a diff and read
  differently to the next maintainer.

## Evidence

INFRA-336 gave every Legion phase worker a `previousHandoff` field, read with

```
jj -R "$LEGION_WORKSPACE" log --no-graph -r 'latest((::@- ~ ::trunk()) & files(root:".legion/<issue>/<phase>.json"))' -T commit_id
```

and described in the prompt as "the commit that carried this role's previous handoff of this
phase". Every Legion retro then ended by removing `.legion/<issue>/`, and a round after a retro
wrote the handoff again, so the query's newest match was retro's **removal** commit. Since
sjawhar/legion#2354 a handoff is a record on the Dispatch issue, not a file, and retro removes
nothing. Measured on this
pull request: the revset answered `ec7c7ad0`, the commit that deleted the directory, for both
`implement.json` and `test.json`.

The consumer is the daemon's refusal of a handoff commit it has already recorded: the field exists
to make a repeated round's bytes differ, and retro's commit does that as well as any other. So the
prose was fixed, not the query — the prompts now say "the newest commit on this branch that
changed `.legion/<issue>/<phase>.json` (usually this role's previous handoff; after a retro,
retro's removal)". Had any consumer read the previous handoff's contents, the same mismatch would
have made the query wrong instead.
