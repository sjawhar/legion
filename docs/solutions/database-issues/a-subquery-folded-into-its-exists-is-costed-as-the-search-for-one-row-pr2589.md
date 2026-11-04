---
title: "A subquery folded into its exists is costed as the search for one row"
category: database-issues
tags:
  - postgres
  - explain
  - index
  - planner
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2589"
---

# A subquery folded into its exists is costed as the search for one row

Extends docs/solutions/database-issues/a-predicate-wrapped-in-a-function-abandons-the-index-its-migration-named-pr2648.md.
That one is about a predicate that takes the index away from a read;
this one is about a predicate the planner keeps but prices for one row.

- When a subquery is folded into the `exists` around it, Postgres costs it as the search for a single row,
and a zero-startup whole-table scan is then the cheapest start.
That scan is paid once per outer row, so the statement's cost is the whole table times the rows that reach it.

- Fence such a subquery with `offset 0` so the planner prices it on its own, for every row it matches,
which is where its index is cheapest.

- Commit a plan test that names the index each fenced arm must reach, with a negative control over the text the change replaced,
and give the fixture enough rows that the planner's choice means something.

## Evidence

In sjawhar/legion#2589 the related-terms probe's document arm reads `document_search` through a subquery fenced with `offset 0`
(`packages/envoy/internal/dispatch/api/related_terms.go:601`),
and the statement's own comment states the cost it avoids:
folded in, "the arm is costed as the search for a single row, for which a scan of the whole table starts cheapest,
and that scan then detoasts one document vector after another until it meets a match — a cost paid again for every lexeme the arms before it leave unanswered,
which is what made a probe of twenty rare words cost seconds"
(`packages/envoy/internal/dispatch/api/related_terms.go:578-586`).

The measured plans are in the pull request body:
unfenced, `Seq Scan on document_search v, actual 383.804 ms, loops=20`;
fenced, "`Bitmap Index Scan on document_search_vector`, 0.069 ms a loop".
The same paragraph records the pair that caught it at "3.85 s against 7.51 s, this release slower than the one before it".

The plan test is `TestEveryDocumentArmSearchesTheDocumentSearchIndex`
(`packages/envoy/internal/dispatch/api/document_search_test.go:220`),
which holds every document arm's plan to an index scan keyed on `document_search_vector` and fails any arm whose plan still tests a tsquery against `artifact_versions`.
Its negative control is `TestMainsDocumentArmSearchesNoDocumentSearchIndex`
(`packages/envoy/internal/dispatch/api/document_search_test.go:238`),
which fails when the replaced text does not show the fault the assertion is for.
The fixture writes 2,000 documents (`packages/envoy/internal/dispatch/api/document_search_test.go:130`) as "the bulk the planner needs, which no read above returns: on a handful of documents every plan costs about the same and the planner's choice says nothing",
and records that at the four hundred it started with "the probe's plan read document_search_pkey and the index assertion below decided nothing about the statement"
(`packages/envoy/internal/dispatch/api/document_search_test.go:114-124`).
