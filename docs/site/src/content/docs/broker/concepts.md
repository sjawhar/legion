---
title: Concepts
description: Sessions and enrollment, machine logins, owner and tier, grants and their lifetime, approvals, and the audit record.
sidebar:
  order: 2
---

This page explains the ideas the rest of the broker's documentation leans on. Each section names
the code that implements it, so you can check the page against the source. A `BROKER_*` name is one
of the broker's settings; the [configuration reference](/legion/broker/reference/config/), generated
from the loader, gives each one's default and accepted values.

## Sessions and enrollment

An **agent session** is one running agent and everything it starts. On a machine, it is the
process `agent-secrets register --exec` starts (a coding agent, or a shell) and every process below
it; in a container or a Kubernetes pod, it is whatever holds the session's key directory. The
broker never deals with a person's or a machine's long-lived key on the agent's behalf. Instead,
every session has a P-256 signing key of its own that lives only as long as the session does, and
an **enrollment** that binds that key to the broker.

Every call a session makes carries a `Proof` header: a JSON Web Signature over the call's method
and URL, signed with the session's key, used once, and valid only within
`BROKER_PROOF_SKEW_SECONDS` of the broker's clock. There is no bearer token for a session to leak.

A **launcher** is the program that enrolls sessions: `agent-secrets-helper`, a per-user daemon on
a machine, for that machine's sessions, or the Legion daemon for the pods it runs. An enrollment
has one of three kinds:

| Kind | What it is | Who holds its key | Who enrolls it |
| --- | --- | --- | --- |
| `host` | An agent session running directly on a machine. | The machine's helper, in memory. | The helper, when the session registers with `agent-secrets register`. |
| `box` | An agent session running in a container on a machine, called a **box**. | The container, in `key.pem` under `AGENT_SECRETS_KEY_DIR`. | The machine's helper, when whoever starts the container runs `agent-secrets enroll --helper` ([run an agent in a container](/legion/broker/guides/run-an-agent-in-a-container/)). |
| `pod` | A Legion worker pod in Kubernetes. | The pod, in its key directory. | The Legion daemon, which proves the pod with a projected service-account token the broker verifies against `BROKER_K8S_OIDC_ISSUER`. |

An enrollment is leased for `BROKER_LEASE_SECONDS` and renewed while its session runs: the helper
renews host sessions, `agent-secrets renew` renews a box, and Legion's pod shim renews a pod. An
enrollment has **ended** once its launcher revokes it (the helper does as soon as a host session's
process exits), the person who approved its machine login revokes that login
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)), or its
lease lapses. From the moment the lease lapses, the session's calls are refused `PROOF_INVALID`; the
broker's sweep, which runs every `BROKER_SWEEP_SECONDS`, then ends the enrollment on its first run
after the lapse. Ending an enrollment any of these ways revokes every grant it held and cancels
every request it still had pending, so those leave the approver's Inbox and an approval can never
land on a session that is gone
(`packages/envoy/internal/broker/enroll/enroll.go`, `endEnrollment`).

Every `host` and `box` enrollment records an **operator**: the person whose machine it runs on. The
operator is the person who approved the machine login that enrolled it, never a value the launcher
chooses. A `pod` enrollment records none, since the Legion daemon logs in as a service rather than
as a person.

## Machine login

A launcher (a machine's helper, or the Legion daemon) cannot enroll anything until a person has
approved a **machine login** for it. The login works like a device code:

1. The machine generates a fresh key and asks the broker to log in, naming the person who should
   approve it (the helper reads that person's Dispatch login from `AGENT_SECRETS_OPERATOR_FILE`).
2. The broker answers with an eight-character confirmation code, `XXXX-XXXX`, which the machine
   prints.
3. That person types the code into Dispatch, sees which machine is asking, and approves or denies
   it. Only the typed code selects a machine login: no link can approve one.
4. On approval the broker mints a **launcher credential** bound to the machine's key. It is never a
   token: the machine uses it by signing with that key.

A launcher credential lasts `BROKER_LAUNCHER_CREDENTIAL_SECONDS`. The person who approved it may
revoke it from Dispatch's machine-login page, before or after it expires, which also ends every
session it enrolled
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)); no one
else may. Its sessions outlive its expiry: a session renews its lease with its own key, never with
the machine's credential, so a box keeps working after the machine's credential expires, and the
page lists an expired login, marked as expired with sessions still running, until its last session
ends. On a person's own machine, whoever approved its login is its operator. The Legion daemon's
login has no operator, since it enrolls pods, so it is listed for whoever approved it, and their
revoke ends every pod it enrolled. The helper keeps its key in memory only, so a helper restart,
like an expired or revoked credential, means logging the machine in again. A
machine login nobody decides expires after 15 minutes, a fixed time rather than a setting
(`machineLoginPendingTTL` in `packages/envoy/cmd/broker/main.go`).

A credential enrolls sessions only for its own operator: an enrollment naming anyone else is refused
`OPERATOR_MISMATCH`. The broker's rate limiter caps how often anyone can start a machine login, per
source address and per named operator.

## Owner and tier: who may have which secret

