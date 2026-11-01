---
title: "A loop woken by Postgres NOTIFY keeps five rules"
category: architecture-patterns
tags:
  - postgres
  - listen-notify
  - background-loops
  - multi-process
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch
---

# A loop woken by Postgres NOTIFY keeps five rules

Extends docs/solutions/architecture-patterns/turning-a-full-scan-loop-into-a-change-feed-and-a-deadline-queue.md.

1. Send the NOTIFY from a trigger or from the SQL function every writer already calls, so writes
   by the previous release during a rolling deploy, and operator commands, wake the loop too.
2. Send it only from transactions that already notify. A notifying transaction takes Postgres's
   notify-queue lock at commit; a NOTIFY added to a transaction that took none adds that lock to
   it. List each new site and the notify its transaction already carries, and name exceptions.
3. Hold passes to the period of the timer the wake replaces (`duty.WokenLoop.MinGap`), so a busy
   database never runs the loop more often than before.
4. Wake every subscriber after each successful LISTEN, the first included, and run a pass at boot
   without waiting for the listener: Postgres does not replay a notification sent while nothing
   listened (`pglisten.Hub`).
5. A leased loop whose lease is held elsewhere retries until a pass of its own runs: the holder's
   pass may have read before the change that woke this process (`duty.RunWoken`).

Give each new permanent LISTEN connection a place in the per-process connection count
(`store.permanentListeners`, mirrored by `infra/gcp` `dispatchConnectionsPerPod`).

## Evidence

Pull request sjawhar/legion#2391 moved the embedding queue, reference excerpts, suggestion
outcomes, edit notices and the delivery settings poll onto these rules. The notify sites are
migrations `0151_embeddings_notify`, `0153_refs_excerpts_owed_notify`,
`0156_delivery_settings_notify` and `0157_document_edit_notices_notify`, each in a transaction
that already notifies (`0107_events_notify` or `0100_delivery_changes`). Two head processes made
3.34 statements per second each at idle against 4.50 before, with every changed loop at zero; the
reviewer's round confirmed each notify site and that no loop runs more often than its timer did.
