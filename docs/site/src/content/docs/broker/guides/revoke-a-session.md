---
title: Revoke a session or a grant
description: End a grant from Dispatch or from the session holding it, cancel a pending request, and end a session so nothing it held still works.
---

A grant ends on its own when it expires. To end access sooner, revoke the grant, or end the session
that holds it: ending a session revokes every grant it held and cancels every request it still had
pending.

## Revoke a grant in Dispatch

Dispatch's **Settings** page lists your **Live grants**: the live grants you approved, and those on
sessions you operate, each with its enrollment, names, approver, and when it was created and
expires. Click **Revoke** to end one at once; the session's next read of it is refused
`GRANT_NOT_LIVE`. The broker allows it when you are the grant's approver or its session's operator,
and refuses anyone else `NOT_APPROVER`. A grant the rules gave automatically has no approver and is
not in that list; its operator can still revoke it through the same broker route:

```console
$ curl -s -X POST -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" -d '{"approver":"ada@example.com"}' "$AGENT_SECRETS_URL/v1/grants/6e38a949-636f-44b4-8f7b-24baf9efa744/revoke-by-approver"
{"state":"revoked"}
```

(That is the call Dispatch's server makes; on the [local stack](/legion/broker/guides/run-locally/)
you can make it yourself.)

## Revoke a grant from its session

A session can end any of its own grants. `agent-secrets self` lists them:

```console
$ agent-secrets revoke c6c1f92f-7ff2-4818-86a0-860494e7ddd7
revoked
$ agent-secrets self
enrollment_id: ddc91237-0835-4ac3-8dc8-936446809913
kind: host
operator: ada@example.com
lease_expires_at: 2026-10-03T02:21:47Z
```

## Cancel a pending request

A session that no longer needs what it asked for withdraws the request, which takes it off the
approver's list:

```console
$ agent-secrets cancel e8a655b5-905d-4681-b9d9-5e3baa990b34
cancelled
$ agent-secrets status e8a655b5-905d-4681-b9d9-5e3baa990b34
state: cancelled
decided_by: session:f1fb7fd9-db9f-4a8b-9b58-a567833bdab3
```

## End a session

Ending a session's enrollment revokes all of its grants and cancels all of its pending requests
in one step, and the approver's Inbox drops them.

- **A host session** ends when its process exits: the helper sees the registered process go and
  revokes its enrollment (it logs `session retired … why="process exited"`).
  `agent-secrets-helper sessions` lists the live ones.
- **A pod's** enrollment ends when its lease runs out once the pod stops renewing it, as does
  **any session's** that stops renewing (for instance because its machine went away): the broker
  ends it after `BROKER_LEASE_SECONDS`.

## Stop a machine from enrolling sessions

A machine's credential lives only in its helper's memory: stopping the helper stops the machine
from enrolling new sessions until someone logs it in again, and the credential expires on its own
after `BROKER_LAUNCHER_CREDENTIAL_SECONDS`. The broker has no route that revokes a launcher
credential before then.
