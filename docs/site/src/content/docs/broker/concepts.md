---
title: Concepts
description: Sessions and enrollment, machine logins, rules and approvers, grants and their lifetime, approvals, and the audit record.
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
process exits) or its lease lapses. From the moment the lease lapses, the session's calls are
refused `PROOF_INVALID`; the broker's sweep, which runs every `BROKER_SWEEP_SECONDS`, then ends the
enrollment on its first run after the lapse. Ending an enrollment either way revokes every grant it
held and cancels every request it still had pending, so those leave the approver's Inbox and an
approval can never land on a session that is gone
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

A launcher credential lasts `BROKER_LAUNCHER_CREDENTIAL_SECONDS`. The helper keeps its key in memory
only, so a helper restart, like an expired credential, means logging the machine in again. A
machine login nobody decides expires after 15 minutes, a fixed time rather than a setting
(`machineLoginPendingTTL` in `packages/envoy/cmd/broker/main.go`).

A credential enrolls sessions only for its own operator: an enrollment naming anyone else is refused
`OPERATOR_MISMATCH`. The broker's rate limiter caps how often anyone can start a machine login, per
source address and per named operator.

## Rules: who may have which secret

The broker decides every request with a **rules file**, `agent-secret-rules.yaml`, which it reads
from S3 in production (`BROKER_RULES_S3_URI`) or from a local file in development
(`BROKER_RULES_FILE`) and rereads every `BROKER_RULES_RELOAD_SECONDS`. A file that does not parse is
refused at startup; a reload that does not parse is logged and the previous rules stay in force.

```yaml
version: 1
secrets:
  DEMO_API_KEY:
    source: example/agent-secrets/DEMO_API_KEY   # where the value lives in the secret store
    owner: ada@example.com                        # who answers for this secret
    delivery: inject                              # put the value in the command's environment
    max_lifetime_seconds: 3600                    # the longest a grant of it lives
    requesters:
      - kind: host
        operator: ada@example.com
        decision: approval
        approver: operator
      - kind: pod
        service_account: system:serviceaccount:legion:legion-worker
        decision: approval
        approver: login:ada@example.com
  DEMO_READ_TOKEN:
    source: example/agent-secrets/DEMO_READ_TOKEN
    owner: ada@example.com
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: host, operator: ada@example.com, decision: automatic}
```

Each secret names its `source`, its `owner`, how it is delivered, the longest a grant of it may
live, and a list of `requesters`. Every field is required; an unknown key is refused.

- **`owner`** names who answers for the secret. The broker requires it and keeps it with the
  secret, but no decision reads it: the requester entries alone decide who gets the secret.
- **`requesters`** is matched against the requesting enrollment. A `box` or `host` entry matches
  enrollments of that kind whose operator is the entry's `operator`; a `pod` entry matches pods
  running as its `service_account`, or every pod when it names none. The broker refuses a file in
  which two entries could match the same caller. An enrollment no entry matches is denied, and an
  empty list denies everyone.
- **`decision`** is the whole of the policy, and there are three: `automatic` grants at once,
  `deny` refuses, and `approval` asks a person. There are no other tiers.
- **`approver`**, on an `approval` entry, names that person: `operator` (the requesting
  enrollment's own operator, so people approve their own agents) or `login:<name>` (a fixed
  Dispatch login). A pod has no operator, so its approval entries use `login:`.
- **`delivery`** is `inject` or `proxy`. The broker releases only `inject` values; it accepts a
  `proxy` secret (which must carry a `proxy` block) but releases no value for it and runs no proxy.

Logins compare case-insensitively everywhere. The rules' SHA-256 is their **version**, recorded on
every request a person approves.

## Requests and grants

A session asks for one or more secrets by name in a **request**, signed with its key and carrying a
reason of at most 400 characters. The broker evaluates every name
(`packages/envoy/internal/broker/requests/machine_state.go`):

- A name no rule mentions refuses the whole request with `UNKNOWN_SECRET`, and nothing is recorded.
- A name the rules deny denies the whole request; nothing is ever half-granted.
- Names that need approval must all need the same approver, else the request is refused
  `MIXED_APPROVERS`; request them separately.
- When every name is automatic, the request is granted at once.
- Otherwise it is **pending**, and the broker writes a credential-request record for the approver to
  decide. An identical request from the same session while one is pending joins it rather than
  asking twice.

A granted request yields a **grant**: the session's right to read those values until the grant
expires. A grant lives the shortest of `BROKER_MAX_GRANT_SECONDS` and each of its secrets'
`max_lifetime_seconds`, counted from the moment it is granted. While it lives, a new request from
the same session for exactly the same names gets the same grant back, without asking anyone again,
as long as the current rules still allow it.

The broker never stores a value. Each time a session reads a grant, the broker checks that the
session is still enrolled, the grant is live, its approval still verifies, and the current rules
still allow every name; then it reads the value from the secret store (AWS Secrets Manager in
production) and returns it to that session alone. `agent-secrets NAME -- command` puts each value
in the command's environment under its name and replaces itself with the command. `agent-secrets`
itself never prints a value, but the command is the agent's choice, and a command can print or send
what its environment holds (`printenv NAME` prints it). A session holding a grant can therefore read
the value: approve a secret only for a session you would trust with the value itself.

A grant ends when it expires, when its session revokes it (`agent-secrets revoke`), when its
approver or its enrollment's operator revokes it in Dispatch (an approved grant) or through the
broker's revoke route, or when its enrollment ends. Ending a grant ends access only to a secret
someone must approve: a secret the rules grant automatically is granted again at the session's next
request. To end access to one, change its rule
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/#end-access-to-an-automatic-secret)).

## Approvals

An approval happens in Dispatch, never in the broker. The broker holds no Dispatch credential and
sends Dispatch nothing: Dispatch's server reads the broker's pending list and records for the
person signed in, and when that person clicks **Approve** or **Deny**, Dispatch's server calls the
broker with the shared UI token (`BROKER_UI_TOKEN`) and that person's Dispatch login, taken from
their Dispatch session. The broker then checks that login against the record's approver: anyone
else is refused `NOT_APPROVER`, whatever state the record is in.

A record is decided once. A second click, a concurrent one, or one after the record expired gets
`RECORD_TERMINAL`. A pending request nobody decides expires after 12 hours, a fixed time rather than
a setting (`agentSecretPendingTTL` in `packages/envoy/cmd/broker/main.go`). Because the UI token
vouches for whoever Dispatch says is approving, it is an approval credential: only Dispatch's
server may hold it.

## The audit record

Everything a person decides rests on a **credential-request record**
(`packages/envoy/internal/broker/record/record.go`): a fixed, line-by-line text holding the
session's signed request verbatim, the approver, the enrollment's kind, runtime id and operator,
the lifetime, the rules version, the expiry, and, for a machine login, its confirmation code. The
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
a session reads a grant, naming the secrets released) and `grant.revoked`. Each row names its actor:
`human:<login>`, `session:<enrollment id>`, `launcher:<credential id>` or `broker`. No record, event
or audit row ever holds a secret value. [Operating the broker](/legion/broker/operate/#the-audit-record)
shows how to read them.
