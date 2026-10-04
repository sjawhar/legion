---
title: Revoke a session or a grant
description: End a grant from Dispatch or from the session holding it, cancel a pending request, end a session so nothing it held still works, and end a machine's login.
sidebar:
  order: 12
---

A grant ends on its own when it expires. To end access sooner, revoke the grant, or end the session
that holds it: ending a session revokes every grant it held and cancels every request it still had
pending. Ending a grant stops the session reading its values again; a command already running with
a value keeps it.

Revoking a grant in Dispatch ends it at once. A grant someone approved is asked for again from its
approver at the session's next request. When you revoke a grant your own session got without
asking, you **withhold** its secrets from that session: the session's other grants that got them
without asking end with it, and its next request for them is sent to their owner for approval (to
anyone signed in, for a shared secret) instead of being granted at once, so the session asks before
it gets them again; your other sessions still get them at once. Only the session's own person
withholds: a person who revokes a grant they approved on someone else's session ends that grant
alone. To end every session's access to a secret,
[change its tags](#end-every-sessions-access-to-a-secret). To end every session a machine started,
and stop it starting more, [revoke its machine login](#end-a-machines-login).

## Revoke a grant in Dispatch

Dispatch's **Settings** page lists your **Live grants**: every live grant of a session you operate,
whether it was granted automatically or approved by someone, and every grant you approved on
anyone's session. Each row names its enrollment, its secrets, how it was **Granted**
(**Automatically**, or **Approved by** an email), and when it was created and expires. Click
**Revoke** to end one at once; the session's next read of it is refused `GRANT_NOT_LIVE`. The
broker allows it when you are the grant's approver or its session's operator, and refuses anyone
else `NOT_APPROVER`.

```console
$ curl -s -X POST -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" -d '{"approver":"ada@example.com"}' "$AGENT_SECRETS_URL/v1/grants/<grant id>/revoke-by-approver"
{"state":"revoked"}
```

(That is the call Dispatch's server makes with the broker's UI token, the credential only
Dispatch's server holds; on the [local stack](/legion/broker/guides/run-locally/) you can make it
yourself.)

## Revoke a grant from its session

A session can end any of its own grants, by the grant id `agent-secrets self` lists. A session
ending its own grant withholds nothing: its next request is decided as before.

```console
$ agent-secrets revoke b51e58bc-3b7b-4144-a2ed-5821f232d612
revoked
$ agent-secrets self
enrollment_id: 2b8627f4-9a2a-4c12-85bf-910144bc0b5c
kind: host
operator: ada@example.com
lease_expires_at: 2026-10-03T03:36:40Z
```

## End every session's access to a secret

Retag the secret in Secrets Manager so its sessions no longer get it automatically: set its `tier`
to `human`, so every request for it needs a person's approval, or give it another `owner`, so the
sessions that had it are no longer the owner's own. Deleting the secret, or moving it out of
`BROKER_SECRETS_PREFIX`, ends access altogether. Once the broker rereads the namespace, at most five
minutes later, it refuses each read of a grant given automatically under the old tags with
`GRANT_NOT_LIVE`, and the session's next request for the secret waits for a person's approval. The
change applies to every session that got the secret automatically, where revoking a grant of your
own session in Dispatch withholds it from that session alone.

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
in one step, and the approver's Inbox drops them. A session ends in one of three ways:

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
- **Its machine login is revoked.** The person who approved the machine login that enrolled it
  revokes that login, which ends every session it enrolled
  ([end a machine's login](#end-a-machines-login)).

## End a machine's login

A machine login lasts `BROKER_LAUNCHER_CREDENTIAL_SECONDS` from its approval, but its sessions
outlive it: a session renews its lease with its own key, never with the machine's credential, so a
box keeps working after the machine's credential expires, and so does a host session while the
helper that renews it runs. The person who approved the login can end it, and every session it
enrolled, at any time: for a machine that is lost, compromised or no longer used, or a Legion
daemon whose pods must lose their secrets now. Dispatch's machine-login page
(`/credentials/machine`) lists **Your machine logins**: every machine logged in as you, and every
service whose login you approved, such as the Legion daemon's, shown as `legion-daemon on <host>`,
each with when its login was issued and when it expires. A login that has expired stays listed,
marked `expired, sessions still running`, while a session it enrolled still runs, so revoking every
row a machine has ends every session it started, those of its earlier logins included. Click
**Revoke** on its row and confirm. The broker then, at once:

- refuses the login's credential, so it enrolls no more sessions; and
- ends every session it enrolled (host sessions and boxes, or every worker pod of the Legion
  daemon's login) as [ending a session](#end-a-session) does: each one's grants are revoked and its
  pending requests cancelled, and its next call is refused `PROOF_INVALID`.

Only the person who approved the login may revoke it; the broker refuses anyone else
`NOT_APPROVER`. A revoked login stays revoked: the machine runs `agent-secrets launcher login`
again, or the Legion daemon starts a new login, and that person approves the new code.

```console
$ curl -s -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" "$AGENT_SECRETS_URL/v1/launcher-credentials?approver=ada@example.com"
{"credentials":[{"credential_id":"5d2b7f0e-8a41-4c3e-9b6f-0c7e2a9d1f34","host":"example-host-devbox","service":null,"issued_at":"2026-10-03T09:12:40.512Z","expires_at":"2026-10-10T09:12:40.508Z","expired":false},{"credential_id":"0b6c1d55-3e7a-4f02-8c19-6a4e2d7b9f10","host":"example-host-cluster","service":"legion-daemon","issued_at":"2026-10-02T17:40:03.101Z","expires_at":"2026-10-09T17:40:03.097Z","expired":false},{"credential_id":"9a4e1c27-5b3d-4f8a-a6e0-2d7c1b9f4e83","host":"example-host-devbox","service":null,"issued_at":"2026-09-26T09:10:12.044Z","expires_at":"2026-10-03T09:10:12.040Z","expired":true}]}
$ curl -s -X POST -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" -d '{"approver":"ada@example.com"}' "$AGENT_SECRETS_URL/v1/launcher-credentials/5d2b7f0e-8a41-4c3e-9b6f-0c7e2a9d1f34/revoke-by-approver"
{"state":"revoked"}
```

(Those are the calls Dispatch's server makes, as above.)

Nothing tells the machine's helper; it finds out the next time it calls the broker. A session's
renewal, within a third of `BROKER_LEASE_SECONDS`, is refused; revoking that session's enrollment is
refused too, and the helper drops the credential and logs at ERROR `the broker refused the launcher
credential (…); cleared: no session can enroll until a human approves a new machine login`. From
then `agent-secrets launcher login-status` exits 1:

```console
$ agent-secrets launcher login-status
expired
agent-secrets launcher login-status: the broker refused the launcher credential (expired or revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch); run: agent-secrets launcher login
```

A helper with no session running finds out when it next enrolls one. The Legion daemon finds out
when it next enrolls or ends a pod: the broker refuses its credential `LAUNCHER_INVALID`, and the
daemon drops it and starts a new machine login, whose code it logs, as when it first logged in. The
pods it had enrolled do not wait for that: their sessions ended with the revoke, so their agents'
`agent-secrets` calls are refused `PROOF_INVALID` from then on.

Stopping the helper also stops the machine from enrolling sessions, since its credential lives only
in the helper's memory, but it ends none. A host session lapses within `BROKER_LEASE_SECONDS` once
the helper stops, since the helper is what renews it. A box renews itself (`agent-secrets renew`),
so it keeps working after the helper stops and after the machine's credential expires. Revoking the
machine login ends both, and its row stays on the machine-login page while either runs, marked
`expired, sessions still running` once the login has expired. Without Dispatch, end each box with
`agent-secrets unenroll --helper --enrollment <id>`, run against a helper logged in as the box's
operator, or stop its `agent-secrets renew`, after which it lapses within `BROKER_LEASE_SECONDS`.
