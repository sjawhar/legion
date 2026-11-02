---
title: "A fake of a GraphQL API answers the selection set and the mutation's own text, never the variables map"
category: testing
tags:
  - graphql
  - fakes
  - mutation-testing
  - regression-lock
date: 2026-10-30
status: active
module: packages/daemon/internal/ghautomerge
related_issues:
  - "sjawhar/legion#2562"
---

# A fake of a GraphQL API answers the selection set and the mutation's own text, never the variables map

Extends docs/solutions/github-api/a-branch-exists-only-by-its-exact-ref-read-commits-resolves-any-commit-ish-and-a-fake-never-echoes-the-name-INFRA-391.md.

That rule — a fake never answers a name it was not seeded with by echoing it back — holds for
REST. GraphQL gives a fake two more ways to be generous than the client is, one on each side of
the call, and a fake that takes either one stays green while the wire carries nothing.

## The rules

- **A variable the mutation's text does not reference is ignored by the server.** A fake that
  records what the variables map holds therefore passes a mutation whose text dropped the field.
  Record what the *request text* passes: look the field up in the query string, and record a field
  the text does not name as absent, not as the map's value. Then deleting the field from the
  mutation fails the test with the field recorded absent.
- **Answer a field only while the selection set asks for it, as the server does.** A fake that
  always writes a field into its answer passes a read whose query stopped selecting it, and the
  production code then builds its request from a zero value.
- **Both halves in the same change.** Building the discipline on the half you are editing and
  leaving the other half generous is the shape this defect takes: the half you did not touch is
  the one no test can see.
- **When a mutant survives, measure what it does on the real surface before you grade it.** A
  surviving mutant whose effect is argued is a judgement; one whose effect is driven is a fact,
  and the fact is what tells a cosmetic gap from a defect that would reach production green.

## Evidence

In `packages/daemon/internal/ghautomerge/ghautomerge_test.go` the mutation side was written this
way from the start: `passed(field, variable)` records `<absent>` unless the request's query
contains `<field>: $<variable>`. Dropping `commitBody: $body` from the mutation text failed three
cases with `body="<absent>"`, and dropping the `headline` entry from the variables map failed with
`headline=%!q(<nil>)`.

The read side was not, and the gap was invisible: deleting `title` from the read query's
pull-request selection left the whole package green, because the fake answered a title the query
no longer asked for. Its effect was then measured rather than argued — with no `title` in the
read, the production code built the headline `" (#<n>)"` from an empty title, and GitHub accepted
it and wrote exactly that as the squash subject of scratch pull request #2570, merge commit
`3d84aee8c012`, whose whole commit message is `" (#2570)"`. So one line of a selection set stood
between the default branch and a commit subject like that, with CI green.

The lock is commit `9965a1e792fb`, "test(daemon): the ghautomerge fake answers a title only while
the read selects it": the fake answers `title` only while the query asks for it. The shipped code
stays green and the same mutant now fails five submission rows with `headline=" (#42)"`.
