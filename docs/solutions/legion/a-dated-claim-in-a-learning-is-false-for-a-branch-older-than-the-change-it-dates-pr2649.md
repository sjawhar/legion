---
title: "A dated claim in a learning is false for a branch older than the change it dates: read the tree, not the note"
category: legion
tags:
  - docs-solutions
  - skip-checks
  - handoff
  - migration
  - push-shape
date: 2026-11-02
status: active
module: docs/solutions
related_issues:
  - "sjawhar/legion#2649"
---

# A dated claim in a learning is false for a branch older than the change it dates: read the tree, not the note

Extends docs/solutions/legion/the-trailer-on-a-pushs-tip-suppresses-the-runs-of-every-code-commit-beneath-it-pr2542.md.
That page's rule — the tip's message decides whether a push starts a workflow — stands. What does
not carry over is the sentence beside it that dates the hazard away.

- **A learning that says "since \<change\>, nothing does X any more" describes main after that
  change, and your branch may be older than it.** A long-lived branch keeps the shape it was cut
  from until the migration's own commit reaches it, and until then every hazard the note retired
  is live on your head.
- **Check the tree, which costs one command, rather than the note's tense.** Before a push, read
  what you are actually pushing: `jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T description`
  for the tip's message, and `jj -R "$LEGION_WORKSPACE" diff --from <branch>@origin --to @-
  --name-only` for the paths. A rule keyed on paths that no longer exist on main can still be the
  rule your branch is under.
- **A role prompt's own step can be the thing that is out of date.** Following "a push that
  touches only the handoff directory ends its head with `skip-checks: true`" is what suppressed
  this branch's CI, on a push that also carried code. When a step's precondition has been migrated
  away, the step is not a reason to do it; the push's own paths are.
- **Write the migration's window into the learning, not only its end state.** "No branch carries
  these files after \<change\> lands on it" is the honest form; "no branch carries these files" is
  the form that costs a reader a round.

## Evidence (sjawhar/legion#2649, INFRA-879)

Two active pages state the end state of sjawhar/legion#2354, which moved every handoff from a
committed file to a record on the Dispatch issue:
`docs/solutions/legion/the-trailer-on-a-pushs-tip-suppresses-the-runs-of-every-code-commit-beneath-it-pr2542.md`
("Since then a handoff is a record on the Dispatch issue, no role pushes a handoff commit, and no
push carries the trailer") and
`docs/solutions/legion/the-private-names-check-reads-each-events-frozen-body-fix-the-text-then-push-and-lint-a-handoff-before-its-trailered-commit-LEGION-781.md`
("not a file in the repository, so the check does not read it").

This issue was admitted before that migration reached its branch, and all three retired
consequences landed on it:

- its handoffs were committed files under `.legion/INFRA-879/` until the import commit
  `32face5f931280681d08706c2a7bbcc5975f0f58`, `chore(legion): INFRA-879's handoffs are on its
  Dispatch issue now`, removed them;
- a push carrying two code commits under a handoff tip whose message ended `skip-checks: true`
  started no workflow at all, and the branch sat with `check-runs` `0` at that head for two rounds
  until a push with an untrailered tip;
- a committed handoff file named the deployment's Tailscale MagicDNS host, which
  `.github/scripts/check-private-names.sh` refuses over the tree and
  `.github/scripts/check-pr-text.sh` refuses in the pull request's body, failing `pr-title` for a
  round before anyone read the log.

The generalisation is not about handoffs. `docs/solutions/` is written by branches and read by
branches, and a page's "since" clause is a fact about main at its date.
