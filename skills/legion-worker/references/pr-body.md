# PR body, recorded results, and the simplify pass

Part of `skill://legion-worker`. Read it before you write or edit any line of the pull request
body, record a result against one of the issue's checks, read another phase's results, or run the
simplify pass. Every path it cites is in cybermonklabs/legion.

## The pull request body template

The implementer writes the PR body from this template from the moment the PR opens, and every
later phase keeps it current rather than replacing it:

```
## For the reviewer

**Outcome:** <one sentence a user of this repository would recognise: what someone can now do, or what stops going wrong>
**Why:** <the problem, one or two sentences, ending with the Dispatch key in parentheses — the key only, never a URL>
**Change:**
- <two to five bullets, each one behaviour a user or operator meets, never a file name>
**Look at first:** <one to three `path:line` places where a wrong decision would hurt> (the reviewer writes this line)
**Proven by:** <the surface and the run of the check the change turns on, one line>
**Not proven / risk:**
- <one line per claim recorded as unproven before READY>, or the single word `none` (the reviewer writes this line; `none` is invalid while such a claim stands)
**Size:** <files changed, +added/−removed>

## Verification

**Threads:** <n> resolved, 0 unresolved. Each answered and resolved individually by the implementer, the pull request's author, never in bulk (the reviewer names the bot threads it accepted; it resolves none itself):
- Thread <node id>: fixed in <commit-sha> — <one line>; resolved at <head-sha>.
- Thread <node id>: not a defect — <reason>; resolved at <head-sha>.
- Thread <node id>: accepted by the reviewer — <its reason, one line>; resolved at <head-sha>.
- Thread <node id>: still open — <what remains>; unresolved at <head-sha>.

**Thermo:** `ce-simplify-code` once at <head-sha>: <0 applied | applied → new head <sha>>; thermonuclear pair at the reviewed head <sha>:
<verdict>. (omitted entirely on a docs-only PR — there is no code for either pass, so neither runs)

**Checks:** one line per `check` block of the issue's Verification document, in the document's
order, each the line `dispatch result` printed when the result was recorded:
- Check <block id> (<when>): <pass|fail> at <the commit's first 12 characters>, recorded by <role>. <the check's first words>
- Check <block id> (after_merge): not yet run

**Fast-follow:** "none", or one named cleanup item in a file this pull request does not touch, and where it will land.

**Chain:** stacked on <base bookmark> frozen at <sha> / not stacked.
```

## Never waiting on CI

The implementer runs the repository's fast local checks for the paths it changed before the push,
then pushes and completes. It never waits for, watches or polls the pull request's remote CI
(`gh run watch`, `gh pr checks --watch`, `sleep` around `gh run view`) and never sleeps in a loop.
Under the Go daemon a check or workflow the base branch requires that settles red on the head,
before the completion or after it, sends the work back to `implementing` with the failing checks
named, until `max_fix_attempts` is spent, when the architect decides. The one CI result an implementer waits for is a re-run: when its task names a red that
failed on its own, it re-runs the failed jobs once (`gh run rerun <run-id> --failed`) and completes once
the re-run settles green, woken by the pull request's checks event (`skill://envoy`, *Wait for
CI or a merge*: subscribe, then end the turn), never by a sleep loop. A completion while that red
still stands is sent back, at once for a required check and at the daemon's next read of the runs
(within two minutes) for a required workflow.

A pull-request body edit re-runs a check that judges the body at once, on the head as it is. While
the pull request conflicts with its base (GitHub shows it `CONFLICTING`), no workflow triggered
`on: pull_request` runs for any of its activity types, so a body edit re-judges nothing there; a
push cures it only once the pull request is mergeable, so the implementer forward-merges a
conflicting one first (*Reintegrating the base* in
`skill://legion-worker/references/conflicts-and-rewrites.md`).

## The brief for the human

