---
title: "A pod name copied from Dispatch into a pull request carries the deployment's project key and fails the private-names check"
category: legion
tags:
  - public-repository
  - check-private-names
  - check-pr-text
  - pr-body
  - operator-output
date: 2026-10-13
status: active
module: .github/scripts
related_issues:
  - "LEGION-164"
  - "sjawhar/legion#1932"
---

# A pod name copied from Dispatch into a pull request carries the deployment's project key and fails the private-names check

Extends docs/solutions/legion/a-name-a-package-stops-shipping-is-swept-in-every-text-the-remaining-package-emits-LEGION-247.md.

- `.github/scripts/check-pr-text.sh` runs `check-private-names.sh` over the pull request's title,
  body, branch name, commits, comments, reviews and review comments, in the required `PR Title`
  job, on every push and every edit. Operator output pasted from Dispatch — a pod, Sandbox,
  namespace or daemon name of the deployment — carries the private project key inside it
  (`legion-<project>-<issue-key>-…`) and fails it; so do a gateway URL, a kubeconfig context, a
  cluster name.
- Before pasting, rewrite each such name to what it is ("the oldest live issue pod, created …",
  "the production daemon pod") or to the rig's own neutral key (a stage proof's `s4a-…` names are
  the rig's and pass). Keep the inputs — contexts, URLs, audiences, ports — on the Dispatch
  comment and link it; quote only the `CHECK` block and the verdict on the pull request.
- The check also refuses a comment it did not write: an edit leaves the old text in the comment's
  history, so a flagged comment is deleted, never edited.

## Evidence

LEGION-164's `Production:` line quoted the release-speed lane's Dispatch answer naming the oldest
live issue pod by its `legion-<project>-<project>-1178` name; `PR Title` run 38332017108 failed
with `body:37: names the private deployment repository or its Dispatch project key`. Rewritten to
"the oldest created 2026-10-12T12:56:35Z", the next run (38332777795) passed. The stage 4a
operator's own naming rule for its result — inputs in the Dispatch comment, the fenced block and
two verdict lines on the pull request — kept the later edits green.
