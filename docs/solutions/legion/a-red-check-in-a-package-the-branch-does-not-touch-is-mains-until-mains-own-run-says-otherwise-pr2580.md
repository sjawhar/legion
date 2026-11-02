---
title: "A red check in a package the branch does not touch is main's until main's own run says otherwise, and a re-run can never clear it"
category: legion
tags:
  - ci
  - forward-merge
  - merge-ref
  - flaky-test
date: 2026-10-31
status: active
module: skills/legion-worker
supersedes: docs/solutions/legion/a-red-check-in-a-package-the-branch-does-not-touch-looks-for-mains-fix-first-pr2366.md
related_issues:
  - "INFRA-841"
  - "sjawhar/legion#2580"
  - "sjawhar/legion#2598"
---

# A red check in a package the branch does not touch is main's until main's own run says otherwise, and a re-run can never clear it

Corrects step 3 of
`docs/solutions/legion/a-red-check-in-a-package-the-branch-does-not-touch-looks-for-mains-fix-first-pr2366.md`,
the rule about looking for main's fix first. That page ties the forward merge to a conflicting
pull request and sends every other case to "re-run the failed job once instead of pushing code".
For a defect that is on main and not yet fixed, that re-run cannot pass: a `pull_request` run is
built on the merge of your head with the base's tip, and a re-run replays the run's own merge ref,
which still carries the broken base. Only a push makes a merge ref over the base's new tip. The
whole procedure follows, so this page stands on its own.

1. **Confirm the branch does not change the package.**
   `jj diff --from <fork point> --to <head> --stat <package>` prints nothing.
2. **Run the named test at your own head's tree.** A pass there and a red in CI already tells you
   the two trees differ, and the only difference is the base's tip.
3. **Read the base's own push run for that job at the base's tip**, not only its history:
   `gh run list --repo <owner>/<repo> --workflow <ci file> --event push --branch <base> --limit 10 --json databaseId,headSha,conclusion`,
   then the failing job's log. This is the discriminator, and it is the step the older page
   leaves out.
   - **Green on the base:** the red is yours or a flake. Re-run the failed jobs once.
   - **Red on the base:** the cause is the base's. Change nothing and push nothing — a push onto
     a red head spends a fix attempt on a defect that is not yours. Never re-run the run: it
     replays its own merge ref and fails again.
4. **Let the fix land on the base in its own pull request**, routed by whoever coordinates the
   work, and say so where the work is reported rather than filing it yourself.
5. **Clear the red with one forward-merge push** once that fix has merged, even where the pull
   request never conflicted: the merge is what makes a merge ref over the base's new tip. Re-run
   the change's own proof at the merge head, and record the fingerprint pair.
6. **Write the reading down on the pull request**: which job and which test, that the branch does
   not touch that package, the base run that proves the cause is the base's, the fix's pull
   request and merge commit, and, in those words, that the old run must not be re-run.

## Evidence

sjawhar/legion#2580, INFRA-841. `envoy-go` and the `ci-ok` aggregate were red at head
`c10800814dec`, run 41165294249, on `TestTheDocumentedClaimsAreTheClaimsAnExchangedTokenCarries`
in `packages/envoy/internal/broker/oauth` — a package that pull request does not touch. The same
test passed at the branch's own tree, before and after the merge
(`go test ./internal/broker/oauth/ -run … -v` → `--- PASS (0.00s)`), and main's own push run
41164510541 at `d7e1daaaaf` failed it too. That pair is what named the cause.

The cause: the check reads the claim list under
`#### The Dispatch identity the broker can issue` in `docs/kubernetes.md`, and the loop in
`documentedClaims` (`packages/envoy/internal/broker/oauth/claims_doc_test.go`), **as it stood at
`c10800814dec`**, stopped at the first line that was neither a bullet nor an indented
continuation once it held one claim: its comment read "the list runs to the first line that is
neither a bullet nor a wrapped bullet's indented continuation — in the document as written, the
blank line under it". A relay on main had reflowed that list so a blank line sits between each
bullet, so the parse read `session` alone against the
`person, project, project_scope, session` the exchange issues. sjawhar/legion#2598 widened the
parser and merged as `1a9095e6cf22`: the head's reader is `claimListNames`, and a blank line now
ends the list only when the next non-blank line is neither a bullet nor an indented continuation,
so `documentedClaims` at the head behaves the opposite way to the sentence above and is no
citation for it. The branch was held until then, and the round that cleared
the red changed no code at all: one merge commit, `2599248d44b3`, no conflict, fingerprint
`756fb89b…` unchanged at both heads, pushed once with the round's handoff, which that release
still committed, so the push carried a code commit and GitHub started a run for it.

Two things the episode measured that a reader should not re-derive. Tightening the document back
was not available: the repository's own layout checker refuses the tightened list, so the parser
was the only side that could move. And the handoff-only push that release had would not have
cleared the red either: it carried `skip-checks: true` and started no run, so it made no merge
ref — the round's push had to carry the merge. Since sjawhar/legion#2354 a handoff is a versioned
record on its Dispatch issue, so there is no handoff-only push and no trailer: every push a round
makes is a code push and starts a run.
