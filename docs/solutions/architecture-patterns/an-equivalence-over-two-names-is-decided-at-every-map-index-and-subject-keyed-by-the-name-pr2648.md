---
title: "An equivalence over two names is decided at every map, index and subject keyed by that name"
category: architecture-patterns
tags:
  - identifiers
  - equivalence
  - case-sensitivity
  - subscriptions
  - daemon-go
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2648"
---

# An equivalence over two names is decided at every map, index and subject keyed by that name

Declaring that two identifiers mean one thing — a renamed repository, an alias, two merged
tenants — is not only a change to how they are compared. Every structure keyed by the identifier
now has two possible keys, and each one is a decision somebody makes, deliberately or by accident.

- **Enumerate the structures before writing the comparison.** Every map, index, uniqueness rule,
  subscription subject, directory name and cache key that the identifier keys. The comparison
  sites are the easy half and the ones a grep finds.
- **For each, choose one of three and say which in a comment**: canonicalize to one name, refuse
  the configuration at load, or read both names in a defined order.
- **Compare loosely, key exactly.** A name that reaches a subscription subject, a directory or a
  row key keeps the producer's spelling. An operator's spelling of the pair may name only
  something nothing else names, because the operator's case is not the producer's.
- **Refuse what cannot be keyed, where every pair is visible.** A rule that needs to see all the
  pairs at once belongs in the constructor that holds them all, not at a use site that sees one.

## Evidence

sjawhar/legion#2648 added `repository_renames` to the Legion daemon and carried four defects of
exactly this shape, each found by review rather than by the author, and each at a different kind
of structure:

- **A subscription subject.** `heardRepositories` replaced GitHub's spelling of a heard
  repository with the configured pair's. Envoy publishes under GitHub's spelling and NATS matches
  subjects case-sensitively, so a pair written `to: CyberMonkLabs/legion` subscribed the daemon to
  subjects nothing publishes. The fix adds a partner only when its lowercased key is not already
  heard, so the producer's spelling always wins and the pair's is used only for a name nothing
  else names.
- **A map key.** `OwnerAliases` is keyed by the owner a repository moved away from, so two pairs
  moving one owner's repositories to two different owners wrote that key twice and the last won —
  silently putting one owner's installation token in another owner's gh directory. `NewRenames`
  refuses that configuration, because it is the only place that holds every pair.
- **A uniqueness rule.** `readDeploys` refuses one repository named twice in a project's
  `deploys`, but it reads a project before the renames settle, so a pair's two names with one
  workflow were both accepted and both matched, the second entry's step silently ignored. A
  settle-time check refuses it, which is also what keeps `DeployOf`'s "at most one matches" true.
- **A row key.** `where repo = $1` could not find a pull-request row recorded under the partner's
  spelling — the very row the feature exists to find. The three reads became
  `lower(repo) = lower($1)` with indexes of their own
  (docs/solutions/database-issues/a-predicate-wrapped-in-a-function-abandons-the-index-its-migration-named-pr2648.md),
  and the four copied "try this name, then the partner" loops collapsed onto one allocation-free
  helper, `ghrepo.Under`.

What they have in common is that the comparison helper was correct in all four. The equivalence
was right; the keys were not.
