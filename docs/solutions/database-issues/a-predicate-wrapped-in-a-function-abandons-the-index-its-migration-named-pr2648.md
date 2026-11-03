---
title: "A predicate wrapped in a function abandons the index its migration was written for, and an Index Scan is not proof it did not"
category: database-issues
tags:
  - postgres
  - explain
  - migrations
  - index
  - daemon-go
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2648"
---

# A predicate wrapped in a function abandons the index its migration was written for, and an Index Scan is not proof it did not

Extends docs/solutions/database-issues/a-read-reaches-an-expression-index-only-through-its-exact-expression-and-keeps-the-old-predicate-beside-it-pr2467.md,
which says a read reaches an expression index only through that index's exact expression. This
one is the other direction: changing a read's predicate takes an index away from it, and the plan
can still say `Index Scan`.

- **Before changing a read's predicate — `col = $1` to `lower(col) = lower($1)`, a cast, a
  `coalesce` — grep the migrations for an index on that column.** An index's own comment usually
  names the read it was created for and the growth it was meant to bound; that comment is the
  argument you are about to overturn.
- **Ship the replacement index in the same change**, on the expression the read now writes.
- **Measure with `EXPLAIN (ANALYZE, BUFFERS)` on a table big enough that a sequential scan is not
  the cheapest plan, after `ANALYZE`.** Read the `Index Cond`, not the node's name: a predicate
  the index cannot bound becomes a `Filter`, the surviving condition still bounds the scan to the
  index, and the plan says `Index Scan` while the read walks the whole index. Buffer counts show
  it; the node's name hides it.

## Evidence

In sjawhar/legion#2648 the daemon's three repository-keyed pull-request reads became
case-insensitive, because a configured repository rename names one repository under two
spellings. Migration `0062_live_issue_reads.up.sql` had created `pull_requests_repo_number` for
two of exactly those reads, "so its cost does not grow with the records of finished issues";
`lower(repo)` is not that index's expression, and the change abandoned it for four review rounds.
`PullRequestRecorded` runs outside any transaction on every review and every checks settlement of
every mapped repository, and twice on a miss when a pair is configured.

Measured on a scratch database of the daemon's own migrations with 20 000 pull requests over 40
repositories, `ANALYZE`d, with the queries exactly as `record/store.go` spells them. Before, each
read was an `Index Scan` whose only `Index Cond` was the number or the branch, with `lower(repo)`
as a `Filter`; after `20261102095707_pull_request_repo_case_reads.up.sql` added
`(lower(repo), number)` and `(lower(repo), branch)`, both columns are index conditions. Shared
buffer hits for the scan node, before to after: 277 → 4 for the branch read, 119 → 3 for the
number read, 112 → 3 for the recorded check.

The review's own wording is the warning in the rule above. It said the reads "now scan the
table", which on PostgreSQL 18 they did not — the surviving condition kept them inside the index.
They walked the whole index instead, so the cost still grew with every recorded pull request,
which was the migration's point. Had the measurement been taken as "does the plan say Index
Scan", the finding would have been dismissed.

The table's uniqueness rule is a separate decision: `0004`'s unique `pull_requests_repo_branch`
stays case-sensitive, because making it case-insensitive changes what the table accepts and can
fail on rows already there.