The broker reads who may have a secret from the secret itself
(`packages/envoy/internal/broker/policy/`). Every agent secret is a secret in AWS Secrets Manager
under one namespace, `BROKER_SECRETS_PREFIX` (such as `production/agent-secrets/`), encrypted with
one KMS key, `BROKER_SECRETS_KMS_KEY_ARN`, and tagged with its owner and its tier:

- **Its name** under the prefix is the name a session asks for, in lowercase with each underscore a
  hyphen: `production/agent-secrets/deel-api-key` is `DEEL_API_KEY`. It is lowercase letters, digits
  and single hyphens, starting with a letter.
- **`owner`** is `shared`, or a person's email in lowercase, the email they sign in to Dispatch with.
- **`tier`** is `agent` or `human`.

Who gets a secret follows from those two tags alone:

| The secret | Its owner's own session | Another person's session, or a pod |
| --- | --- | --- |
| A person's, `tier=agent` | Granted at once. | Sent to the owner for approval. |
| A person's, `tier=human` | Sent to the owner for approval. | Sent to the owner for approval. |
| `owner=shared`, `tier=agent` | Granted at once. | Granted at once. |
| `owner=shared`, `tier=human` | Approved by anyone signed in to Dispatch. | Approved by anyone signed in to Dispatch. |

A session is its owner's own when its operator is the owner: the owner approved the machine login
it enrolled under. A pod has no operator, so a pod asking for a person's agent-tier secret sends it
to that person for approval. An owner may also be a service, whose secrets go only to that
service's own sessions; the broker has no way yet to register a service, so it refuses a secret
whose owner tag names one.

One thing besides the tags changes what a session gets at once: a session's own person, its
operator, who revokes a grant the session got without asking **withholds** its secrets from that
session, even when the grant had already ended by then
(`packages/envoy/internal/broker/requests/machine_state.go`, `RevokeByApprover`). Each
withheld secret is human tier to that session for as long as the session lives: every other grant
the session got it on without asking ends with the revoked one, and the session's requests for it
are sent to the owner for approval, or to anyone signed in to Dispatch for a shared secret, so the
session asks before it gets the secret again. Only the session's own person withholds: anyone else
who revokes one of its grants, the person who approved it, ends that grant alone. Other sessions,
the person's own included, still get the secrets at once.

