---
title: "A Dispatch rendering stands for one room instance, and an update is in the room only at or below the head its load folded"
category: envoy
tags:
  - dispatch
  - crdt
  - ygo
  - observer
  - rendering
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2453"
---

# A Dispatch rendering stands for one room instance, and an update is in the room only at or below the head its load folded

When you change the room's update observer, a write's fork, or anything else that reuses a rendering in `packages/envoy/internal/dispatch/docs`:

- Reuse a write's rendering for the room only on the room instance the write's fork was read from. ygo encodes the room's whole delete set in every update, so the same update can delete text in another instance. A fork read from the store (`coldFork`) carries no rendering, and a reloaded room's replica has a fresh epoch.
- Treat an update as already in the room only when its version is at or below `roomInstance.loadedHead`, the head the room's load read and folded (`load.head`). Do not use the cursor's start, `roomCursor.through`: it also folds in versions appended while the load ran, which are durable but not in the loaded document.
- Keep `*liveWriteOrigin` with its single constructor in `publishLiveUpdate`. The observer trusts its fields (`rendering`, `inRoom`) because no other commit carries that origin. Settlement and identity repairs use `*identityClosureOrigin` (`applySuppressed`), and service writes register their own origins (`serviceTransact`).
- A replica outside the schema (`r.markdown == nil`) renders every update, so the update still logs a WARN and counts as changed.

## Evidence

- `pending_authors.go:261` sets `loadedHead: load.head`; `livewrite.go:832` sets `origin.inRoom` before the update is applied, and `livewrite.go:829` is the only `&liveWriteOrigin{...}`.
- Negative controls in sjawhar/legion#2453: forcing "already in the room" to true makes `TestRenderReuseAColdWriteAppendedDuringALoadIsRendered` fail (`classified content_changed=false, the shadow says true`). Letting a cold fork record the replica's mark makes `TestRenderReuseARoomBehindTheHeadRendersThePublish` fail on the rendered bytes.
- The same facts are stated in `packages/envoy/AGENTS.md`, in the paragraph on the room's update observer.
