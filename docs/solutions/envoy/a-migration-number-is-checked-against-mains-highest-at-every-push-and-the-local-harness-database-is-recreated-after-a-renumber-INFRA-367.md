---
title: "A migration number is checked against main's highest at every push, and the local harness database is recreated after a renumber"
category: envoy
tags:
  - dispatch
  - migrations
  - pgmigrate
  - harness
date: 2026-10-27
status: superseded by sjawhar/legion#2411 — a Dispatch or daemon migration added after the numbered series is named for the UTC moment it was created (`YYYYMMDDHHMMSS_<slug>.up.sql`), so two open pull requests no longer take one number and nothing is renumbered; every numbered file keeps the number it landed with, and no number is added again (`packages/envoy/cmd/dispatch/README.md`, `packages/daemon/internal/store/migrations/embed.go`)
module: packages/envoy/internal/dispatch/store/migrations
related_issues:
  - "INFRA-367"
  - "sjawhar/legion#2389"
---

# A migration number is checked against main's highest at every push, and the local harness database is recreated after a renumber

Extends docs/solutions/envoy/two-pull-requests-take-the-same-migration-number.md.

- Two files such as `0129_a.up.sql` and `0129_b.up.sql` never conflict in git, so GitHub can call
  the pull request mergeable. The only check that refuses the pair,
  `TestMigrationSetIsNumberedOneToN`, runs only on a tree that holds both files.
- Do not wait for a rebase to compare numbers. Before every push and every review round of a
  branch that adds a migration, fetch and compare main's highest migration number with your
  lowest.
- A numbering agreement with one sibling pull request reserves nothing: an unrelated pull request
  can take the number first.
- After a renumber, recreate the local harness database too: the `bun run dispatch:local`
  database is truncated and seeded at every start but never re-created. It recorded your old
  number for your file, so main's migration at that number never applies there. The symptom is
  the opposite of "column … already exists": a seed or a request that touches main's schema
  answers 500 with `column … does not exist`. Run the proof on a freshly created database.

## Evidence

sjawhar/legion#2389 picked migrations 0129-0133 against the `main` of 2026-10-27 morning and
agreed its numbers with the INFRA-156 pull request alone. The unrelated #2137 then merged
`0129_document_edit_notices`. Git reported no conflict between the two 0129 files (round-2 thread
PRRT_kwDORFy7ds6wpyUC). The merge 87bda901 and the renumber to 0130-0134 (7bbf6b71) fixed it.
After the renumber, the harness's existing `dispatch_local` database had already recorded 129
for the old `0129_comments_written_version`. On it the seed's `POST /api/v1/issues` answered 500:
`column "last_person" of relation "doc_settlements_pending" does not exist`. A freshly created
database applied 129 through 134, and the block-graph driver passed with 67 PASS lines.
