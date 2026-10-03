---
title: Quickstart
description: An agent session uses a secret with one command; the approver sees the request in Dispatch; the command runs with the value in its environment.
---

This page follows one secret from the agent that asks for it, through the person who approves it,
back to the command that uses it. It assumes the machine is already logged in to the broker and the
agent session is registered; on a machine you set up yourself, [log the machine
in](/legion/broker/guides/log-a-machine-in/) first and start each agent with
`agent-secrets register --wait 10 --exec -- <agent>`. Every output below is real, captured from
the [local stack](/legion/broker/guides/run-locally/), whose rules grant `DEMO_READ_TOKEN`
automatically and need `ada@example.com` to approve `DEMO_API_KEY`.

## 1. The agent asks

An agent never handles a secret's value: it names the secrets a command needs and lets
`agent-secrets` run the command with them.

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

When they need a person's approval, `agent-secrets` waits (up to 30 minutes; set `--wait` to change
that) while the approver decides.

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
| Requested, Expires | When it was asked, and when it expires undecided (12 hours later) |
| Rules version | The SHA-256 of the rules that decided it needs approval |
| Approver | `ada@example.com` |
| The agent's stated reason | Deploy the example service |

and two buttons, **Approve** and **Deny**. The broker records the decision under the login the
approver is signed in to Dispatch as; [approving a request](/legion/broker/guides/approve-a-request/)
covers the page in full.

## 3. The command runs

Once approved, the waiting `agent-secrets` collects the grant and replaces itself with the command,
`DEMO_API_KEY` set in its environment. Nothing prints the value:

```console
$ agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- sh -c 'echo "deploying with a ${#DEMO_API_KEY}-character key"'
deploying with a 18-character key
```

The grant outlives the command. Until it expires, the same session asking for exactly the same
secrets gets it again without asking anyone; `agent-secrets self` lists the session's live grants:

```console
$ agent-secrets self
enrollment_id: 5a60201a-2c18-488e-9e67-10b3e41390e3
kind: host
operator: ada@example.com
lease_expires_at: 2026-10-03T02:21:40Z
grant: 95c3597a-c6d9-42a5-818b-f8fea438f502 (request fb85d9d7-8dfb-4571-8bfe-0674a7f093c0, expires 2026-10-03T03:06:41Z)
grant: e2123cec-576a-4dbe-aba5-f17b8cb57701 (request d92c775b-b69b-4775-a7e6-4fce079e3b0c, expires 2026-10-03T03:06:41Z)
```

## When the answer is no

A denied request runs nothing, and `agent-secrets` exits 77:

```console
$ agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
agent-secrets: request 15c26ea4-d896-4bcf-8092-ad253194185c was denied
```

A request still undecided when `--wait` runs out exits 75 and runs nothing; a name the rules do not
know is refused at once:

```console
$ agent-secrets NO_SUCH_SECRET -- true
agent-secrets: no rule names this secret (UNKNOWN_SECRET)
```

[Troubleshooting](/legion/broker/guides/troubleshooting/) lists every refusal and what to do about
it, and [Concepts](/legion/broker/concepts/) explains sessions, rules and grants.
