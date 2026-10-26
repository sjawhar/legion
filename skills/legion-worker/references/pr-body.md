# PR body, proofs, and the simplify pass

Part of `skill://legion-worker`. Read it before you write or edit any line of the pull request
body, put a `proof` array in a handoff, verify another phase's proof, or run the simplify pass.
Every path it cites is in sjawhar/legion.

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
**Proven by:** <the `E2E (implementer)` line's surface and run, one line>
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

**E2E (implementer):** <surface> — ran `<command or run id>`, observed <result>, at head <sha>.
Negative control: <deliberately broken input or call> → <refusal or failure observed>.

**E2E (tester):** <surface> — ran `<command or run id>`, observed <result>, at head <sha>.
Negative control: <deliberately broken input or call> → <refusal or failure observed>.
Verified the implementer's proof by <re-running its command | driving the same surface independently>.

**Production:** <what was checked in production, how, what was observed> — merge commit <sha>; woken by <event>.
(written by the implementer from the production check after the merge closes the issue, naming what woke that look; `pending deploy of <sha>` until then)

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

## What a proof is

**A proof** is the changed behaviour exercised on the surface a user reaches it through, recorded
as the exact command or run id, what was observed, the head SHA, and one negative control —
a deliberately broken input or call and the refusal or failure observed. The surface is
**production-like** — the repository's real-process test harness and fixtures, a sandbox
repository, a real browser, a devN stack, staging, or a local stack with real migrations, one that
has the resource the change touches — and each `E2E` line carries a **link** to that run,
screenshot, or e2e; human review does not replace user-facing verification, and a green unit suite
is not it. A unit or integration test is a regression lock, never proof of a criterion. The agent
that develops the change proves it this way before the merge, and whatever blocks that proof is
fixed, not skipped (*When no surface reaches the changed path*, below). The implementer's proof
and the tester's proof below are both this proof.

## The rules every phase's evidence follows

- **The implementer proves the change before its phase completes, and writes the `E2E (implementer)` line when the pull request opens.**
  The proof is the one defined above. It goes into `.legion/<issue>/implement.json` as the required `proof`
  array, and into the PR body, because the reviewer and the merger verify facts on GitHub and never
  from a handoff. Nothing checks the handoff's shape when you complete — `handoff_complete` reports
  the pushed commit carrying the file, and the daemon reads nothing in it — so an implement handoff
  without a `proof` array, or with a blank or whitespace-only field in one, reaches the tester as it
  is, and the tester records it as a failure against you.
- **The tester verifies the implementer's proof and adds its own `E2E (tester)` line.** It re-runs
  the implementer's command or drives the same surface independently, and records the verdict in
  `.legion/<issue>/test.json` as `implementerProof` (`{verdict, how}`). The tester runs the
  application, not CI again. Try to break the changed surface — wrong and boundary inputs, the
  second and the concurrent run, the party it must refuse — judge whether the implementer's tests
  would catch the ways it breaks, writing the red test for one that none would, and
  read CI on GitHub at the head you are judging for everything static: lint, types and the unit
  and integration suites are CI's, read there and recorded for no later step, and no CI lane is
  rerun as evidence. A lane GitHub shows `queued` or `in progress` is read as such: the tester
  never waits for it, since a required red that settles sends the work back from `testing` and
  the reviewer reads the head's checks before it approves.
  A test handoff whose predecessor carried no proof is a test failure, not a gap for the tester to fill:
  record it in `failures` with `implementerProof.verdict: "rejected"`, complete the phase with
  `verdict: "fail"`, and the daemon returns the issue to the implementer — the agent that developed the change owns
  proving it (a rejected verdict, or `failed > 0`, always comes with the failure recorded in
  `failures`). Otherwise, add your own proof before completing — a proof as defined
  above — as the `E2E (tester)` line and the `proof` array a test handoff that reports no failure
  carries. A code path whose first execution is after merge — a
  deploy workflow's inline step, a post-merge helper, a production-only resource — is untested
  until the implementer has executed it against a devN stack; if no surface can reach it, the
  tester names that missing surface as the blocker instead of passing the phase. Environment or
  secret-scrub evidence (e.g. "`LEGION_*`/`DISPATCH_*`/`ENVOY_*` unset") is recorded once, in
  `.legion/<issue>/test.json`, and only when the issue's acceptance criteria call for it — never
  re-pasted into the PR body each round. After a forward merge, compute the
  fingerprint (*The unchanged-diff check* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`) at the head your `E2E` line
  names and at the new head. Equal: re-run only the
  bare gates — the repository's CI green at the new head and its smoke check — and change the
  `E2E` line's head to the new SHA with
  `forward-merge re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only`. The
  real-surface verification is not repeated. Different: a full test round.
- **The implementer runs `skill://ce-simplify-code` once per pull request, in its first
  implementing round, after its own proof and before it completes that round, when the diff
  touches runtime code; a docs-only diff gets none.** The daemon starts no implementing round
  between a clean review and the approval, so this is the one slot before the tester and the
  reviewer read the head; a later round answering a review runs no second pass. It is scoped to
  the pull request's own diff at that head: nothing applied leaves the head as it is; applied →
  the implementer re-runs the repository's checks and its own proof on the applied head for the
  surface the simplify diff touched, since a refactor that "preserves behaviour" is a claim until
  it is executed, and updates the `E2E (implementer)` line to that head before it completes.
  That cost is why 0-applied is the expected outcome and a pass that applies is spent sparingly.
  The reviewer's pair runs at the head each round reviews. Record both in the `Thermo` line.
- **No deferrals** is the body's rule (*PR body, review, and the merge gate* in
  `skill://legion-worker`): a wording nit, and any clear fix in a file this pull request touches,
  is made in the running round, because a later round costs a CI pass and a re-read of the whole
  change (the operator's answer on that case:
  dispatch://INFRA-49/ask/2d080aec-ce9a-4c7c-a235-ae998d2b0830). The `Fast-follow:` line reads
  `none`, or names only cleanup in files this pull request does not touch.
  A base frozen for others to stack on is never rewritten (*Rewriting pushed commits* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`); the `Chain` line records it.
- **GitHub refuses a pull request body over 65,536 characters.** Each verification round links its
  evidence (the run, the comment) rather than inlining it once the body passes about 48,000
  characters; the `Production` line's record always links.

## When no surface reaches the changed path

No surface reaches the changed path is a report to the architect, never a reason to complete the phase.
Say which surface is missing and what it would have to do — a rig that can spawn the role, a
sandbox that holds the resource, a credential, a command that does not exist yet — and send it to
the architect with `envoy_publish` to its role topic. The architect creates a child issue in this
tree to build it (infrastructure, tooling, or a skill) and resumes you once it lands. A code path
whose first execution would be after the merge — a deploy
workflow's inline step, a post-merge helper, a production-only resource — is untested until you
have executed it somewhere production-like; completing with a unit-test-only handoff is the
failure this rule exists to stop.
