---
title: Approve a request
description: Find a pending secret request in Dispatch, check what it asks for, and approve or deny it.
sidebar:
  order: 10
---

When an agent asks for a secret a person must approve, that person decides it in Dispatch. Only the
approver the request names can decide it: the secret's owner, for a person's secret, or anyone
signed in to Dispatch, for a shared human-tier secret, whose requests show the approver `anyone`
and wait in every person's Inbox. [Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)
explains which requests need whom.

## Find it

Pending requests you decide appear at the top of your Dispatch **Inbox**, under **Credential
requests**, one row per request: **Secret request** with the secrets' names, or **Machine login**
with a machine's host name ([log a machine in](/legion/broker/guides/log-a-machine-in/) covers
those). The section is absent when nothing waits on you, and hidden entirely when the deployment
has no broker connected.

## Check it

Opening a **Secret request** shows the broker's record of it:

- **Identifiers**: the secrets asked for. A grant covers all of them or none.
- **Enrollment**: the session asking, as kind, runtime id and operator. A `host` runtime id starts
  with the machine's host name; a `pod` enrollment also shows its **Worker slot**.
- **Lifetime**: how long the grant would last from the moment you approve.
- **Requested** and **Expires**: when it was made, and when it
  [expires](/legion/broker/concepts/#approvals) if nobody decides it.
- **Rules version**: the version of the secret policy the request was decided under.
- **Approver**: who may decide it: an email, or `anyone`.
- **The agent's stated reason**: the `--reason` the agent gave, shown as plain text.

Approve only what the reason and the session justify. The agent's command is waiting; a denial runs
nothing.

## Decide it

Click **Approve** or **Deny**. Dispatch sends the broker your decision with the login you are
signed in as, and the broker records it on the request's record: the agent's waiting command runs,
or exits 77.

The broker refuses a decision that cannot stand, and Dispatch shows its message under the buttons:

| Message | Why |
| --- | --- |
| `only the record's approver may decide it` (`NOT_APPROVER`) | You are not the approver this request names. A request whose approver is `anyone` is refused to the login `anyone` and to no one else. |
| `request is already decided` (`RECORD_TERMINAL`) | Someone, or another tab, decided it first, or the agent's session ended and withdrew it. |
| `request expired before its approver acted on it` (`RECORD_TERMINAL`) | It waited past its expiry. The agent asks again if it still needs the secret. |
| `this grant's approval chain no longer verifies` (`GRANT_CHAIN_INVALID`) | The stored request no longer matches its own signature; nothing was granted. Tell whoever runs the broker. |

## Without Dispatch

On the [local stack](/legion/broker/guides/run-locally/), `agent-secrets-devrelay` stands in for
Dispatch: it calls the same broker routes, with the same UI token and an approver's login.

```console
$ curl -s -H "Authorization: Bearer $AGENT_SECRETS_UI_TOKEN" "$AGENT_SECRETS_URL/v1/pending?approver=$AGENT_SECRETS_APPROVER" | jq .
{
  "pending": [
    {
      "record_id": "7b1a96bcccee227f5774b29f4f9d947d4ee32a3284ae8030f2ada3aa8954961b",
      "kind": "agent_secret",
      "identifiers": [
        "DEMO_API_KEY"
      ],
      "requested_at": "2026-10-03T03:21:49.962111Z"
    }
  ]
}
$ agent-secrets-devrelay approve --record 7b1a96bcccee227f5774b29f4f9d947d4ee32a3284ae8030f2ada3aa8954961b
{"credential_id":null,"grant_id":"be34ed2a-78e2-4457-9a75-71283a87c16b","state":"approved"}
```

`agent-secrets-devrelay deny --record <id>` denies one. The [HTTP API
reference](/legion/broker/reference/api/) documents these routes.

A request has two ids. The request id is the one `agent-secrets` prints and takes (`self`,
`status`, `cancel`, and the exit-75 message); the record id is the one the approver's list shows
and `agent-secrets-devrelay` takes. `agent-secrets request NAME --json` prints both.