`## For the reviewer` is written for the person who merges; `## Verification` below it stays the
ledger the reviewer and merger check against GitHub. The implementer writes `Outcome`, `Why`,
`Change`, `Proven by`, and `Size` when the pull request opens, and keeps them true after every
push; `Outcome` is a sentence a user of the repository would recognise, never "fix bug" or a file
name, and `Why` ends with the Dispatch key, never a URL. The reviewer writes `Look at first` and
`Not proven / risk` at each round, into the live body (`gh api
repos/{owner}/{repo}/pulls/{number} --jq .body`, edit, then `--method PATCH ... -F body=@body.md`);
`Not proven / risk` copies every claim recorded as unproven before READY — the tester's
`failures`, the reviewer's own review, any proof-check comment already on the pull request —
word for word, and `none` is a finding while one stands. The merger quotes `Outcome` and
`Not proven / risk` from the body at the head its READY packet names
(*The READY packet* in `skill://legion-worker/references/merge-gate.md`); a stale `Outcome` that no
longer describes the diff is a finding against the implementer, not a line the merger rewrites.

## What a result is

**A check** is a `check` block of the issue's Verification document (`verification.md`). It names
the requirements it serves, where a person reaches the change (`surface`: `browser`, `cli` or
`api`), what to `run`, `when` it runs (`before_merge`, `after_merge` or `default_branch`) and its
`negative_control`. The architect writes the checks with the spec; nothing else decides what must
be proved.

**A result** is one run of one check, recorded with
`dispatch result --issue <issue> --check <block id> --role <your role> --verdict pass --commit <sha>`,
with `--surface`, `--run`, `--observed` and `--negative-control` as the check asks for them. The
command prints the pull request body's line for it, and that line goes into the body unchanged.
The surface is **production-like** — the repository's real-process test harness and fixtures, a
sandbox repository, a real browser, a devN stack, staging, or a local stack with real migrations,
one that has the resource the change touches — and `--run` or `--observed` carries a **link** to
that run, screenshot, or e2e; human review does not replace user-facing verification, and a green
unit suite is not it. A unit or integration test is a regression lock, never the result of a
check. The agent that develops the change records its results this way before the merge, and
whatever blocks a check is fixed, not skipped (*When no surface reaches the changed path*, below).

**A result is current** while its commit is still the head of an open pull request of the check's
issue, or the final head of a merged one, and the check's words have not changed since; an
`after_merge` or `default_branch` result is current at a commit the default branch carries. A push
makes every result at the old head older, with no Dispatch write of anyone's, so each check is
recorded again at the new head. `dispatch results --issue <issue>` prints each check with its
latest result and whether that result is current, and `dispatch read --issue <issue>` names each
requirement of the spec no check serves.

**An issue is proved by its own checks.** Its approval is gated by the `before_merge` checks of
its own Verification document, read at its own head, so a child's pull request never makes a
root check's result current and a child never records the result that proves a root's check. A
child that carries part of a root's requirement proves it with a check on the child's own
Verification document whose `serves` names that requirement. A root's own checks are proven at
the root's own head, or on the default branch for an `after_merge` check, in the root's last
round.

**An issue with no `before_merge` check is a named gap.** An issue with no Verification document,
or with one that holds no `before_merge` check, has no evidence of its own. The tester names that
gap in its handoff and the reviewer in its review, and neither one sends the issue back for it;
both tell the architect, who writes the checks, and the architect's acceptance comment names the
gap and decides there, which is the only gate on such an issue.

## The rules every phase's evidence follows

- **The implementer records a result for every `before_merge` check before its phase completes,
  and writes the `Checks` lines when the pull request opens.**
  A check with no surface yet is the report of *When no surface reaches the changed path* below,
  never a check left unrecorded. The results are on the Dispatch issue, and the lines are in the
  PR body, because the reviewer and the merger verify facts on GitHub and never from a handoff.
  The `implement` handoff carries no results: Dispatch holds them, and the tester reads them with
  `dispatch results`.
