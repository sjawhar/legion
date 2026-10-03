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

Revoking a grant in Dispatch ends that session's access to its secrets until someone approves them
again. A grant someone approved is asked for again from its approver at the session's next
request. A grant the session got without asking **withholds** its secrets from that session: its
next request for them is sent to their owner for approval (to anyone signed in, for a shared
secret), instead of being granted at once; the person's other sessions still get them at once. To
end every session's access to a secret, [change its tags](#end-every-sessions-access-to-a-secret).
To end every session a machine started, and stop it starting more, [revoke its machine
login](#end-a-machines-login).

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
change applies to every session that got the secret automatically, where revoking one grant in
Dispatch withholds it from that grant's session alone.

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

## End a machine's login

A machine login lasts `BROKER_LAUNCHER_CREDENTIAL_SECONDS` from its approval, but the person who
approved it can end it sooner: for a machine that is lost, compromised or no longer used. Dispatch's
machine-login page (`/credentials/machine`) lists **Your machine logins**, every machine logged in
as you, with when its login was issued and when it expires. Click **Revoke** on its row and confirm.
The broker then, at once:

- refuses the machine's credential, so the machine enrolls no more sessions; and
- ends every session the machine enrolled, host sessions and boxes alike, as [ending a
  session](#end-a-session) does: each one's grants are revoked and its pending requests cancelled,
  and its next call is refused `PROOF_INVALID`.

Only the login's operator may revoke it; the broker refuses anyone else `NOT_OPERATOR`. A revoked
login stays revoked: the machine runs `agent-secrets launcher login` again, and its operator approves
the new code.

```console
$ curl -s -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" "$AGENT_SECRETS_URL/v1/launcher-credentials?operator=ada@example.com"
{"credentials":[{"credential_id":"5d2b7f0e-8a41-4c3e-9b6f-0c7e2a9d1f34","host":"example-host-devbox","issued_at":"2026-10-03T09:12:40.512Z","expires_at":"2026-10-10T09:12:40.508Z"}]}
$ curl -s -X POST -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" -d '{"operator":"ada@example.com"}' "$AGENT_SECRETS_URL/v1/launcher-credentials/5d2b7f0e-8a41-4c3e-9b6f-0c7e2a9d1f34/revoke-by-operator"
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

A helper with no session running finds out when it next enrolls one.

Stopping the helper also stops the machine from enrolling sessions, since its credential lives only
in the helper's memory, but it ends none: a host session lapses within `BROKER_LEASE_SECONDS` once
the helper stops, since the helper renews it, while a box renews itself (`agent-secrets renew`) and
keeps working. Revoking the machine login ends both.
