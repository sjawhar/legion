---
title: Log a machine in
description: Install the helper on a machine that runs agents, log it in to the broker with a confirmation code, and approve the login in Dispatch.
---

A machine that runs agent sessions directly needs `agent-secrets-helper`, a per-user daemon that
holds each session's key, and a **machine login**: a credential, approved by the machine's
operator, that lets the helper enroll that operator's sessions. Legion's daemon logs itself in the
same way when it enrolls Kubernetes pods; it prints its code in its log, and you approve it as
below.

## 1. Install and start the helper

Each `legion-envoy-v*` GitHub release ships `agent-secrets-amd64.tar.gz` and
`agent-secrets-arm64.tar.gz`, each holding `agent-secrets/bin/agent-secrets` and
`agent-secrets/bin/agent-secrets-helper`. Put both on your `PATH`, write your Dispatch login to
the operator file, and run the helper as yourself (a user service manager such as `systemd --user`
keeps it running):

```sh
mkdir -p ~/.config/agent-secrets
echo ada@example.com > ~/.config/agent-secrets/operator
AGENT_SECRETS_URL=https://secrets.internal.example agent-secrets-helper serve
```

The helper listens on `$XDG_RUNTIME_DIR/agent-secrets/helper.sock` and logs
`agent-secrets-helper listening`. `agent-secrets-helper --help` lists its settings.

## 2. Start the login on the machine

```console
$ agent-secrets launcher login
machine login code: EAGD-7372
enter it at https://dispatch.example.com/credentials/machine — approve only if the code matches this terminal
```

The command waits for the decision. Set `AGENT_SECRETS_APPROVE_URL` to your Dispatch address to get
the full link; without it, the second line says to enter the code on the Dispatch credential page.

## 3. Approve it in Dispatch

As the operator the login names, open Dispatch's **Enter machine login code** page
(`/credentials/machine`; the Inbox's **Machine login** row links there), type the code, and click
**Look up**. Dispatch shows the machine's host name, the credential's lifetime, and the sentence
"Approving lets `<host>` start agent sessions as you." Approve only if the code is the one your
terminal shows. A machine login can only be selected by its code: no link approves one.

Back on the machine, `agent-secrets launcher login` exits 0 and the helper logs
`machine login issued; the helper holds a launcher credential`. Check it at any time:

```console
$ agent-secrets launcher login-status
issued
```

## 4. Start agents as sessions

Start each agent through `agent-secrets register`, which makes the agent's process, and everything
it starts, one session the helper enrolls with the broker:

```sh
agent-secrets register --wait 10 --exec -- omp
```

`--wait 10` gives the helper up to 10 seconds to enroll the session before the agent starts. Without
a machine login it starts the agent anyway, with a warning that the session's `agent-secrets` calls
fail until the machine is logged in.

## When to log in again

The credential lasts 7 days by default (`BROKER_LAUNCHER_CREDENTIAL_SECONDS`), and the helper keeps
it only in memory, so log in again after it expires or after the helper restarts.
`agent-secrets launcher login-status` exits 1 and says why when the machine needs it:

```console
$ agent-secrets launcher login-status
none
agent-secrets launcher login-status: no machine login has run on this helper; run: agent-secrets launcher login
```

A login nobody approves expires after 15 minutes; start a new one.
