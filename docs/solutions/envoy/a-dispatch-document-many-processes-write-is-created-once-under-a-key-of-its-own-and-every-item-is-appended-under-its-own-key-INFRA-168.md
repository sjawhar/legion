---
title: "A Dispatch document many processes write is created once under a key of its own, and every item is appended under its own key"
category: envoy
tags:
  - dispatch
  - idempotency-key
  - artifacts
  - concurrent-writers
date: 2026-10-30
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-168"
  - "sjawhar/legion#2161"
---

# A Dispatch document many processes write is created once under a key of its own, and every item is appended under its own key

Dispatch has no create-only upload. An upload of a name the owner already holds is a new version of that document, and its text replaces everything the earlier versions held (`findArtifactByName` in `packages/envoy/internal/dispatch/api/artifacts.go`). So a second upload under a name erases every item appended since the first. That includes an upload that the client gave up on and that commits late, and an upload from another process. When several call sites or processes write items to one document:

- **Upload it once, and append every item.** Read the document by name first (`getProjectArtifact`), and create it only on Dispatch's own 404. Write every item with a `docEdit` insert at `end`, the first item included. Never re-upload to add an item.
- **Make the create one intent across attempts and processes.** Send it under a caller-chosen `Idempotency-Key` derived from the owner and the name. Give it a body fixed for the document, with no time, no plugin version and no first item in it. Then every attempt from every process is the same request, and Dispatch replays the key or answers `409 KEY_IN_FLIGHT` instead of running the create again.
- **Treat `409 KEY_COMMITTED` and `422 KEY_REUSED` as "go and read what landed".** Go back to the read by name and then to the append. Never send another upload.
- **Key each append by the item's own id** (for example, the sha256 of the writer and the item id). A retried or late append is then replayed, and it adds its own item or nothing.
- **Write down the two facts the rule rests on, and check them for your writers.** First, a key belongs to the login that sent it (`requestKeyLogin` in `request_keys.go`), so two writers under different logins do not share it. Every Legion pod writes under the shared agent token, which is one login. Second, Dispatch keeps a key for 24 h (`IdempotencyKeyTTL`), and a keyed run is bounded at 10 min (`keyedHandlerLimit`), so a late commit always lands while its key is remembered.
- **Test it against a fake Dispatch that follows both rules:** the replace-on-upload rule and the key states (a replay, `KEY_IN_FLIGHT`, `KEY_COMMITTED`, `KEY_REUSED`). Make a first create that is given up and commits late the red case.

This rule assumes `envoy-client`'s `DispatchClient` lets the caller give the key. `projectArtifact` and `docEdit` take an optional key, and the client sends it on every attempt of the call (`callerKey ?? crypto.randomUUID()` in `#send`).

## Evidence

- `packages/pi-envoy/extensions/askgate/decision-record.ts:20-51` states the rule and both facts. The by-name read is at `:443`, the create at `:468-509`, the keyed append at `:371-375`, `mayHaveCreated` at `:511`, and the key derivation `intentKey` at `:526`.
- 9aed5df8: two decision recorders in one session each uploaded the session's document, and the second upload replaced the first's record. Live sessions found it. The fix shares one recorder.
- 451e53fc fixed review thread `PRRT_kwDORFy7ds6x189x`: a first upload the client gave up could commit after a second upload and erase the records appended since. On the code before the fix, the test 'a first create this side gave up on, committed late, cannot replace the records written since' leaves the document holding only the first record.
- At 451e53fc, 22 concurrent live sessions against production Dispatch wrote 43 of 43 records, with no duplicates and none lost.
