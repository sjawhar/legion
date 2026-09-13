---
title: "A sibling PR's promise to move a line is verified by reading its + lines at its current head, not by seeing a hunk"
category: legion
tags:
  - legion
  - implementer
  - cross-issue
  - pr-body
  - scope
  - verification
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-32"
  - "sjawhar/legion#983"
  - "LEGION-10"
  - "sjawhar/legion#957"
---

# A sibling PR's promise to move a line is verified by reading its `+` lines at its current head, not by seeing a hunk

## What happened

LEGION-32 replaced the OMP fork pin. A repository grep for the old version found one copy outside
the changed file: `scripts/smoke/README.md:73`. That file was open in LEGION-10's pull request #957,
whose architect had asked on LEGION-32 that `scripts/smoke/` be left alone because "#957 already
moves the version string in `scripts/smoke/README.md`". The LEGION-32 spec repeated the claim; the
implementer fetched #957's diff, saw a `-`/`+` pair for line 73, and wrote in the PR body that #957
"rewrites that exact line".

It did not. At #957's head `37b17c5a` the `+` side of that hunk re-added line 73 with the *same* old
string `18.1.15-sami.20260908-220934` — the hunk existed because the paragraph around it changed. The
tester caught it with `legion gh -- pr diff 957`; the reviewer required the PR body corrected before
approval. The PR passed through one head where a load-bearing scope justification was false.

## The check

A hunk touching the line proves the sibling PR edits the *area*. Whether it changes the *value* is a
separate question, answered only by the `+` lines:

```
legion gh -- api repos/<owner>/<repo>/pulls/<n>/files \
  --jq '.[] | select(.filename=="<path>") | .patch' \
  | grep '^+' | grep -n '<old literal>'
```

Non-empty output means the sibling PR still carries the literal at its current head, whatever its
description or its architect says it will do. Run it against the head you are citing — a PR's diff
changes under you — and record that head sha next to the claim.

## Resolving it without editing the other PR's file

Two PRs changing the same line conflict in the merge queue, so "edit it anyway" was still wrong. The
fix was a cross-issue agreement, made durable in the *other* issue's record: on 2026-09-13 the
LEGION-10 architect agreed to rewrite README line 73 in #957 to refer to the pin source ("where
`<pin>` is the single Oh My Pi fork pin printed by `bun packages/daemon/src/daemon/omp-pin.ts`")
instead of a literal version, recorded in LEGION-10's specification version 16, with the merge
order stated: #957 lands after #983 because its end-to-end run needs the fixed release as the
default. The LEGION-32 PR body's proof 4 then cited that spec version, not a promise.

When a grep finds a stray copy of the value you are changing in a file another in-flight PR owns:
escalate to that PR's architect, get the decision written into their spec with a version number,
state the merge order, and quote the spec version in your PR body. A comment saying "we'll handle
it" is not an anchor; a spec version is.
