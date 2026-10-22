---
title: "Lint the handoffs you inherit before your first code push: a redaction commit answers another role's private name"
category: legion
tags:
  - check-private-names
  - handoff
  - public-repository
  - skip-checks
  - implementer
date: 2026-10-21
status: active
module: .github/scripts
related_issues:
  - "LEGION-791"
  - "sjawhar/legion#2096"
---

# Lint the handoffs you inherit before your first code push: a redaction commit answers another role's private name

Extends docs/solutions/legion/the-private-names-check-reads-each-events-frozen-body-fix-the-text-then-push-and-lint-a-handoff-before-its-trailered-commit-LEGION-781.md.

That note says every role lints its own handoff before the trailered commit, and that a
handoff-only push starts no check. The delta is the handoff you did not write:

- Before your first code push, run `.github/scripts/check-private-names.sh` over the whole tree,
  or at least over `.legion/<issue>/`: an earlier role's handoff carrying the deployment's project
  key fails the required lint job on *your* commit, since its own push ran nothing.
- The remedy is a redaction commit of your own over that file — replace the key with a pointer to
  the Dispatch document that holds the full text ("the second project's controller, named in the
  plan document") — never a rewrite of the other role's commit. Say so in your handoff and the pull
  request; the Dispatch document stays the other role's record.
- Everything you write afterwards inherits the same rule: a role topic that embeds another
  project's key, a production-check instruction naming that project's controller, belongs in the
  Dispatch plan or a Dispatch message, and reaches the committed handoff and the pull request body
  only as "the other deployment's controller".

## Evidence

sjawhar/legion#2096 (LEGION-791): the planner's committed handoff named the deployment repository's
Dispatch project key six times (a controller role topic and the production-check notes), pushed
with `skip-checks: true`, so no lint ran. The implementer's first local run of
`check-private-names.sh` over the repository reported the six lines before the first code push;
one commit redacted them in place and the implementer's own handoff and body were written without
the key. The whole-repository check passed at every later head.
