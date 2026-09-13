---
title: "A rule that spans the Envoy listener and the daemon reducer: check the normalizer before the spec names a field, forward facts not policy, converge in either webhook order, and degrade to yesterday's behaviour under deploy skew"
category: daemon
tags:
  - reducers
  - envoy
  - normalize
  - webhook
  - push
  - fix-attempts
  - pr-blocked
  - deploy-skew
  - state-migration
  - mutation-proof-tests
date: 2026-09-13
status: active
module: daemon
related_issues:
  - "LEGION-33"
  - "sjawhar/legion#993"
  - "LEGION-23"
  - "sjawhar/legion#966"
symptoms:
  - "`pr-blocked` fires on every `.legion/*.json` handoff push to a red PR"
  - "a spec says 'the daemon reads the push webhook's file lists' but the daemon never sees a push payload with file lists"
  - "two GitHub webhooks describe one commit and the daemon's answer depends on which arrives first"
---

# A Rule That Spans the Envoy Listener and the Daemon Reducer

LEGION-33 (PR sjawhar/legion#993) made a fix attempt mean "a new head on a red PR whose push
changed something outside `.legion/`". The rule is one sentence; landing it needed a change in
the Go listener, three new state fields, a two-webhook convergence pattern, and a deliberate
degradation mode. Each of those is reusable the next time a rule needs a fact the daemon does
not yet receive.

## 1. Check the normalizer before the spec names a webhook field

The spec's first draft said the daemon would classify from the push webhook's per-commit
`added`/`removed`/`modified` lists — which GitHub does send. It was wrong about what the
**daemon** receives, on two counts:

- The daemon learned of a new head only from the pull request `synchronize` webhook
  (`pullRequest` in `reducers.ts`), which carries no file list; the push webhook was thrown away
  at the top of `reduceGithubEvent` (`if (repo && ref.startsWith("refs/heads/legion/")) return []`).
- Even had it been read, the listener's normalizer had already reduced a push to
  `kind/repo/ref/after/before/pusher/head_subject/commit_count/compare_url` (`githubPayload`'s
  `case "push":` in `packages/envoy/internal/contracts/normalize.go`). Reducers never see a raw
  GitHub body — every GitHub payload they read is the flat string map that function produces.

So a spec or plan that names a GitHub webhook field is checked against `normalize.go`'s case for
that event, not against GitHub's documentation. If the field is not in the flat map, the change
has a listener half, and the two halves deploy separately (below). The planner on LEGION-33
caught this before implementation; it is the first thing to check, not the last.

## 2. The listener forwards facts; the daemon owns the policy

The listener gained `changed_paths` (unique paths across every commit's three lists, first-seen
order, newline-joined, capped at 100) and `changed_paths_truncated` (`"true"`/`"false"`), computed
by `githubPushChangedPaths` with **no** knowledge of `.legion/`. The daemon's `classifyPush`
owns `HANDOFF_PATH_PREFIX` and the every-path rule. Envoy's own guidance ("It does not own Legion
workflow policy") is the reason, and the payoff is concrete: the `.legion/` rule can change in
TypeScript, unit-tested in-process, without touching Go or redeploying the listener.

Two wire facts bit during implementation and belong in any consumer of a new normalized field:

- `payloadJSON` drops empty-string values. A push listing no commits therefore has **no**
  `changed_paths` key on the wire, exactly like a push from a listener that predates the field.
  The always-present boolean (`changed_paths_truncated`) is the "new listener" marker; the paths
  key's absence alone is ambiguous. `classifyPush` reads the boolean first for this reason.
- A boolean transmitted as a string has three values, not two. `"true"`, `"false"`, and anything
  else each need a branch and a test — the `"maybe"` row in `reducers.test.ts` was a review
  finding, and deleting the `!== "false"` branch made exactly that row fail.

## 3. Two webhooks for one commit, no ordering contract: a consumed pending slot, not a heuristic

GitHub sends the push webhook and the PR `synchronize` webhook for the same commit with no
promised order. The daemon's answer for one push must not depend on which arrives first. The
pattern that made it order-independent is three optional `PrState` fields:

- `pendingPush?: { sha, handoffOnly }` — the push arrived first. One slot, keyed by sha, latest
  push wins. `resetPrHead` **consumes** (deletes) it when a head with that exact sha arrives —
  from the synchronize webhook or from resync's GitHub read alike — so a stale slot can only ever
  describe the commit it names.
- `headCounted?: true` — the synchronize arrived first and was counted. A later handoff-only push
  for that head takes the attempt back. Literal `true`/absent, never `false`: one representation
  of "not counted".
- `blockedAttempts?: number` — the count `pr-blocked` was last published for. A take-back whose
  pre-decrement count equals it deletes it, so the next real fix republishes; and
  `reduceCiEmission` publishes only when `fixAttempts !== blockedAttempts`, so a repeated red
  verdict at the same count is silent.

Write the tests in **both** orders, and also the round-alternating order
(`six handoff-only pushes count nothing whichever webhook arrives first each round`). A single-order
test proves half the contract. The cross-tree case (push for a newer head arriving while the
current head is counted) also needs its own row: it proves the slot is keyed by sha, not "the
next head".

## 4. Under deploy skew, unknown degrades to yesterday's behaviour — loudly

The listener and the daemon are upgraded by hand and separately. Until both run the new code:
daemon-only sees every push as "changed_paths absent"; listener-only has the daemon ignoring
the new fields. The design that made that safe: every shape the daemon cannot classify (no
`changed_paths_truncated`, `"true"`, an unrecognised value, no commits listed) **counts exactly
as before the change** and emits one `{ kind: "log", message }` effect naming the PR and the
reason. A missing signal degrades to the prior behaviour, never to silence (`pr-blocked` could not
quietly stop firing) and never to the new behaviour (a handoff push could not be un-counted on a
guess).

The `log` effect itself was the cheapest way to get a reducer — pure, no logger — to say why:
`events.ts`'s exhaustive `dispatch` switch executes it as `console.warn("[legion] …")`, it cannot
fail (so the durable lane's dispatch-before-save contract is untouched), and a test can assert it
as data instead of spying on console.

## 5. A version-bump-only migration is fine — say why in the migration

`migrateV24State` is `{ ...state, version: 25 }`. Absent is the correct starting value for all
three fields on every existing record, and the doc comment says so — including the one visible
consequence (a record already at or past the limit publishes `pr-blocked` once more after the
upgrade, then stops). The next migration's author audits v24→v25 from that comment, not by
re-deriving it. The test that locks it: load a v24 file, expect the same PR record with no new
keys and a `<file>.v24.bak` beside it.

## 6. Prove a review-added test row is load-bearing before pushing it

Review found three rows the suite lacked (a `.legion/` path **first** in a mixed push; an
unrecognised truncated value; exactly 100 unique paths plus a duplicate). Each was landed with a
break-then-fix proof: mutate the guarded code path (`split("\n")[0]` instead of `.every`; delete
the `!== "false"` branch; swap the `seen` and cap checks in `githubPushChangedPaths`), watch
exactly that row fail and nothing else, restore. The fresh-eyes retro reviewer's point stands:
the cap-boundary row (`len(paths) == max` checked before append, in the same iteration) is the
kind of edge the first red test should have targeted — write the boundary case with the plain
truncation case, not after review. The general method is
[mutation-proof-probe-tests](../testing/mutation-proof-probe-tests.md).

## What the retro reviewer would have kept

The `README.md`-only push row was dropped at review as a duplicate of the golden replay
(`envelope-goldens.test.ts`, `docs/acceptance.md` + `README.md`). That golden does cover the
"no `.legion/` path at all" case, so coverage held — but when a reviewer says "drop the sibling",
check what still covers the sibling's distinct edge before deleting it, and name the covering test
in the thread reply.