- **The tester judges the implementer's results and records its own.** It re-runs the
  implementer's command or drives the same surface independently, and records the verdict in
  its `test` handoff as `implementerProof` (`{verdict, how}`). The tester runs the
  application, not CI again. Try to break the changed surface — wrong and boundary inputs, the
  second and the concurrent run, the party it must refuse — judge whether the implementer's tests
  would catch the ways it breaks, writing the red test for one that none would, and
  read CI on GitHub at the head you are judging for everything static: lint, types and the unit
  and integration suites are CI's, read there and recorded for no later step, and no CI lane is
  rerun as evidence. A lane GitHub shows `queued` or `in progress` is read as such: the tester
  never waits for it, since a required red that settles sends the work back from `testing` and
  the reviewer reads the head's checks before it approves.
  A `before_merge` check the implementer left with no current result is a test failure, not a gap
  for the tester to fill: record it in `failures` (each `{check, evidence}`, `check` the block id)
  with `implementerProof.verdict: "rejected"`, complete the phase with
  `verdict: "fail"`, and the daemon returns the issue to the implementer — the agent that developed the change owns
  proving it (a rejected verdict, or `failed > 0`, always comes with the failure recorded in
  `failures`). Otherwise record your own result for each `before_merge` check before you complete,
  and put each printed line in the body. A code path whose first execution is after merge — a
  deploy workflow's inline step, a post-merge helper, a production-only resource — is a check
  marked `after_merge`, recorded after the merge, unless the implementer can execute it against a
  devN stack before then; if no surface can reach it, the
  tester names that missing surface as the blocker instead of passing the phase. Environment or
  secret-scrub evidence (e.g. "`LEGION_*`/`DISPATCH_*`/`ENVOY_*` unset") is recorded once, in
  its `test` handoff, and only when a check calls for it — never
  re-pasted into the PR body each round. After a forward merge, compute the
  fingerprint (*The unchanged-diff check* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`) at the commit your results name
  and at the new head. Equal: re-run only the
  bare gates — the repository's CI green at the new head and its smoke check — and record each
  check again at the new head, its `--observed` ending
  `forward-merge re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only`. The
  real-surface verification is not repeated, except in a parent's last round, which the architect
  or the implementer's forward-merge comment names to you: then run a full test round. Different:
  a full test round.
- **The implementer runs `skill://ce-simplify-code` once per pull request, in its first
  implementing round, after its own results and before it completes that round, when the diff
  touches runtime code; a docs-only diff gets none.** The daemon starts no implementing round
  between a clean review and the approval, so this is the one slot before the tester and the
  reviewer read the head; a later round answering a review runs no second pass. It is scoped to
  the pull request's own diff at that head: nothing applied leaves the head as it is; applied →
  the implementer re-runs the repository's checks and the issue's own checks on the applied head
  for the surface the simplify diff touched, since a refactor that "preserves behaviour" is a
  claim until it is executed, and records each result at that head before it completes.
  That cost is why 0-applied is the expected outcome and a pass that applies is spent sparingly.
  The reviewer's pair runs at the head each round reviews. Record both in the `Thermo` line.
- **No deferrals** is the body's rule (*PR body, review, and the merge gate* in
  `skill://legion-worker`): a wording nit, and any clear fix in a file this pull request touches,
  is made in the running round, because a later round costs a CI pass and a re-read of the whole
  change (the operator's answer on that case: fix it while you are there, and defer no fix to
  another issue). The `Fast-follow:` line reads
  `none`, or names only cleanup in files this pull request does not touch.
  A base frozen for others to stack on is never rewritten (*Rewriting pushed commits* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`); the `Chain` line records it.
- **GitHub refuses a pull request body over 65,536 characters.** Each verification round links its
  evidence (the run, the comment) rather than inlining it once the body passes about 48,000
  characters; a `Check` line keeps its evidence in the result on Dispatch and links the rest.

## When no surface reaches the changed path

No surface reaches the changed path is a report to the architect, never a reason to complete the phase.
Say which surface is missing and what it would have to do — a rig that can spawn the role, a
sandbox that holds the resource, a credential, a command that does not exist yet — and send it to
the architect with `envoy_publish` to its role topic. The architect builds it in this tree
(infrastructure, tooling, or a skill), in an open pull request or a child issue, and resumes you
once it lands. A code path
whose first execution would be after the merge — a deploy
workflow's inline step, a post-merge helper, a production-only resource — is a check marked
`after_merge`, recorded once the merge reaches production, and no reason to leave a
`before_merge` check unrecorded; completing with a unit suite as the only evidence is the
failure this rule exists to stop.
