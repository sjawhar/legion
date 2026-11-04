---
title: "Dropping an index an audit calls unused changes statements you did not touch"
category: database-issues
tags:
  - postgres
  - index
  - migrations
  - performance
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2589"
---

# Dropping an index an audit calls unused changes statements you did not touch

An index audit counts scans, not statements.
A statement whose text your change never edits can still be slower once the index it could have served is gone.

- Before dropping an index an audit calls unused, name every statement whose predicate that index could serve,
by reading the queries rather than the audit's counters.

- Measure each of those statements before and after the drop, on a database at each schema, since no one database holds both sides.

- Publish the reading that got worse beside the reason the drop still stands,
and say in the statement's own comment which index the surviving read now uses and why its predicate is written the way it is.

## Evidence

In sjawhar/legion#2589 migration `20261101182440` drops `asks_open_artifact`, the partial index on open asks by artifact, which the index audit "found … unused for nine days".
The read it could have served survives unchanged:
`listArtifactAsksQueryOpen` keeps its state as a literal in the text
(`packages/envoy/internal/dispatch/api/asks.go:975-976`),
because "State and ownership each have a fixed query, with the state written in as a literal, so that the index a state carries stays eligible under a generic plan too …
A document's asks read asks_artifact_id (0032) and filter the state, whatever state is asked for"
(`packages/envoy/internal/dispatch/api/asks.go:957-965`).

The reading that got worse is published in the pull request body's protocol 2 table:
`listArtifactAsksQueryOpen` 1.636 ms before and 2.676 ms after, the one reading higher after of the three measured one side each,
beside `listIssueAsksQueryAll` 7.086 ms → 1.101 ms and `listIssueRootMessagesQuery` 4.014 ms → 214 µs.
The body states both the cause and why the drop stands:
"`listArtifactAsksQueryOpen` is the one reading that is higher after, and it is **not a changed statement**: this branch edits its comment and not its text.
Its rise is the drop of `asks_open_artifact`, which R7 asks for once the query is shown to use `asks_artifact_id`, and the index audit found that index unused for nine days."
