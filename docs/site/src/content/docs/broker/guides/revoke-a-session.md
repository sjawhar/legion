---
title: Revoke a session or a grant
description: End a grant from Dispatch or from the session holding it, cancel a pending request, and end a session so nothing it held still works.
sidebar:
  order: 12
---

A grant ends on its own when it expires. To end access sooner, revoke the grant, or end the session
that holds it: ending a session revokes every grant it held and cancels every request it still had
pending. Ending a grant stops the session reading its values again; a command already running with
a value keeps it.

Revoking a grant ends a session's access only to a secret someone must approve: the session's next
request for it waits for that approval again. A grant given automatically comes straight back: the
same session's next request for that secret is granted at once, under a new grant. To end access to
an automatic secret, [change its tags](#end-access-to-an-automatic-secret).

## Revoke a grant in Dispatch

Dispatch's **Settings** page lists your **Live grants**: the live grants you approved, and those on
sessions you operate, each with its enrollment, names, approver, and when it was created and
expires. Click **Revoke** to end one at once; the session's next read of it is refused
`GRANT_NOT_LIVE`. The broker allows it when you are the grant's approver or its session's operator,
and refuses anyone else `NOT_APPROVER`. A grant given automatically has no approver and is
not in that list. Its operator can revoke it through the same broker route, with the grant id the
session's `agent-secrets self` prints, but that does not end access: the session's next request for
the secret is granted again at once.

```console
$ curl -s -X POST -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" -d '{"approver":"ada@example.com"}' "$AGENT_SECRETS_URL/v1/grants/<grant id>/revoke-by-approver"
{"state":"revoked"}
```

(That is the call Dispatch's server makes with the broker's UI token, the credential only
Dispatch's server holds; on the [local stack](/legion/broker/guides/run-locally/) you can make it
yourself.)

## Revoke a grant from its session

A session can end any of its own grants, by the grant id `agent-secrets self` lists:

```console
$ agent-secrets revoke b51e58bc-3b7b-4144-a2ed-5821f232d612
revoked
$ agent-secrets self
enrollment_id: 2b8627f4-9a2a-4c12-85bf-910144bc0b5c
kind: host
operator: ada@example.com
lease_expires_at: 2026-10-03T03:36:40Z
```

## End access to an automatic secret

Retag the secret in Secrets Manager so its sessions no longer get it automatically: set its `tier`
to `human`, so every request for it needs a person's approval, or give it another `owner`, so the
sessions that had it are no longer the owner's own. Deleting the secret, or moving it out of
`BROKER_SECRETS_PREFIX`, ends access altogether. Once the broker rereads the namespace, at most five
minutes later, it refuses each read of a grant given automatically under the old tags with
`GRANT_NOT_LIVE`, and the session's next request for the secret waits for a person's approval. The
change applies to every session that got the secret automatically, not to one session.

## Cancel a pending request

A session that no longer needs what it asked for withdraws the request, which takes it off the
approver's list. It names the request by the request id `agent-secrets` printed when it asked
(`agent-secrets request` prints it, and so does a command whose `--wait` ran out):

```console
$ agent-secrets cancel 715ea84a-6f5c-4230-83c9-1cf258acd88b
cancelled
$ agent-secrets status 715ea84a-6f5c-4230-83c9-1cf258acd88b
state: cancelled
decided_by: session:2b8627f4-9a2a-4c12-85bf-910144bc0b5c
```

## End a session

Ending a session's enrollment revokes all of its grants and cancels all of its pending requests
in one step, and the approver's Inbox drops them. A session ends in one of two ways:

- **Its launcher revokes it.** A host session's ends when its process exits: the helper sees the
  registered process go and revokes its enrollment (it logs
  `session retired … why="process exited"`). `agent-secrets-helper sessions` lists the live ones.
  A box's launcher revokes it with `agent-secrets unenroll --helper --enrollment <id>`.
- **Its lease lapses.** A session that stops renewing (a pod that is gone, a box whose
  `agent-secrets renew` stopped, or a machine that went away) is refused `PROOF_INVALID` from the
  moment its lease lapses, `BROKER_LEASE_SECONDS` after its last renewal. The broker's sweep ends it
  on its next run, within `BROKER_SWEEP_SECONDS`, and logs
  `broker sweeper: ended an enrollment whose lease lapsed`. The
  [configuration reference](/legion/broker/reference/config/) gives both settings' defaults.

## Stop a machine from enrolling sessions

A machine's credential lives only in its helper's memory: stopping the helper stops the machine
from enrolling new sessions until someone logs it in again, and the credential expires on its own
after `BROKER_LAUNCHER_CREDENTIAL_SECONDS`. The broker has no route that revokes a launcher
credential before then.

Neither ends a session already enrolled: a session renews its lease with its own key, never with
the machine's credential. A host session lapses within `BROKER_LEASE_SECONDS` once the helper
stops, since the helper is what renews it. A box renews itself (`agent-secrets renew`), so it keeps
working after the helper stops and after the machine's credential expires: end each box with
`agent-secrets unenroll --helper --enrollment <id>`, run against a helper logged in as the box's
operator, or stop its `agent-secrets renew`, after which it lapses within `BROKER_LEASE_SECONDS`.
