---
title: "A sentence you keep in a merge is a claim about the other side's code, and a presence grep does not check it"
category: best-practices
tags:
  - forward-merge
  - merge-conflicts
  - documentation-edits
  - logging
  - verification
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2542"
  - "sjawhar/legion#2504"
---

Extends `docs/solutions/daemon/a-forward-merge-brings-mains-new-outbox-writer-in-unscoped-the-commit-scope-census-runs-again-and-the-racy-wakes-error-line-is-the-tests-witness-LEGION-694.md`,
which runs its census the other way — main's new code against the branch's invariant. This is the
mirror: the branch's own sentences against main's new code. It also extends
`docs/solutions/best-practices/a-word-level-merge-of-prose-is-verified-by-reading-the-paragraph-INFRA-42.md`,
whose check is grammatical: the paragraph here read perfectly and was false.

## Rules

- **Read "keep both sides" as two different operations.** For a sentence about your own change it
  is text. For a sentence about behaviour the other side owns it is a claim, and the conflict is
  the other side announcing that it changed that behaviour. Re-derive the claim from the merged
  code before you keep it.
- **The conflicting pull request names itself.** When every conflict of a merge is with one recent
  change, read that change's own entry (its changelog fragment, its `AGENTS.md` bullet) for the
  behaviours it renamed or re-routed, then grep your kept prose for those nouns.
- **A presence grep answers "something logs this string", never "this path logs it".** The
  predicate for a prose claim about a log line is the **guard of the `slog` call and its attribute
  list**, both of which are a four-line read. Add that row to the instrument table in
  `docs/solutions/best-practices/an-instrument-that-answers-a-smaller-question-returns-the-comfortable-answer-INFRA-141.md`:
  the narrow answer is the comfortable one, because it lets the merge land.
- A claim that names a message **and** an attribute is two conjuncts, and a re-route can falsify
  each separately.

## Evidence

sjawhar/legion#2542's forward merge of main hit four two-sided conflicts, every one in a file
sjawhar/legion#2504 (`fix(envoy): a rewatch the listener repairs is a warning, one that never
succeeds is on /healthz`) also changed. The resolution kept this branch's clause that *each
recovery retry logs `envoy nats recovery resubscribe failed` with an `attempt`*, verified by
grepping that the string was still in the tree. It is: `recoverUntilHealthy`
(`packages/envoy/internal/bus/recovery.go`) sets `failure` to that message and logs it under
`if err != nil`, where `err` is the reconnect and `restoreSubscriptions` error alone. #2504 had
deliberately moved a failed run of the hooks out of that condition — `hookErr` is joined into
`attemptErr` for the loop's control flow only, and reported by `reportHookFailure`, which logs
`envoy nats reconnect hook failed; it is retried` (or, at `RepairGrace`, `envoy nats reconnect
hooks have not succeeded`) with `stall_ms` and no `attempt` at all. The kept clause was wrong in
its message and in its attribute, and it stood in three places: `packages/envoy/AGENTS.md` and
both settings descriptions in `packages/envoy/cmd/dispatch/settings.go`. The reviewer read the
code path and removed it in `9eddf894b532`, keeping the branch's other clause, the redelivery
sweep's (`internal/dispatch/redeliver/sweep.go`), which the same read showed true.