The broker reads the namespace when it starts, and refuses to start when it cannot, then again every
five minutes, a fixed time rather than a setting (`policyRefresh` in
`packages/envoy/cmd/broker/main.go`). A tag change takes effect at the next read. A read that fails
(Secrets Manager or KMS out of reach) is logged, and the policy from the last good read stays in
force. The broker leaves out a secret it cannot serve, and logs it by name on every read ([Operating
the broker](/legion/broker/operate/#health-and-logs)): a missing or malformed `owner` or `tier`
tag (an email with a capital letter is malformed), a name that is not in the form above, or a
secret encrypted with any key but the agent-secrets key, the AWS-managed key included. A request
for a secret left out is refused `UNKNOWN_SECRET`, and a grant of it stops.

The policy's **version** is the SHA-256 of every served secret's name, owner, tier and ARN, recorded
on every request.

## Requests and grants

A session asks for one or more secrets by name in a **request**, signed with its key and carrying a
reason of at most 400 characters. The broker evaluates every name
(`packages/envoy/internal/broker/requests/machine_state.go`):

- A name the policy does not serve refuses the whole request with `UNKNOWN_SECRET`, and nothing is
  recorded.
- A name the policy denies denies the whole request; nothing is ever half-granted. Only a service's
  secret, asked for by anyone but that service, is denied.
- Names that need approval must all need the same approver, else the request is refused
  `MIXED_APPROVERS`; request them separately.
- When every name is automatic, the request is granted at once.
- Otherwise it is **pending**, and the broker writes a credential-request record for the approver to
  decide. An identical request from the same session while one is pending joins it rather than
  asking twice.

A granted request yields a **grant**: the session's right to read those values until the grant
expires. A grant lives `BROKER_MAX_GRANT_SECONDS`, counted from the moment it is granted, unless its
session ends first. While it lives, a new request from the same session for exactly the same names
gets the same grant back, without asking anyone again, as long as the current policy still allows
it.

The broker never stores a value. Each time a session reads a grant, the broker checks that the
session is still enrolled, the grant is live, its approval still verifies, and the current policy
still allows every name; then it reads the value from the secret store (AWS Secrets Manager in
production) and returns it to that session alone. `agent-secrets NAME -- command` puts each value
in the command's environment under its name and replaces itself with the command. `agent-secrets`
itself never prints a value, but the command is the agent's choice, and a command can print or send
what its environment holds (`printenv NAME` prints it). A session holding a grant can therefore read
the value: approve a secret only for a session you would trust with the value itself.

A grant ends when it expires, when its session revokes it (`agent-secrets revoke`), when its
approver or its enrollment's operator revokes it in Dispatch, or when its enrollment ends. A grant
the session got without asking is revoked like any other, and when its operator revokes it the
session asks for approval before it gets the same secrets again (the operator
[withholds](#owner-and-tier-who-may-have-which-secret) them, even when the grant had already
ended); a session revoking its own grant, and anyone but the operator revoking one, withholds
nothing. To end every session's automatic access to a secret, change its tags
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/#end-every-sessions-access-to-a-secret)).

## Approvals

An approval happens in Dispatch, never in the broker. The broker holds no Dispatch credential and
sends Dispatch nothing: Dispatch's server reads the broker's pending list and records for the
person signed in, and when that person clicks **Approve** or **Deny**, Dispatch's server calls the
broker with the shared UI token (`BROKER_UI_TOKEN`) and that person's email, taken from their
Dispatch session. The broker then checks that email against the record's approver: anyone else is
refused `NOT_APPROVER`, whatever state the record is in. A request for a shared human-tier secret,
or for a shared agent-tier secret its session's operator withheld from it, names the approver
`anyone`: it waits in every person's Inbox, any person signed in to Dispatch may decide it, and the
broker records the email of whoever did. A withheld secret is its owner's to approve for that
session even in a request that was already waiting when the operator withheld it: a request that
waited on anyone for a shared human-tier secret and the operator's own withheld secret is then the
operator's alone to approve, and anyone else's approval is refused `NOT_APPROVER`. While the
withheld secret is out of the broker's policy (deleted, or its tags refused), no one may approve
such a request, since the secret's owner cannot be read; it waits until the secret is back, or
expires. The approval it gets lasts as any approval does (below): a change to another secret's
tags does not end it, nor does the operator's later revoke of another grant, unless that revoke
withholds another secret the request got without asking.

An approval belongs to the person who gave it, so a change to a secret's tags reaches what was
approved before it wherever the new tags want the secret approved for that session:

- An approved grant keeps releasing its value only while the person who approved it may still
  approve the secret for its session. Once a shared human-tier secret becomes a person's, or a
  person's secret becomes another's, every such grant the new owner must approve and did not stops
  with `GRANT_NOT_LIVE`, whatever value the secret holds by then, and the session's next request is
  decided under the new tags. A grant keeps working when the secret becomes shared (anyone may
  approve a shared human-tier secret, and a shared agent-tier one needs no approval), and when the
  new tags give its session the secret without asking, as they do once an agent-tier secret is the
  session's operator's. A secret the operator withheld from the session stays human tier there
  under any tags, so a grant of it someone else approved stops once the secret is the operator's.
- A pending request is approved by whomever the new tags name to approve it for its session. One
  waiting on anyone for a secret that has become a person's is that person's alone to approve;
  anyone else's approval is refused `NOT_APPROVER`, the requester's own operator included. One
  waiting on a person for a secret another person must now approve can be approved by no one. One
  waiting on a person stays that person's when the secret becomes shared, or when the new tags give
  its session the secret without asking. A denial releases nothing, so the approver a request waits
  on can still deny it, which takes it off the pending list: for a request waiting on anyone, any
  signed-in person may deny it. Otherwise it expires, or its session cancels it
  (`agent-secrets cancel`) and asks again.

A record is decided once. A second click, a concurrent one, or one after the record expired gets
`RECORD_TERMINAL`. A pending request nobody decides expires after 12 hours, a fixed time rather than
a setting (`agentSecretPendingTTL` in `packages/envoy/cmd/broker/main.go`). Because the UI token
vouches for whoever Dispatch says is approving, it is an approval credential: only Dispatch's
server may hold it.

## The audit record

Everything a person decides rests on a **credential-request record**
(`packages/envoy/internal/broker/record/record.go`): a fixed, line-by-line text holding the
session's signed request verbatim, the approver, the enrollment's kind, runtime id and operator,
the lifetime, the policy version, the expiry, and, for a machine login, its confirmation code. The
record's id is the SHA-256 of that text, so a record cannot change without changing its id.
Decisions are events on the record (`approved`, `denied`, `expired`, `cancelled`, `revoked`), each
naming who made it.

Every time the broker releases a value or authenticates a launcher credential, it re-verifies the
whole chain: the record still reproduces its id, the session's signature inside it still verifies,
and exactly one approval by the record's approver stands behind it. A grant or credential with no
approved record behind it releases nothing.

Beside the records, the broker keeps an append-only `audit` table with one row per event:
`enrollment.created`, `enrollment.revoked`, `enrollment.expired`, `request.created`,
`request.granted`, `request.denied`, `request.cancelled`, `request.expired`, `grant.used` (each time
a session reads a grant, naming the secrets released), `grant.revoked` (naming, under `withheld`,
the secrets an operator's revoke withheld from the session, and, on each other grant of the session
that revoke ended, the withheld secrets that grant held), `grant.withheld` (an operator's revoke of
a grant already revoked, naming under `withheld` the secrets it withheld from the session) and
`launcher_credential.revoked` (naming the machine login's `credential_id`, its `host`, its
`service` for a service's login, and under `enrollments` the sessions the revoke ended). Each row
names its actor: `human:<email>`, `session:<enrollment id>`, `launcher:<credential id>` or
`broker`. No record, event or audit row ever holds a secret value.
[Operating the broker](/legion/broker/operate/#the-audit-record) shows how to read them.
