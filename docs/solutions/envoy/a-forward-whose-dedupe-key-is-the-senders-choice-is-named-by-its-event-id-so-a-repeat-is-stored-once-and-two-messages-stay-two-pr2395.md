---
title: "A forward whose dedupe key is the sender's choice is named by its event id, so a repeat is stored once and two messages stay two"
category: envoy
tags:
  - nats
  - jetstream
  - deduplication
  - role-lane
  - forward-merge
date: 2026-11-01
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "sjawhar/legion#2395"
---

# A forward whose dedupe key is the sender's choice is named by its event id, so a repeat is stored once and two messages stay two

Extends docs/solutions/envoy/a-forward-to-a-captured-subject-is-stored-and-retried-forwards-share-one-msgid-per-recipient-pr2370.md.

- **Name a forward by what identifies one message.** The `Nats-Msg-Id` of a forward to a captured
  subject decides which sends the stream counts as one. It must be equal for every send of one
  message to one recipient, and different for two messages.
- **A dedupe key qualifies only when it names the event.** When the envelope's key is derived
  from the upstream event (`contracts.DedupeKeyNamesTheUpstreamEvent`), name the forward by that
  key and the subject (`bus.ForwardMsgID`). When the key is the sender's choice, two different
  envelopes can share it, so name the forward by the envelope's `event_id` and the subject: every
  envelope carries one, a retry or a second listener forwarding the same message keeps it, and two
  messages never share it.
- **Never drop the name to protect distinct messages.** A forward with no `Nats-Msg-Id` keeps two
  messages apart but stores a copy for every listener and every retry that sends it.
- **When a merge brings two rules for one name, keep both tests.** Each side's test pins one half
  (one message stored once; two messages under one key stored twice). A rule that passes both,
  unchanged, is the merge; one that passes only one side's test has dropped the other's behaviour.

## Evidence

- `packages/envoy/cmd/listener/role_lane.go:77` (`roleForward.msgID`), written at the merge
  `e1639b5b` of main's #2446 into this pull request (diff check
  https://github.com/sjawhar/legion/pull/2395#issuecomment-6562838986). Main's role lane named
  every forward by its dedupe key; this pull request's named a forward only when its key named
  its event, and otherwise sent none.
- Main's `TestRoleLaneRefusedSharedQueueFallsBackToMachineGroupsAndStoresOneCopy`
  (`cmd/listener/role_lane_test.go:398`): two listeners forwarding one message leave one stored
  copy. This pull request's `TestARoleForwardIsStoredOncePerMessageAndTwoUnderOneChosenKeyTwice`
  (`cmd/listener/delivery_role_forward_test.go:24`): two envelopes sharing a chosen key leave two.
  Both pass unchanged at `7892827c`, and the live `scripts/e2e/envoy-session-delivery.sh` run at
  that merge counted every role event once in a real Oh My Pi and a real Claude Code session.
- `event_id` is required by the envelope schema (`packages/contracts/schemas/envelope.schema.json`)
  and checked on decode (`internal/contracts/generated.go`).
