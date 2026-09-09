# Task C4 report

## Delivered

- Added the visible-artifact margin: threaded, newest-first comments; anchored asks; suggestions with accept/reject; pinned events; and the `ArtifactsTabSlot` mount point.
- Added selection actions and the markdown composer, including reference chips, `Ctrl+K` current-issue/artifact picker, and image paste/drop uploads.
- Added live CodeMirror anchor decorations. Decorations map through document changes and card hover/click state updates without replacing their mapped positions.
- Added orphaned-anchor history navigation. Historical views locate an orphaned comment's unique quote in its preserved version when its live mapped offsets have moved, then highlight it read-only.
- Added client-side Ask option normalisation at the API boundary for server responses that contain `options: null`.

## DocView selection limit

DocView enables Comment and Ask only when the browser selection maps unambiguously to one rendered markdown block whose source offsets and rendered text exactly match. Suggestions remain editor-only. Selections across blocks or transformed markdown are intentionally not anchored.

## Browser evidence

The real Dispatch Go server and Postgres acceptance suite passed in Chromium and iPhone. Captured screenshots:

- `e2e/test-results/margin.e2e.ts-margin-creat-f4c8a-erves-anchored-review-items-chromium/anchored-margin-comment.png`
- `e2e/test-results/margin.e2e.ts-margin-creat-f4c8a-erves-anchored-review-items-chromium/orphaned-margin-card.png`
- equivalent `iphone/` paths

## Hardening ledger

No shortcuts or skipped/disabled tests.