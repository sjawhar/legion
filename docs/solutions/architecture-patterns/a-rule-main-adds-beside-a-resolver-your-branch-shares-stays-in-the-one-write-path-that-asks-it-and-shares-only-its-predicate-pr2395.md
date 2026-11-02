---
title: "A rule main adds beside a resolver your branch shares stays in the one write path that asks it, and shares only its predicate"
category: architecture-patterns
tags:
  - forward-merge
  - shared-resolver
  - role-claim
  - write-path
date: 2026-11-01
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "sjawhar/legion#2395"
---

# A rule main adds beside a resolver your branch shares stays in the one write path that asks it, and shares only its predicate

Applies when your branch moves logic into a shared package (a resolver two subsystems call) and a
forward merge brings a new rule main wrote next to the old copy.

- **Place the rule by who asks its question, not by where your branch moved the code.** List the
  callers that must answer the question the rule decides. When only one write path asks it, the
  rule stays in that path.
- **Name the writes the shared resolver may make, and keep that list.** A resolver its callers
  trust to make only one kind of write (here: release a claim whose holder is gone) must not gain
  a second kind (take it from a live holder) because a merge put the code beside it.
- **Share the predicate, not the decision.** Point main's new code at the shared package's
  primitives (a liveness check, a match function) so there is one definition of each, and keep
  the decision that uses them where its only caller is.
- **This is not a file move.** When the branch moved an abstraction and not a file, porting
  main's hunks to "the new home" puts the rule in the wrong place; port only the symbols it
  references.

## Evidence

- Merge `1b15f353` of main's #2592 (`151d2d7996`, a soft claim takes a role from its predecessor
  only within one client instance) into this pull request's move of role-holder resolution into
  `internal/roles`. Resolution and reasons: the diff-check comment
  https://github.com/sjawhar/legion/pull/2395#issuecomment-6546429898.
- The rule stays in the claim route: `packages/envoy/cmd/listener/role_holder.go:260`
  (`roleSupersessionVerdict`, its liveness read on `roles.Registered`), whose only caller is
  `cmd/listener/api.go:1209` (`POST /v1/roles/set`).
- The shared resolver is unchanged: `internal/roles/roles.go:124` says the release of a gone
  holder is the one write it interprets. Its callers are the listener's role lane
  (`cmd/listener/role_holder.go:352`) and the outbox's role forward
  (`internal/dispatch/outbox/role.go:89`); neither may take a role from a live holder.
- #2592's tests passed unmodified: the merged head's diff from main hashed the same, per test
  file, as this pull request's own diff before the merge.
- Nearest documents:
  `docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md`
  ports main's edits to moved files, which here would have pushed the rule into the resolver;
  `docs/solutions/architecture-patterns/read-display-only-data-in-the-handler-that-shows-it-not-in-a-query-helper-other-callers-share-INFRA-569.md`
  weighs callers on cost, not on which write they may make.
