---
title: Quickstart
description: An agent session uses a secret with one command; the approver sees the request in Dispatch; the command runs with the value in its environment.
sidebar:
  order: 1
---

This page follows one secret from the agent that asks for it, through the person who approves it,
back to the command that uses it. It assumes the machine is already logged in to the broker and the
agent session is registered; on a machine you set up yourself, [log the machine
in](/legion/broker/guides/log-a-machine-in/) first and start each agent with
`agent-secrets register --wait 10 --exec -- <agent>`.

No broker yet? [Run the local stack](/legion/broker/guides/run-locally/) and log its helper in, as
that page shows. It has no Dispatch: where step 2 says Dispatch, approve with
`agent-secrets-devrelay`, as [approving without
Dispatch](/legion/broker/guides/approve-a-request/#without-dispatch) shows. Every terminal output
below is real, captured from that stack, whose rules grant `DEMO_READ_TOKEN` automatically and need
`ada@example.com` to approve `DEMO_API_KEY`.

## 1. The agent asks

An agent names the secrets a command needs and lets `agent-secrets` run the command with them in
its environment. `agent-secrets` itself never prints a value, but the command is the agent's choice
and can (the `printenv` below does), so a session holding a grant can read the value: approve a
secret only for a session you would trust with the value itself.

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
```

- `DEMO_API_KEY` is the secret's name in the broker's rules, and the name of the environment
  variable the command gets it in. Name several to get several.
- `--reason` is what the approver reads. Say what the command is for.
- Everything after `--` is the command.

When the rules grant a secret automatically, the command runs at once:

```console
$ agent-secrets DEMO_READ_TOKEN -- printenv DEMO_READ_TOKEN
demo-read-token-value
```

When they need a person's approval, `agent-secrets` waits (up to 30 minutes; set `--wait` to a
duration such as `5m` to change that) while the approver decides.

## 2. The approver decides in Dispatch

The approver the rules name (here `ada@example.com`, the operator of the machine the agent runs on)
finds the request at the top of their Dispatch **Inbox**, under **Credential requests**: a
**Secret request** row naming `DEMO_API_KEY` and when it was asked. Opening it shows the request's
page:

| Field | What it shows |
| --- | --- |
| Kind | Secret request |
| Identifiers | The secrets asked for: `DEMO_API_KEY` |
| Enrollment | The session's kind, runtime and operator: `host · example-host-build:2150654:335907311 · ada@example.com` |
| Lifetime | How long a grant would last: 1 hour |
| Requested, Expires | When it was asked, and when it [expires](/legion/broker/concepts/#approvals) undecided |
| Rules version | The SHA-256 of the rules that decided it needs approval |
| Approver | `ada@example.com` |
| The agent's stated reason | Deploy the example service |

and two buttons, **Approve** and **Deny**. The broker records the decision under the login the
approver is signed in to Dispatch as; [approving a request](/legion/broker/guides/approve-a-request/)
covers the page in full.

## 3. The command runs

Once approved, the waiting `agent-secrets` collects the grant and replaces itself with the command,
`DEMO_API_KEY` set in its environment. This command prints only the value's length:

```console
$ agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- sh -c 'echo "deploying with a ${#DEMO_API_KEY}-character key"'
deploying with a 18-character key
```

The grant outlives the command. Until it expires, the same session asking for exactly the same
secrets gets it again without asking anyone; `agent-secrets self` lists the session's live grants:

```console
$ agent-secrets self
enrollment_id: 2b8627f4-9a2a-4c12-85bf-910144bc0b5c
kind: host
operator: ada@example.com
lease_expires_at: 2026-10-03T03:36:40Z
grant: f5167d29-1789-4719-8e8c-9aec6f1229e7 (request ce11549a-ab03-4a13-95cf-896961c36fc0, expires 2026-10-03T04:21:47Z)
grant: be34ed2a-78e2-4457-9a75-71283a87c16b (request 7db349e6-ad3c-4f40-86d7-c6a38e899306, expires 2026-10-03T04:22:04Z)
```

## When the answer is no

From a session with no live grant of `DEMO_API_KEY` (a new session, or one that ran
`agent-secrets revoke <grant id>`), the same command asks again. When the approver clicks **Deny**
(on the local stack, `agent-secrets-devrelay deny --record <record id>`), nothing runs and
`agent-secrets` exits 77:

```console
$ agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
agent-secrets: request 2375f92d-bab0-4123-a0cb-d139c98de73c was denied
```

A request still undecided when `--wait` runs out exits 75 and runs nothing; a name the rules do not
know is refused at once:

```console
$ agent-secrets NO_SUCH_SECRET -- true
agent-secrets: no rule names this secret (UNKNOWN_SECRET)
```

[Troubleshooting](/legion/broker/guides/troubleshooting/) lists every refusal and what to do about
it, and [Concepts](/legion/broker/concepts/) explains sessions, rules and grants.
