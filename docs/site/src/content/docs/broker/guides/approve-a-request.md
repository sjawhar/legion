---
title: Approve a request
description: Find a pending secret request in your Dispatch Inbox, open its page on the secrets broker, check what it asks for, and approve or deny it there.
sidebar:
  order: 10
---

When an agent asks for a secret a person must approve, that person decides it on the secrets
broker's own page, signed in to the broker. Only the approver the request names can decide it: the
secret's owner, for a person's secret, or anyone who signs in to the broker, for a shared secret. A
shared secret's request waits in every person's Inbox at once, and the first person to decide it
decides it for everyone: it then leaves every Inbox.
[Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) explains which
requests need whom.

## Find it

Pending requests you decide appear at the top of your Dispatch **Inbox**, under **Credential
requests**, one row per request: **Secret request** with the secrets' names, or **Machine login**
with a machine's host name ([log a machine in](/legion/broker/guides/log-a-machine-in/) covers
those). The section is absent when nothing waits on you, and hidden entirely when the deployment
has no broker connected. The agent's own output names the same page: `waiting for <you> to approve
it on the secrets broker: <url>`.

A **Secret request** row is a link to the request's page on the broker,
`<broker>/approve/<record>`. Click it. When you signed in to the broker within the last day (a
sign-in lasts a day unless the deployment set it shorter) the page opens at once. Otherwise the
broker sends you to sign in with your Workspace account, which asks nothing when you are already
signed in to it, and brings you back to the same request. A row that reads **Decided on the secrets
broker.** and links nowhere comes from a broker older than that page; ask whoever runs it to
upgrade it.

## Check it

The page prints the broker's own record of the request, not anything Dispatch or the agent passed
along:

- **Secrets**: the secrets asked for. A grant covers all of them or none.
- **Requested by (verified by the broker)**: the enrollment asking, as kind, operator and runtime
  id. A `session` started on a machine has a runtime id that starts with the machine's host name; a
  `pod` enrollment also shows its worker slot when it has one.
- **Session the request names (its own unsigned claim, not verified)**: shown only when the request
  names a session other than its enrollment's. The enrollment above is the one the broker checked.
- **Reason**: the `--reason` the agent gave, shown as plain text.
- **Lasts once approved**: how long the grant would last from the moment you approve: the `--ttl`
  the agent asked for, or **until the session ends**.
- **Requested** and **Expires**: when it was made, and when it
  [expires](/legion/broker/concepts/#approvals) if nobody decides it.
- **Approver**: who may decide it: an email, or anyone signed in for a shared secret.
- **State**: pending, or how it was decided, by whom and when.

The page header names who you are signed in as. Signed in as someone the request does not name,
the page says who may decide it and offers no buttons, only **Sign in as someone else**, which
sends you through the sign-in again with an account chooser and back to the same request.

Approve only what the reason and the session justify. The agent's command is waiting; a denial runs
nothing.

## Decide it

Click **Approve** or **Deny**. The broker records the decision under the email you signed in to it
as, so a shared secret's record names whoever decided it, and the agent's waiting command runs, or
exits 77. Every other request waiting on you is listed below the first, each with its own buttons,
and **Approve all N shown** approves every request the page printed, and only those.

The page then shows, under **What happened**, what became of each request it decided:

| Line | Why |
| --- | --- |
| `granted (approved by <you> at <time>)` | The grant is made; the agent's command collects it. |
| `denied by <you> at <time>` | You denied it; the agent's command exits 77. |
| `recorded as denied because the requesting session ended` | You approved it, but the session that asked had ended, so nothing was granted. |
| `already decided: …` | Someone, or another tab, decided it first, or the agent's session ended and withdrew it (`cancelled`). |
| `expired before anyone decided it` | It waited past its expiry. The agent asks again if it still needs the secret. |
| `not yours to decide; only <approver> may` | The request names someone else. |
| `still pending; it was not decided` | The broker refused your approval though the request names you: the secret's tags now name someone else to approve it ([what an owner change does](/legion/broker/concepts/#approvals)), the session's operator withheld one of its secrets, which only that secret's owner may then approve, and no one while the broker does not serve that secret to that session (it is out of the policy, or a service's), or the secret's `max-grant` became shorter than the lifetime the request asked for, which no one may then approve. You can still deny it. |

A decision is accepted only from the page itself, within an hour of opening it, while your sign-in
lasts, and only for the requests that page printed: a page more than an hour old, or a form from
anywhere else, decides nothing and says so. Open the request again. Dispatch cannot decide a secret
request for you: its Approve, from a tab opened before its Inbox linked this page, gets the broker's
`DECIDE_ON_BROKER_PAGE` refusal naming the page.

## Without Dispatch

On the [local stack](/legion/broker/guides/run-locally/) there is no Dispatch Inbox: open the page
`agent-secrets` prints in a browser on the same machine. The stack's stand-in identity provider
signs you in as the request's approver with no page to fill in, and the broker brings you back to
the request. `agent-secrets-devrelay` stands in for Dispatch only for a machine login
(`machine-approve`, with its code): the broker refuses every route a service calls to decide a
secret request.

The list Dispatch reads names each request's page as `approve_url`:

```sh
curl -s -H "Authorization: Bearer $(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")" \
  "$AGENT_SECRETS_URL/v1/pending?approver=$AGENT_SECRETS_APPROVER" | jq '.pending[] | {record_id, kind, approve_url}'
```

The [HTTP API reference](/legion/broker/reference/api/) documents the page's routes and that list.

A request has two ids. The request id is the one `agent-secrets` prints and takes (`self`,
`status`, `cancel`, and the exit-75 message); the record id is the one the approver's page and list
name, the last part of `approve_url`. `agent-secrets request NAME --json` prints both.
