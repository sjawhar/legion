---
title: "A field a change outlaws is zeroed on every path that writes a record back, not only at the route that now refuses it"
category: envoy
tags:
  - deletion
  - read-modify-write
  - ttl
  - session-registry
  - interest-row
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2674"
---

# A field a change outlaws is zeroed on every path that writes a record back

Extends docs/solutions/envoy/a-field-that-decides-who-keeps-a-capability-is-read-from-the-store-never-from-a-cache-pr2592.md.

## The rule

Refusing a field at the route that sets it stops new values only.
Find every other path that reads a record and writes it back — a claim, a renewal, a heartbeat, a
refresh — and zero the field there too.
A write-back that carries the old value forward renews the record past the TTL that would have
retired it, so the value an upgrade was meant to drain never drains.

Then list the field's readers and say, for each, what it does while a legacy value is still
stored.
A reader that treats the field as a capability keeps reporting work done that nobody does.

Pin it with a record that carries the field, written by a build from before the change, one call of
the write-back path, and a read-back.

Second rule, from the same two stores: a row that answers "where is this client now" is rewritten
by the latest registration, field by field, with the stored value as the fallback only for a field
the caller cannot know.

## Evidence

sjawhar/legion#2674 made `/v1/interests/subscribe` answer 400 to a registration whose `port` is
above 0, because the Envoy listener no longer pushes to a session's serve port.

`roleSetHandler` (`packages/envoy/cmd/listener/api.go`) read the claimant's session entry back, set
`SelfSubscribed`, **kept its `Port`** and wrote it again.
So a client from before the change, whose every registration was refused 400, kept a `Port > 0`
entry alive by claiming a role: each claim renewed the entry past the five-minute `SessionEntry`
TTL that would otherwise have retired it.
While that entry lived, `session.Resolve` skipped it as self-subscribed and Dispatch's outbox
(`internal/dispatch/outbox/role.go`, `streamAck: entry.Port != 0`) counted the stream's
acknowledgement of the stored copy as that holder's delivery — messages reported delivered that
nobody handed to anyone.
The reviewer found it by reading lines **outside** the diff.
The fix is one line, `entry.Port = 0`, with the reason beside it, and the proof is a listener built
at the branch's pre-change fork point storing `s_legacy` at port 8080, then one
`POST /v1/roles/set` against the head turning it into `port: 0`.

The second rule is `mergeForUpsert` in `packages/envoy/internal/store/kv.go`.
It pinned a session's interest row to the registration that created it, so a session that
registered again through another listener kept being fanned out by the first and had its messages
stamped with the first listener's machine and directory.
It now takes `first(requested.MachineID, cur.MachineID)` and `first(requested.Dir, cur.Dir)`: the
registration's own values win, and the stored row is the fallback only for a field the caller
leaves empty — a role claim carries the claiming listener's machine id and no directory
(`SetRoleWithPrevious`), and claiming a role must not clear the row's `Cwd`.
Every reader of the row — `subjectIndex.set`, `Registry.Match`, the durable path's ownership check
and `senderStamp` — was left unchanged and became correct by the write alone.
