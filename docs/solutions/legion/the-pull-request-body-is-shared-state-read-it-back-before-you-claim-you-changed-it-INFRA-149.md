---
title: "The pull request body is shared state several roles edit in turn: read it back after your edit, and claim in a handoff only what you observed"
category: legion
tags:
  - pull-request-body
  - handoff
  - evidence
  - phase-workers
date: 2026-10-24
status: active
module: legion-worker
related_issues:
  - "INFRA-149"
  - "sjawhar/legion#2143"
---

# The pull request body is shared state: read it back after your edit, and claim only what you observed

- **Your local idea of the body is not evidence.** The implementer writes it, the reviewer writes
  `Look at first` and `Not proven / risk` into it, the tester rewrites its own `E2E` line, and
  retro rewrites what the repository derives from the changed paths. Between two of your own turns
  it has moved. Read it with
  `gh api repos/{owner}/{repo}/pulls/{number} --jq .body` **after** the edit, not only before, and
  check the lines you meant to change actually changed.
- **A handoff line describes something you observed after doing it, never something you set out to
  do.** Write the handoff's claims about the body once the read-back is in front of you. Composing
  them from an intent list while still editing is how a line that was true when planned becomes
  false when committed.
- **A partly true sentence is the dangerous shape.** "I refreshed the `Size`, `Threads` and `CI`
  lines" travels on the credibility of the two that are true; nobody downstream re-checks the
  third. Split the claim or verify all of it.
- **If you patch the body with a script, assert every replacement.** One assertion per line you
  claim to have changed costs nothing and converts a false handoff claim into a failed command.

The reader this protects is the reviewer, the merger and the architect at acceptance, each of whom
is told to verify facts on GitHub rather than from a handoff. The cost of a false claim is paid
only when one of them trusts it instead.

## Evidence

On sjawhar/legion#2143 the implementer's round-2 handoff recorded that the body's `Size`,
`Threads` and `CI` lines had all been refreshed in that push. `Size` and `Threads` had been;
`CI` had not. It still named round one's code head `946624b2` and still said the heads above it
carry `skip-checks: true` and have no runs — false of that round's head, a code-push head with 23
check runs. The tester found it while verifying the body against the forge, rewrote the line at
the current head and recorded the discrepancy rather than failing the round.

The implementer had patched the body with a script that asserts each replacement string is
present before writing; the `CI` line was simply not among the replacements, while the sentence
claiming it had been was already written. One more assertion would have failed the command
instead of shipping the claim.
