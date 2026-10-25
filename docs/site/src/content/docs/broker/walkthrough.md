---
title: Walkthrough
description: One secret request from start to finish, with a still of each page - an agent asks the secrets broker for an API key, a person approves it on the broker's own page, and the agent's command runs with it.
sidebar:
  order: 5
---

Before you start, the agent's machine needs the helper installed and running
(`agent-secrets-helper install --url <broker address>`), with its operator (the person who approves
for the machine) named in `~/.config/agent-secrets/operator`, and `AGENT_SECRETS_URL` set to the
broker's address in the shells agents start
from. The machine also sets `AGENT_SECRETS_APPROVE_URL` to Dispatch's address, which makes
`agent-secrets machine login` print Dispatch's machine-login page to open.
[Log a machine in](/legion/broker/guides/log-a-machine-in/) sets all of this up.

A tour of one secret request, on example data: the Dispatch workspace's person is
`alice@example.com`, the agent's machine is `example-host-build`, and the secret, `DEMO_API_KEY`,
holds a made-up value. The narrated recording of this tour is being remade for the broker's own
approval page, so this page shows no video until it is; the steps below are the current ones, with
a still of each page.

## 1. Log the machine in, once

On the agent's machine, `agent-secrets machine login` prints a code and the address of Dispatch's
machine-login page, `/credentials/machine`. Alice opens that page, types the code, and checks the
record it finds: a machine login for `example-host-build`, with her as its approver. She approves
it, the page lists it under **Machine logins**, where **Revoke** would end it and every session
it started ([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)),
and the login on the machine returns. `agent-secrets machine login-status` checks the machine
login at any time: it prints `issued` while the machine holds one, and says when that login
lapses. The helper refreshes it every day, and resumes it after a restart, so the machine logs in
once.

![The machine login page with a code looked up: a machine login for example-host-build, approver alice@example.com, with Approve and Deny buttons, and below them the machine logins list, empty until she approves](/legion/media/broker/machine-login.png)

## 2. Start a session

`agent-secrets register --wait 10 --exec -- bash` registers a session with the machine's helper,
as an agent's session is registered when it starts, and runs a shell in it. `agent-secrets self`
shows the session's enrollment and its operator, `alice@example.com`.

## 3. Ask for the secret

The session asks for the secret to run one command, and says why:

```bash
agent-secrets DEMO_API_KEY --reason "Publish the docs preview for PR 42 with the demo API" -- ./check-demo-key.sh
```

`DEMO_API_KEY` is alice's own secret at the human tier (its tags are `owner=alice@example.com` and
`tier=human`), so a request for it waits for her approval, even from her own machine: the command
waits, and names who approves it, alice, and the request's page on the secrets broker, where she
decides it.

## 4. Approve it

The request is at the top of alice's Inbox, under **Credential requests**, and counts toward
**Needs you** beside the sidebar's Inbox. The row is a link to the request's page on the secrets
broker. The stills in this step come from Dispatch's browser test harness, whose request is a pod's
request for alice's secret `ALICE_DEPLOY_KEY`; the page is the same for any request.

![The Inbox, with a secret request for ALICE_DEPLOY_KEY under Credential requests, Needs you 1 beside Inbox in the sidebar, and Blocked on you: 1 item](/legion/media/broker/inbox-credential-request.png)

The link opens the request's page on the broker. Alice signed in to the broker earlier that day, so
it opens at once; otherwise the broker would have her sign in with her Workspace account first and
bring her back here. The page prints, from the broker's own record, what was asked for, by which
enrollment, why, for how long, who may approve it and its state. The request named no `--ttl`, so
its grant lasts until the session ends.

![The secrets broker's page Approve a secret request, signed in as alice@example.com: the secret ALICE_DEPLOY_KEY, requested by a pod enrollment the broker verified, its reason, lasting until the session ends, its requested and expiry times, approver alice@example.com, state pending, and Approve and Deny buttons](/legion/media/broker/credential-request.png)

She clicks **Approve** on the broker's page, and the page says what became of the request.

![The same page after approval: What happened reads ALICE_DEPLOY_KEY granted, approved by alice@example.com, and the request's state reads approved by alice@example.com](/legion/media/broker/credential-request-approved.png)

## 5. The command runs

The waiting command checks the broker every few seconds, so it carries on within a few seconds of
the approval (10 at most): it receives `DEMO_API_KEY` in its environment and runs. The demo command
prints the key's length and last four characters to show it arrived. `agent-secrets status
<request>` names who decided the request.

## 6. The live grant

**Settings** lists the live grants of alice's sessions, whether granted automatically or approved,
and those she approved on anyone's session, each with its session, how it was granted and when it
expires: this one when the session ends. **Revoke** ends a grant at once; a command already running
keeps the value it was given.

![Live grants in Settings: the example-host-build session holding DEMO_API_KEY, approved by alice@example.com, expiring when the session ends, with a Revoke button](/legion/media/broker/live-grants.png)

The rig that records the screenshots and the narrated recording, and the scripts that record them
again, are in [`docs/site/media/broker/`](https://github.com/sjawhar/legion/tree/main/docs/site/media/broker).
[The Secrets Broker's introduction](/legion/broker/) says what the broker is for.
