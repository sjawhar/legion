# Task 3 report — document content in the Proof tree

## Delivered

- Added `internal/dispatch/docs/tree.go`, the tree boundary for Dispatch documents:
  - room content is read from the `prosemirror` `Y.XmlFragment` and rendered through `pmdoc`;
  - seeded documents create the server-owned, empty `marks` map;
  - incoming markdown is parsed with anchor marks stripped;
  - malformed input is reported as `ErrInvalidMarkdown`, while a malformed resident tree is reported as `ErrDocSchema`.
- Changed `SeedText` and `ReplaceText` to return canonical rendered markdown. Both API callers now seed or replace the Yjs tree before writing an artifact version, and store/index that returned canonical markdown.
- Removed all service reads and writes of `Y.Text("content")`. Text reads, snapshots, settlements, named versions, agent edit operations, and offset-based suggestion replacement now use a Proof `pmdoc.Node` tree.
- Replaced UTF-16 string operations in agent edits with sequential `pmdoc.FindQuote`, `FindHeading`, `Size`, and `Splice` tree operations. Inline inserts/replacements preserve leading and trailing whitespace around an inline document.
- Preserved the transaction protocol: document reads occur outside a Yjs transaction; tree mutations occur in the one transaction; joined API writes still persist one update through `AppendUpdateTx`; and the rendered post-mutation tree drives transactional anchor re-resolution.
- Made settlement render the live Proof tree. A schema-invalid live tree logs the schema failure and skips settlement without retrying or creating a version.
- Updated HTTP error mappings for Proof target ambiguity/missing targets, invalid markdown, and document schema errors. Existing text-anchor ambiguity/missing-target mapping remains for Task 3's offset anchors. Browser anchors now honor `occurrence` during stale range recovery, preventing a new canonical offset collision from silently selecting a different repeated quote.
- Repointed document tests to browser-style tree mutation helpers and canonical markdown. Added coverage for tree seeding, tree-based agent edits at headings/quotes/edges, rejected invalid markdown, rendered settlement, schema-invalid settlement, invalid artifact uploads, and invalid issue specs.
- Review corrections:
  - quote anchors that no longer exist now return `404 TARGET_NOT_FOUND`, including the `pmdoc` target-not-found path;
  - `TestReresolveAnchorsClosesRowsBeforeUpdating` now verifies that the closed cursor path writes both a re-resolved ask and an orphaned comment;
  - `anchors_test.go` was touched because canonical rendering changes rendered-offset expectations and can collide with a stale selected range; `server_test.go` was touched for canonical primary-document text plus rejected issue-spec markdown; `persistence_test.go` was touched so manually-created version fixtures have the same canonical markdown that production creation persists; this report is the required Task 3 handoff artifact.

## Test evidence

### Red first

After rewriting the target document tests, I ran:

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/docs/ -run 'TestSeedText|TestApplyOps|TestSettle' -v
```

It exited 1 as intended before the implementation: the compiler reported the missing tree-era contract (`fragmentName`, `editLiveTree`, `replaceRun`, `ErrDocSchema`, and `ErrInvalidOp.Reason`) and the planned two-value `SeedText`/`ReplaceText` results. This proved the new tests exercised behavior absent from the legacy `Y.Text("content")` implementation.

### Review corrections

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/api/ -run '^TestAskAnchorAmbiguityAndOccurrence$' -v -count=1
```

- After changing the assertion to the required `404 TARGET_NOT_FOUND`, this exited 1 against the prior mapping: `missing anchor target: status=422`.
- After changing `anchorResolveError` to produce 404, the same command exited 0.

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/docs/ -run '^TestReresolveAnchorsClosesRowsBeforeUpdating$' -v -count=1
```

- With `reresolveAnchors` temporarily stubbed to return nil, this exited 1: the ask retained `[0,5)` instead of moving to `[7,12)`. Restoring the implementation made the command exit 0, including the orphaned-comment assertion. The test's successful per-row updates also require the result rows to close before the subsequent updates run.

### Focused green checks

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/docs/ -run 'TestSeedText|TestApplyOps|TestSettle' -v -count=1
```

- Exited 0: **13 passed**. This includes all five agent-edit tests, both seed tests, and six settlement tests.

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/api/ \
  -run 'TestUploadRejectsMarkdownOutsideProofSchema|TestCreateIssueRejectsSpecOutsideProofSchema|TestDocumentEditMapsMissingAndAmbiguousTargets' \
  -v -count=1
```

- Exited 0: **3 passed**.

### Package and full-suite checks

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && gofmt -l ./internal/dispatch
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && go vet ./internal/dispatch/...
```

- Both exited 0 with no output.

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/docs/ -count=1
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/api/ -count=1
```

- Initial full suites exited 0: docs completed in 16.460s and API in 36.407s. After the review corrections, both were rerun against the same isolated database and exited 0 again: docs in 27.238s and API in 52.017s.

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/envoy && \
  PROOF_EDITOR_DIST=/home/ubuntu/tmp/proof-editor-dist \
  DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55462/dispatch?sslmode=disable' \
  go test ./internal/dispatch/... -count=1
```

- Exited 0: API, auth, config, docs, events, githubapi, identity, outbox, pmdoc, routes, store, and text passed; `model` has no test files.

### Real-server E2E

I started the task-owned PostgreSQL containers `pg-pr3-t3` and `pg-pr3-t3e`, then ran the real Go Dispatch server through the web test harness:

```sh
cd /home/ubuntu/.worktrees/legion/pr3-tree/packages/dispatch && \
  DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55463/dispatch_c?sslmode=disable' \
  DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_E2E_PORT=8792 bun run e2e
```

- Exited 0 after building the web client and exercising **both Playwright projects**: Chromium had 40 passed and one intentional phone-only skip; iPhone had 40 passed and one intentional tablet-only skip. Total: **80 passed, 2 skipped**.
- Removed both task-owned containers with `docker rm -fv pg-pr3-t3 pg-pr3-t3e`.

## Self-review

- Reviewed every `SeedText`, `ReplaceText`, and legacy-content callsite in `internal/dispatch`; service behavior now uses the `prosemirror` fragment, and the sole remaining `GetText("content")` reference is the regression assertion that the legacy type stays empty.
- Confirmed all tree reads precede transactions and all `pmdoc.Update` calls use a fragment obtained before entering the transaction, avoiding ygo's document-lock reentry deadlock.
- Confirmed agent edits resolve against the progressively edited tree and return `pmdoc` target errors to the API for the intended 404/409 results.
- Confirmed live-tree schema failures cannot create a settled version or retry indefinitely.
- Confirmed review corrections map missing quote anchors to 404 and that the re-resolution test fails when its implementation is replaced by a no-op, then passes for the observable moved/orphaned outcomes.

## Concerns

No functional concerns. The successful E2E run emitted its existing Vite large-chunk advisory, Playwright `NO_COLOR` environment warnings, and expected canceled-request teardown log; all checks exited 0.

## Hardening ledger

No shortcuts, weakened/deleted tests, skipped checks, or deferred work.
