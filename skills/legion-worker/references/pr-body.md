# PR body, proofs, and the simplify pass

Part of `skill://legion-worker`. Read it before you write or edit any line of the pull request
body, put a `proof` array in a handoff, verify another phase's proof, or run the simplify pass.
Every path it cites is in sjawhar/legion.

## The READY format

The implementer writes the PR body in the READY format from the moment the PR opens, and every
later phase keeps it current rather than replacing it:

```
## Verification

**CI:** `Tests` run <run-id> — jobs lint, typecheck, test all success at <head-sha>; `PR Title` run <run-id> — job pr-title success at <head-sha>.

**Threads:** <n> resolved, 0 unresolved. Each disposed individually, never in bulk:
- Thread <id>: fixed in <commit-sha> — <one line>.
- Thread <id>: not a defect — <reason>.
`legion threads resolve --pr <n> --repo <owner>/<repo>` at <head-sha>:
resolved <thread URL> — its opener's acceptance
resolved <thread URL> — the Legion reviewer's acceptance of a bot's thread
left open <thread URL> — newest reply by <login> is not an acceptance
left open <thread URL> — newest reply by <login> is an unsubmitted draft in a pending review
left open <thread URL> — newest reply by <login> is not its opener's or the Legion reviewer's acceptance

**Thermo:** `ce-simplify-code` once at <head-sha>: <0 applied | applied → new head <sha>>; thermonuclear pair at the final head <sha>:
<verdict>. (omitted entirely on a docs-only PR — there is no code for either pass, so neither runs)

**Security:** `Security` run <run-id> at <head-sha>: <n> new findings against the base (report-only until the window closes) or 0; pair: <the review's Security: line>. (omitted with Thermo on a docs-only PR)

**E2E (implementer):** <surface> — ran `<command or run id>`, observed <result>, at head <sha>.
Negative control: <deliberately broken input> → <refusal or failure observed>.

**E2E (tester):** <surface> — ran `<command or run id>`, observed <result>, at head <sha>.
Negative control: <deliberately broken input> → <refusal or failure observed>.
Verified the implementer's proof by <re-running its command | driving the same surface independently>.

**Production:** <what was checked in production, how, what was observed> — merge commit <sha>.
(written by the implementer after the merge lands; `pending <what is missing>` until then)

**Fast-follow:** <one named cleanup item and where it will land>, or "none".

**Chain:** stacked on <base bookmark> frozen at <sha> / not stacked.
```

## What a proof is

**A proof** is the changed behaviour exercised on the surface a user reaches it through, recorded
as the exact command or run id, what was observed, the head SHA, and one negative control —
a deliberately broken input and the refusal or failure observed. The surface is
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
  The proof is the one defined above. It goes into `.legion/implement.json` as the required `proof`
  array (`handoff_write` for phase `implement` refuses a payload without one, or with a blank or
  whitespace-only field, and names the field), and into the PR body, because the reviewer and the
  merger verify facts on GitHub and never from a handoff.
- **The tester verifies the implementer's proof and adds its own `E2E (tester)` line.** It re-runs
  the implementer's command or drives the same surface independently, and records the verdict in
  `.legion/test.json` as `implementerProof` (`{verdict, how}`).
  When the change adds or moves an authorization or refusal boundary, the negative control is the
  unauthorized caller: drive the boundary as the party it must refuse, and record the refusal as
  the observation.
  A test handoff whose predecessor carried no proof is a test failure, not a gap for the tester to fill:
  record it in `failures` with `implementerProof.verdict: "rejected"`, complete the phase, and let
  the architect return the issue to the implementer — the agent that developed the change owns
  proving it (`handoff_write` for phase `test` refuses a rejected verdict, or `failed > 0`,
  with no recorded failure). Otherwise, add your own proof before completing — a proof as defined
  above — as the `E2E (tester)` line and the `proof` array `handoff_write` for
  phase `test` requires whenever you report no failure. A code path whose first execution is after merge — a
  deploy workflow's inline step, a post-merge helper, a production-only resource — is untested
  until the implementer has executed it against a devN stack; if no surface can reach it, the
  tester names that missing surface as the blocker instead of passing the phase. Environment or
  secret-scrub evidence (e.g. "`LEGION_*`/`DISPATCH_*`/`ENVOY_*` unset") is recorded once, in
  `.legion/test.json`, and only when the issue's acceptance criteria call for it — never
  re-pasted into the PR body each round. After a conflict-forced rebase, compute the
  fingerprint (*The unchanged-diff check* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`) at the head your `E2E` line
  names and at the new head. Equal: re-run only the
  bare gates — the repository's CI green at the new head and its smoke check — and change the
  `E2E` line's head to the new SHA with
  `rebase re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only`; the
  real-surface verification is not repeated. Different: a full test round.
- **The implementer runs `skill://ce-simplify-code` once per pull request, after the last review round
  closes and before the reviewer's final pass, when the diff touches runtime code; a docs-only
  diff gets none.** It is scoped to the pull request's own diff, at the head where the last review
  round closed: nothing applied leaves that head final; applied → the applied head is the final
  head: CI runs on it, the pair runs once on it, and the E2E proof re-runs on it for the surface
  the simplify diff touched, since a refactor that "preserves behaviour" is a claim until it is
  executed. That cost is
  why 0-applied is the expected outcome and a pass that applies is spent sparingly. At the applied
  head the implementer re-cites the `CI` line and re-runs its own proof into `E2E (implementer)`,
  and the tester re-runs its proof for the touched surface into `E2E (tester)`, before the
  reviewer's final pass. Simplify is the last code change; the pair is the last review. Record it
  in the `Thermo` line.
- **No deferrals** is the body's rule (*PR body, review, and the merge gate* in
  `skill://legion-worker`): the `Fast-follow:` line holds naming, duplication, or wording cleanup
  only. A base frozen for others to stack on is never rewritten (*Rewriting pushed commits* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`); the `Chain` line records it.

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
