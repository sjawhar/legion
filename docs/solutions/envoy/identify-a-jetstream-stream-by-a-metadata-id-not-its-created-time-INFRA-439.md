---
title: "Identify a JetStream stream by a metadata id, not its created time"
category: envoy
tags:
  - nats
  - jetstream
  - stream-identity
  - backup-restore
  - cluster
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-439"
  - "sjawhar/legion#2228"
---

# Identify a JetStream stream by a metadata id, not its created time

**Written before dispatch://INFRA-771, when `POST /v1/replay` existed.** That route is gone: a
session reads only its own JetStream consumer, and the stream's identity now reaches a session
through its registration answer's `stream_id` rather than a replay answer's `stream.id`. The rule
below — identify a stream by a metadata id, never by its created time — is unchanged, and it is
what the consumer's resume point is compared against.

Extends docs/solutions/envoy/nats-jetstream-stream-ensure-only-adds-subjects.md.

## Rule

- Do not treat a change in `StreamInfo.Created` as "the stream was recreated". A backup and
  restore changes it. After a clustered restore, the node that ran the restore and the followers
  hold different times, so a leader stepdown between them changes it too.
- Give the stream an id in `StreamConfig.Metadata` (Envoy's key is `envoy_stream_id`), set once.
  A backup and restore keeps it (it is in the backup's config), and so does every election.
- Every reconcile that calls `UpdateStream` must carry the deployed stream's metadata. A config
  built from the process's own settings drops the metadata.
- An update whose only purpose is adding the id writes the stream's own `info.Config` with the id
  added. Writing the process's config instead resets fields the process does not choose: live, a
  3-replica stream became 1 replica.
- With no compare-and-swap on `UpdateStream`, racing first writers derive the id from what they
  all read, such as the stream name and its `Created`, rather than each picking a random one.
- A reader whose cursors are stream sequences also checks `cursor > last_seq` under the same id.
  The stream moved back (restored from an older backup, or re-added empty). Re-read from
  `first_seq - 1` and let a record of what was shown skip repeats. This cannot see a stream that
  grows past the cursor again before the next read.
- To drive this live with a local 3-node cluster, set `no_advertise: true` in each node's cluster
  block. Otherwise a client learns every node's address and reconnects around a proxy you killed
  to cut it off.

## Evidence

- In sjawhar/legion#2228 (round 6), sessions compared `Created`, and INFRA-14's planned move of
  `ENVOY_NOTIFICATIONS` to a clustered hub by backup and restore would have reset every session.
- Live on nats-server 2.15.1: a standalone stream was backed up with `nats stream backup` and
  restored with `nats stream restore --replicas 3`.
  - The followers held 02:33:38.205717744 and the restoring node 02:33:38.313960731.
  - A stepdown between them changed `/v1/replay`'s `stream.created`.
  - `envoy_stream_id` survived.
  - A session at 067a15af kept its cursor; the old plugin reset and skipped a message.
- Plain elections and restarts on a cluster that was never restored did not change `Created` on
  2.10.29 or 2.15.1: replicas take it from the stream assignment.
- Writing the deployed config back through nats.go v1.50's `StreamConfig` drops fields that struct
  lacks (`allow_atomic`, `allow_batched`). It fails on `persist_mode: async` or
  `allow_msg_schedules`.
- Commits: 313374bb (id set once, metadata kept, `/v1/replay` answers `stream.id`), 067a15af
  (id-only update writes the stream's own config), fcc7d008 (moved-back re-read; the tester's
  restore-behind-cursor legs went from LOSS to PASS).
