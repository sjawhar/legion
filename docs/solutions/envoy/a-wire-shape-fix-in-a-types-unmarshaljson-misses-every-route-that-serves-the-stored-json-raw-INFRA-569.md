---
title: "A wire-shape fix in a type's UnmarshalJSON misses every route that serves the stored JSON raw"
category: envoy
tags:
  - dispatch
  - json
  - unmarshal
  - event-payloads
  - wire-shape
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-569"
  - "sjawhar/legion#2363"
---

# A wire-shape fix in a type's UnmarshalJSON misses every route that serves the stored JSON raw

- When you fix how a field reads on the wire by decoding it through a model type (an
  `UnmarshalJSON` that turns `null` into `[]`, a default, a renamed key), first list every route
  that serves that field **without** decoding it into the type. In Dispatch these are mostly
  event payloads: `readEventRows` keeps a payload as `json.RawMessage`, and
  `attachAskEventFields` re-encodes it from `map[string]any`. That one path serves the issue
  event log, the artifact event log, the `GET /api/v1/events` catch-up and the relay's live
  frames. Fix each such route directly, and reuse the type's decoding there (decode the field
  through the type and write it back) rather than writing the rule a second time.
- Prove each route by asserting on the raw body (`map[string]json.RawMessage`,
  `string(v) == "[]"`), never by decoding it into the type. A typed decode runs the same
  `UnmarshalJSON` in the test and passes even when the server sent `null`.
- Fix stored rows on read, not only on the write path: rows and events written before the fix
  keep the old shape.

## Evidence

- INFRA-569 added `model.AskAnswer.UnmarshalJSON` (`model/model.go`), which reads a null or
  absent `selected` as `[]`, and made new answers store `[]` (`api/ask_transitions.go`). That
  fixed every typed read: `docs.ScanAsk`, `loadAskAnswersByAsk` and `listMyAnswers`. It did not fix
  `api/events.go`, whose `attachAskEventFields` comment says other payloads "pass through as the
  database returned it". The web `EventBody.tsx` ran `previous.selected.join(", ")` on such a
  payload and threw `TypeError: null is not an object`.
- The gap cost two send-backs on one root cause. The plan normalized search alone. The architect
  added the other ask routes. The reviewer found the event log, which went back to implementing
  at acceptance. The fix is `normalizeAnsweredSelected` in `api/events.go`;
  `TestAnsweredEventStoredWithNullSelectedServesEmptyLists` failed before it, with
  `"selected":null, want "selected":[]`.
- The raw assertion helper is `requireEmptySelected` (`api/ask_selected_test.go`).
