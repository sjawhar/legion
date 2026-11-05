# Requirement conversation sources

From the repository root:

```bash
PGOPTIONS='-c default_transaction_read_only=on' psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -f packages/envoy/internal/dispatch/provenanceaudit/requirement_sources.sql
```

Read-only: two selects, no temporary tables. On production, run it with the pre-deploy census's credentials or against a read replica. Wrap the run in `BEGIN READ ONLY; SET LOCAL statement_timeout = '60s';` and `ROLLBACK;` for one consistent transaction. Use `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY` when both results must share a snapshot.

## Output contract

psql prints two CSV result sets. The first has one row, with these columns in order:

1. `scope_issues`: issues whose primary document has a current requirement block.
2. `requirement_blocks`: current requirement blocks in those primary documents.
3. `cited_requirement_blocks`: requirements citing at least one conversation.
4. `cited_document_ask_only`: requirements citing only document ask blocks.
5. `cited_other_only`: requirements citing only comments, messages or item asks.
6. `cited_both`: requirements citing both categories.
7. `person_document_ask_answers`: answered document asks whose latest `ask.answered` event is by a user.
8. `person_item_ask_answers`: the same count for asks without `block_artifact_id`.
9. `person_comments`: user comments on scoped issues or their documents.
10. `person_messages`: user messages on scoped issues.
11. `item24_share_cited`: `cited_other_only / cited_requirement_blocks`.
12. `item24_share_volume`: `(person_item_ask_answers + person_comments + person_messages)` divided by all four person counts.

Shares are rounded to three decimal places and are null when the denominator is zero. Requirement text is its latest non-null indexed text at or below `document_block_versions.version`, including children. An owed or missing index version contributes a requirement to scope but provides no text to classify.

A `dispatch://` URL containing `/ask/`, `/comment/` or `/message/` is an other-conversation citation. A document block URL (`/spec#b-<id>` or `/artifact/<slug>#b-<id>`) is a document-ask citation only if its node is a current `document_blocks` row of type `ask`. Block ids are taken verbatim. Other URLs do not count. Repeated URLs count once per sample; repeated citations do not increase requirement counts.

The second result has columns `node_id, class, urls`: up to ten cited requirements ordered by `md5(node_id)`. Classes are `document_ask_only`, `other_only` and `both`. `urls` lists the distinct conversation URLs in sorted order, separated by `; `. It contains no block text.

## Item 24 decision

Build item 24 when either share is at least **0.25**. Use `item24_share_cited` only when `cited_requirement_blocks >= 20`. Compare the underlying counts at the boundary, not the rounded display. Below both thresholds, defer item 24 to a version 5 schema bump.

Hand-check the ten samples with `dispatch read --ref dispatch://<node>`, adding `b-` after `#` in the stored node id. Verify each cited document target is an ask block. Record `agreement n/10`; below 8/10, report the classification failure and do not apply the threshold. If fewer than ten cited requirements exist, record the actual sample count and do not claim ten were checked.

The cited share can miss uncited decisions; volume includes comments that are not decisions. These are proxies, not a census of decisions.

## Test

Set `DISPATCH_TEST_DATABASE_URL` to the local test Postgres connection string, then run from `packages/envoy`:

```bash
go test ./internal/dispatch/provenanceaudit -count=1
```

The test runs the embedded SQL in a read-only transaction over an isolated migrated database. It checks exact counts, URL classes and negative controls for agents, out-of-scope issues, earlier answers and text versions outside the indexed version.
