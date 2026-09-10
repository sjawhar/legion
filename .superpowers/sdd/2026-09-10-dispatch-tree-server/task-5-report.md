# Task 5 report — anchors are marks

## Changes

- Replaced UTF-16 range anchors with `{artifact_id, mark_id, version, quote, orphaned}` and quote-or-browser-mark anchor input validation.
- Reworked anchored ask and comment creation to allocate a row identifier inside the transaction, write or verify the matching tree mark, persist the mark identifier, and evict the live room after a failed post-mark transaction.
- Removed range selection and legacy anchor re-resolution from the HTTP path. Missing quotes and marks now map to `TARGET_NOT_FOUND` (404) and `ANCHOR_MISSING` (409); actioning a deleted suggestion maps to `ANCHOR_ORPHANED` (409) after committing the row's orphaned state.
- Added Proof `marks` map projection for anchored comments and suggestions. Reply creation and comment actions rebuild a root anchor's reply projection; accept, reject, and resolve project their current statuses.
- Review repair: suggestion acceptance and rejection exclude only their acting mark from the in-transaction refresh, so completed actions cannot mark their own anchor orphaned. Projections are keyed by `anchor.mark_id`, and every projection mutation has the same rollback-eviction protection as other live document writes.
- Replaced offset refresh with `FindMark`: version writes refresh covered quotes from live marks and orphan only rows whose mark is gone.
- Re-anchored every open row during `ReplaceText` from its pre-update mark, retaining original mark attributes and choosing duplicate replacement text nearest the prior ProseMirror position.
- Added a one-minute default unrecorded-mark TTL. Settlement tracks first sighting, re-arms for the nearest expiry, removes only unrecorded `dispatchAsk`, `proofComment`, and `proofSuggestion` marks, and leaves all persisted rows—including resolved comments—and other Proof-native marks intact.
- Deleted `text/anchor.go`, `text/anchor_test.go`, and `text/utf16.go`; the three size-cap call sites now use private non-allocating `api.len16`. Removed `docs.API.ApplyReplace`.

## Red and green evidence

Red first:

```text
cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/api/ -run 'TestQuoteAnchor|TestMarkAnchor|TestAnchorInput|TestReplyMayNot|TestAnchorQuoteNotFound' -v
FAIL: quote anchors returned no mark_id; mark_id was an unknown JSON field; missing browser marks returned 400; the range input was accepted; anchored replies were accepted. Five of six top-level contract tests failed.

cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/docs/ -run 'TestVersionWriteRefreshesAnchorsByMark|TestSettleSweepsUnrecordedMarksAfterTTL|TestReplaceTextReanchorsOpenRows' -v
FAIL [build]: Deps.UnrecordedMarkTTL was undefined.

cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/docs/ -run '^TestSettleSweepsUnrecordedMarksAfterTTL$' -v -count=1
FAIL: a resolved comment's recorded proofComment mark was swept after the TTL.

cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/api/ -run 'TestMarkAnchorVerifiesBrowserMark|TestCommentsSuggestionsAndArtifactFilter|TestSuggestionAcceptAppliesLiveDocument|TestCommentProjectionFailureEvictsLiveDocument' -v -count=1
FAIL: browser-mark projection was absent at mark id `m-1`; successful accept and reject persisted `orphaned: true`; reply-root and resolve projection failures left the websocket room live.
```

Green:

