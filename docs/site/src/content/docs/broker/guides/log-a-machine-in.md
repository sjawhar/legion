---
title: Log a machine in
description: Install the helper on a machine that runs agents, log it in to the broker with a confirmation code, and approve the login in Dispatch.
sidebar:
  order: 11
---

A machine that runs agent sessions directly needs `agent-secrets-helper`, a per-user daemon that
holds each session's key, and a **machine login**: a credential, approved by the machine's
operator (the person whose agents it runs), that lets the helper enroll that operator's sessions.
Legion's daemon logs itself in the same way when it enrolls Kubernetes pods; it prints its code in
its log, and anyone signed in to Dispatch approves it as below.

## 1. Install and start the helper

The helper runs on Linux only. Each `legion-envoy-v*` GitHub release ships
`agent-secrets-amd64.tar.gz` and `agent-secrets-arm64.tar.gz` for Linux, each holding
`agent-secrets/bin/agent-secrets`, `agent-secrets/bin/agent-secrets-helper` and their third-party
licenses in `agent-secrets/THIRD_PARTY_NOTICES`; its macOS archives,
`agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`, hold the CLI alone,
for [managing secrets](/legion/broker/guides/manage-a-secret/). Put both binaries on your `PATH`,
write your Dispatch login to the operator file, give the broker's address to the helper and to
every agent you will start, and run the helper as yourself:

```sh
mkdir -p ~/.config/agent-secrets
echo ada@example.com > ~/.config/agent-secrets/operator
export AGENT_SECRETS_URL=https://secrets.internal.example
agent-secrets-helper serve
```

Put the `export AGENT_SECRETS_URL=…` line in the profile your shells and agents start from, so the
helper and every agent session get it: every `agent-secrets` form that calls the broker needs it.
When a user service manager keeps the helper running (`systemd --user`), set it in the helper's
unit too (`Environment=AGENT_SECRETS_URL=https://secrets.internal.example`).

The helper listens on `$XDG_RUNTIME_DIR/agent-secrets/helper.sock`, where `agent-secrets` finds it
without being told, and logs `agent-secrets-helper listening`. Set `AGENT_SECRETS_HELPER_SOCK` only
to use another socket, and then in the helper's environment and every agent's alike.
`agent-secrets-helper --help` lists its settings.

## 2. Start the login on the machine

```console
$ agent-secrets machine login
machine login code: EAGD-7372
enter it at https://dispatch.example.com/credentials/machine — approve only if the code matches this terminal
```

The command waits for the decision. Set `AGENT_SECRETS_APPROVE_URL` to your Dispatch address to get
the full link; without it, the second line says to enter the code on the Dispatch credential page.
Interrupting it (Ctrl-C) leaves the login pending: run it again and it shows the same code, until
someone approves the login or it expires.

## 3. Approve it in Dispatch

As the operator the login names, open Dispatch's **Enter machine login code** page
(`/credentials/machine`; the Inbox's **Machine login** row links there), type the code, and click
**Look up**. Dispatch shows the machine's host name, the credential's lifetime, and the sentence
"Approving lets `<host>` start agent sessions as you." Approve only if the code is the one your
terminal shows. A machine login can only be selected by its code: no link approves one, and only
the person the login names may approve a person's machine. Once you approve it, the machine is
listed under **Machine logins** on the same page, where you can
[revoke its login](/legion/broker/guides/revoke-a-session/#end-a-machines-login); from your own
shell on a logged-in machine, `agent-secrets machine list` lists your own machines' logins among
them.

A Legion daemon's login names its service, so anyone signed in to Dispatch may approve it: it waits
in every signed-in person's Inbox, and whoever has the code from the daemon's log types it in. The
sentence reads "Approving lets `legion-daemon on <host>` start worker pods as `legion-daemon`, not
as you: no secret of yours reaches its pods unless you approve the request for it." A pod has no
operator, so its requests for your secrets come to you for approval. When the broker binds
`legion-daemon` to the service account its pods run as (`BROKER_SERVICES`), each of its pods
running as that account gets every secret `legion-daemon` owns at once, with no further approval
([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)). Approve a
service's login only if the code matches the one its service printed. The login is listed under
**Machine logins** as `legion-daemon on <host>` for everyone signed in, with who approved it, and
anyone's revoke ends every pod it enrolled.

Back on the machine, `agent-secrets machine login` exits 0 and the helper logs
`machine login issued; the helper holds a launcher credential`: the machine credential the approval
minted, which the helper now enrolls sessions with. Check it at any time; it also says when the
credential expires, since the broker has no renewal:

```console
$ agent-secrets machine login-status
issued
agent-secrets machine login-status: the launcher credential expires at 2026-10-10T03:21:40Z (in 6d23h59m); the broker has no renewal, so a new machine login a human approves must replace it before then
```

## 4. Start agents as sessions

Start each agent through `agent-secrets register`, which makes the agent's process, and everything
it starts, one session the helper enrolls with the broker:

```sh
agent-secrets register --wait 10 --exec -- <agent>
```

`<agent>` is the command that starts your agent (`bash` works for trying it out). `register`
replaces itself with that command and passes its own environment on unchanged, so the agent's
environment needs `AGENT_SECRETS_URL` (step 1's profile line gives it), and
`AGENT_SECRETS_HELPER_SOCK` when the helper uses a socket other than the default.

`--wait 10` gives the helper up to 10 seconds to enroll the session before the agent starts. Without
a machine login it starts the agent anyway, with a warning that the session's `agent-secrets` calls
fail until the machine is logged in.

An agent in a container instead holds its own key: [run an agent in a
container](/legion/broker/guides/run-an-agent-in-a-container/) shows how the helper enrolls it.

## When to log in again

The credential lasts `BROKER_LAUNCHER_CREDENTIAL_SECONDS` (the
[configuration reference](/legion/broker/reference/config/) gives its default), and the helper keeps
it only in memory, so log in again after it expires, after the helper restarts, or after you revoke
the machine's login.
`agent-secrets machine login-status` exits 1 and says why when the machine needs it:

```console
$ agent-secrets machine login-status
none
agent-secrets machine login-status: no machine login since the helper started; a restart discards the launcher credential; run: agent-secrets machine login
```

A login nobody approves [expires](/legion/broker/concepts/#machine-login); start a new one. When
the credential reaches its expiry, or the broker refuses it (as it does once the login is revoked),
the helper drops it and logs why at ERROR, and `login-status` prints `expired` with that reason.
