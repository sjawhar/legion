---
title: Walkthrough
description: A narrated video of one secret request from start to finish - an agent asks the secrets broker for an API key, a person approves it in Dispatch, and the agent's command runs with it.
sidebar:
  order: 5
---

Before you start, the agent's machine needs `agent-secrets-helper serve` running, with its operator
(the person who approves for the machine) named in `~/.config/agent-secrets/operator`, and
`AGENT_SECRETS_URL` set to the broker's address, for the helper and in the shells agents start
from. The video's machine also sets `AGENT_SECRETS_APPROVE_URL` to Dispatch's address, which makes
the commands below print the Dispatch page to open.
[Log a machine in](/legion/broker/guides/log-a-machine-in/) sets all of this up.

<video controls preload="metadata" poster="/legion/media/broker/walkthrough.jpg" style="width: 100%" aria-label="Walkthrough: an agent requests a secret and a person approves it in Dispatch">
  <source src="/legion/media/broker/walkthrough.mp4" type="video/mp4">
  <track kind="captions" src="/legion/media/broker/walkthrough.vtt" srclang="en" label="English" default>
</video>

A narrated tour of one secret request, recorded on example data: the Dispatch workspace's person is
`alice`, the agent's machine is `example-host-build`, and the secret, `DEMO_API_KEY`, holds a
made-up value. The steps below are the ones the video shows, with stills of each Dispatch page.

## 1. Log the machine in, once

On the agent's machine, `agent-secrets launcher login` prints a code and the address of Dispatch's
machine-login page, `/credentials/machine`. Alice opens that page, types the code, and checks the
record it finds: a machine login for `example-host-build`, with her as its approver. She approves
it, and the login on the machine returns. `agent-secrets launcher login-status` checks the machine
login at any time: it prints `issued` while the machine holds one, and says when that login
expires. The broker does not renew it, so before then a person must approve a new machine login.

![The machine login page with a code looked up: a machine login for example-host-build, approver alice, with Approve and Deny buttons](/legion/media/broker/machine-login.png)

## 2. Start a session

`agent-secrets register --wait 10 --exec -- bash` registers a session with the machine's helper,
as an agent's session is registered when it starts, and runs a shell in it. `agent-secrets self`
shows the session's enrollment and its operator, `alice`.

## 3. Ask for the secret

The session asks for the secret to run one command, and says why:

```bash
agent-secrets DEMO_API_KEY --reason "Publish the docs preview for PR 42 with the demo API" -- ./check-demo-key.sh
```

The demo's rules send a request for `DEMO_API_KEY` to the machine's operator for approval, so the
command waits, and prints the Dispatch page where the request is decided.

## 4. Approve it

The request is at the top of alice's Inbox, under **Credential requests**, and counts toward
**Needs you** beside the sidebar's Inbox.

![The Inbox, with a secret request for DEMO_API_KEY under Credential requests, Needs you 1 beside Inbox in the sidebar, and Blocked on you: 1 item](/legion/media/broker/inbox-credential-request.png)

Its page shows what was asked for, by which session, for how long, who may approve it, and the
agent's stated reason.

![A credential request for DEMO_API_KEY: the enrollment on example-host-build, a one-hour lifetime, approver alice, the agent's stated reason, and Approve and Deny buttons](/legion/media/broker/credential-request.png)

She approves it, and the page records the decision.

![The same request after approval, reading Approved just now](/legion/media/broker/credential-request-approved.png)

## 5. The command runs

The waiting command checks the broker every few seconds, so it carries on within a few seconds of
the approval (10 at most): it receives `DEMO_API_KEY` in its environment and runs. The video
shortens that wait, which took 8 seconds in the recording. The demo command prints the key's length
and last four characters to show it arrived. `agent-secrets status <request>` names who decided the
request.

## 6. The live grant

**Settings** lists the live grants alice approved, and those on sessions she operates, each with
its session, its approver and when it expires. **Revoke** ends a grant at once; a command already
running keeps the value it was given.

![Live grants in Settings: the example-host-build enrollment holding DEMO_API_KEY, approver alice, with a Revoke button](/legion/media/broker/live-grants.png)

The rig that recorded the video and the screenshots, and the scripts that record them again, are in
[`docs/site/media/broker/`](https://github.com/sjawhar/legion/tree/main/docs/site/media/broker).
[The Secrets Broker's introduction](/legion/broker/) says what the broker is for.
