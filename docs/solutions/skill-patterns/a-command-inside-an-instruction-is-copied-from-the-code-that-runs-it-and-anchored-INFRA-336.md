---
title: "A command inside an instruction is copied from the code that runs it, and anchored so its answer cannot depend on the shell's directory"
category: skill-patterns
tags:
  - agent-instructions
  - prompts
  - jj
  - revset
  - fileset
  - silent-failure
date: 2026-10-31
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A command inside an instruction is copied from the code that runs it, and anchored so its answer cannot depend on the shell's directory

Extends docs/solutions/legion/gh-run-list-commit-takes-the-full-sha-and-an-empty-answer-is-not-a-measurement-INFRA-92.md.
That note's rule — an empty answer is not a measurement until the query is known to be well
formed — bites hardest when nobody reads the empty answer, because an instruction told an agent to
record a default in its place.

- **Prose that hands an agent a command is a second implementation of a code path**, with no
  compiler, no test and nothing holding the two equal. Copy the command from the source that runs
  it in production, and cite that source beside it; never retype it from memory.

- **Anchor every path in the command, so the answer cannot depend on where the reader's shell
  stands.** In jj, `-R <repo>` fixes the repository only: a `files("path")` fileset resolves
  against the shell's working directory, and `files(root:"path")` against the repository root.

- **Prove a documented command from at least two directories a reader may stand in** — the
  workspace root and a package subdirectory — before the instruction ships. One directory proves
  nothing about position dependence, and the author always runs it from the root.

- **Where the instruction takes a default on empty output, write "succeeds and prints nothing".**
  Otherwise a non-zero exit with empty stdout — a wrong repository, a parse error, a missing
  binary — is recorded as the default, and the record then looks measured.

## Evidence

INFRA-336 added `previousHandoff` to every Legion handoff and told the worker to read it with

```
jj -R "$LEGION_WORKSPACE" log --no-graph -r 'latest((::@- ~ ::trunk()) & files(".legion/<issue>/<phase>.json"))' -T commit_id
```

The function this mirrors, `findHandoffCommit` in `packages/pi-shared/src/handoff-commit.ts`,
builds its fileset as `root:` plus the path. The prompt dropped the prefix. Measured in an issue
workspace: from the workspace root the command printed the commit; run unchanged from
`packages/pi-legion` it printed nothing and exited 0; from `/tmp` it failed outright with a
fileset parse error. The instruction then told the worker to write `""` for the empty case, so the
subdirectory run — the plausible one — recorded a default that reads like "there is no previous
handoff", which is precisely the duplicate-bytes state the field exists to prevent.

The fix was the one qualifier, `files(root:"…")`, which answers the same commit from all three
directories. A review round of the same pull request fixed the second half: the sentence's "an
empty string when that command prints nothing" became "succeeds and prints nothing", so a failed
call is no longer recorded as an absent predecessor.