```text
cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/api/ -run 'TestQuoteAnchor|TestMarkAnchor|TestAnchorInput|TestReplyMayNot|TestAnchorQuoteNotFound' -v
PASS: 6/6 top-level anchor contracts (including 4 XOR validation cases) in 2.981s.

cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/docs/ -run 'TestVersionWriteRefreshesAnchorsByMark|TestSettleSweepsUnrecordedMarksAfterTTL|TestReplaceTextReanchorsOpenRows|TestTransactionalApplyRefreshesAnchoredComment|TestRefreshAnchorsClosesRowsBeforeUpdating' -v
PASS: 5/5 document anchor contracts in 1.811s.

cd packages/envoy && gofmt -l ./internal/dispatch && go vet ./internal/dispatch/...
PASS: no output.

cd packages/envoy && PROOF_EDITOR_DIST=/home/ubuntu/tmp/proof-editor-dist DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/... -count=1 -timeout=180s
PASS: 12 tested packages green; model has no test files; 49.90s wall time.

cd packages/dispatch && DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_E2E_PORT=8795 bun run e2e -- margin.e2e.ts
PASS: 8/8 Playwright scenarios across Chromium and iPhone in 21.9s.

cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/api/ -run 'TestMarkAnchorVerifiesBrowserMark|TestCommentsSuggestionsAndArtifactFilter|TestSuggestionAcceptAppliesLiveDocument|TestCommentProjectionFailureEvictsLiveDocument' -v -count=1
PASS: 4/4 review-regression contracts (with two projection-eviction subtests) in 2.598s.

cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/api/ -count=1
PASS: API package in 48.133s.

cd packages/envoy && DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/docs/ -count=1
PASS: docs package in 20.534s.

cd packages/envoy && PROOF_EDITOR_DIST=/home/ubuntu/tmp/proof-editor-dist DISPATCH_TEST_DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable go test ./internal/dispatch/... -count=1 -timeout=180s
PASS: 12 tested packages green; model has no test files; 55.14s wall time.

cd packages/dispatch && DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_E2E_PORT=8795 bun run e2e -- margin.e2e.ts --project=chromium
PASS: 4/4 Chromium scenarios in 9.0s.

cd packages/dispatch && DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55466/dispatch?sslmode=disable DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_E2E_PORT=8795 bun run e2e -- margin.e2e.ts --project=iphone
PASS: 4/4 iPhone scenarios in 9.8s.
```

## Self-review

- Tree reads occur before a Yjs transaction; `Update`, `MarkRange`, and `Unmark` run inside their one transaction. `ReplaceText` gathers open anchor rows before `Server.Apply`, reads the old mark positions before mutation, and writes re-anchored marks within the update transaction.
- `refreshAnchors` closes each SQL row set before issuing its update. It uses mark identity and leaves anchor versions as the version that originally created the anchor.
- The sweep calculates records across all anchored asks and comments, so visible resolved comments do not become dangling marks. It only considers the three Dispatch-owned mark types, then re-arms the settle timer at the earliest unexpired mark.
- Anchor operations preserve issue-first lock ordering. The artifact lock is `FOR KEY SHARE`: it protects the artifact key without blocking the foreign-key check of an already-running document update, avoiding the observed advisory-lock/document-update deadlock.
- Comments project their stored record under `anchor.mark_id`, so browser-owned mark identifiers resolve the matching Proof record. The action-specific refresh exclusion applies only while accept/reject's SQL transaction is unfinished; a later version refresh sees its now-resolved row normally.

## Concerns

- The prescribed port 8794 was occupied by another agent's Dispatch server, so its exact invocation stopped before Playwright. The same suite was exercised against this tree on free port 8795.
- The combined Chromium+iPhone Playwright command remains flaky: iPhone's first scenario twice timed out while Playwright retried a fixed selection toolbar at `top: 997px`, outside the 664px mobile viewport. Each project run independently is green (4/4 Chromium, 4/4 iPhone); the issue is in the pre-Task-7 rendered-preview selection toolbar, not the Task 5 Go surface. Its trace is at `packages/dispatch/e2e/test-results/margin.e2e.ts-margin-creat-bec75-erves-anchored-review-items-iphone/trace.zip`.
- This Task 5 tree predates Task 7's sibling SPA commit, so the local `margin.e2e.ts` run has no `typeof comment.anchor.mark_id === 'string'` assertion. The server-side anchor contract verifies that response field; the Task 7 assertion must run when that sibling is integrated.

## Hardening ledger

No shortcuts or weakened/skipped tests.
