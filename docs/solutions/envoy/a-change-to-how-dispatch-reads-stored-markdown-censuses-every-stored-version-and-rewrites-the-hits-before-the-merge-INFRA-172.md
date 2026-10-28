---
title: "A change to how Dispatch reads stored markdown censuses every stored version and rewrites the hits before the merge"
category: envoy
tags:
  - dispatch
  - pmdoc
  - census
  - stored-documents
  - migration
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "INFRA-172"
  - "sjawhar/legion#2272"
---

# A change to how Dispatch reads stored markdown censuses every stored version and rewrites the hits before the merge

- Before a change to `pmdoc.Parse` (or any reader of stored markdown) merges, count the stored
  documents whose reading changes: the latest version of every document, every approved version,
  and every past version. A document the new reader refuses or reads otherwise is unreadable, or
  shows different content, the moment the deploy lands.
- Run the count through Dispatch's API when no database credential is at hand:
  `GET /api/v1/artifacts/{id}/versions/{n}` returns the stored markdown (`/text` renders the live
  tree again and is not the stored text). List each project's issues with one unpaged
  `GET /api/v1/issues?project=<KEY>`, since offset paging repeats and skips issues while others
  write. Retry a network failure or a 5xx, send one request at a time, write the latest markdown to
  a corpus file under `$TMPDIR`, and never commit that corpus.
- Run `pmdoc/gen/differential.sh --base <base> --head <head> --corpus <corpus.jsonl>` over that
  corpus; its gates must be zero, and each document it flags must be one the count found.
- Before the merge, rewrite each hit's latest version with a `dispatch doc-edit` guarded by the
  document token, in a form both the old and the new reader read the same and that keeps what the
  author meant; comment on the owning issue. A hit that is only a past version, never approved,
  needs no edit. Stop and ask when an approved version is hit, since the edit makes the approval
  stale.
- Run the count again after the deploy, for what the old build stored in between.

## Evidence

- The PR made a `|` after an even run of backslashes end a table cell, as the browser editor
  reads it, and widened the too-wide-row refusal (`packages/envoy/internal/dispatch/pmdoc/table.go:603`,
  `insert_test.go:140`). Before the rewrite, `pmdoc.Parse` at the head refused one live document
  ("a table row holding 5 cells where its table has 4").
- The census read 26,619 versions across 11 projects. 8 latest versions held the changed shape and
  no approved version did; all 8 were rewritten before the merge (the PR body's
  `## Contract change census`). The differential over 2,050 stored documents reported 0
  regressions, misreads and panics, and flagged exactly the 4 documents the census had found among
  them.
- A first census run lost 689 requests to an outage and repeated issues through offset paging;
  it was discarded.
