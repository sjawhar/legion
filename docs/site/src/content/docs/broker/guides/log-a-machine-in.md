---
title: Log a machine in
description: Install the helper on a machine that runs agents, log it in to the broker with a confirmation code, and approve the login in Dispatch.
sidebar:
  order: 11
---

A machine that runs agent sessions directly needs `agent-secrets-helper`, a per-user daemon that
holds each session's key, and a **machine login**: a credential, approved by the machine's
operator (the person whose agents it runs), that lets the helper enroll that operator's sessions.
The Legion daemon logs itself in with its own Kubernetes ServiceAccount token instead, which no
person approves.

## 1. Install and start the helper

The helper runs on Linux only. Each `legion-envoy-v*` GitHub release ships
`agent-secrets-amd64.tar.gz` and `agent-secrets-arm64.tar.gz` for Linux, each holding
`agent-secrets/bin/agent-secrets`, `agent-secrets/bin/agent-secrets-helper` and their third-party
licenses in `agent-secrets/THIRD_PARTY_NOTICES`; its macOS archives,
`agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`, hold the CLI alone,
for [managing secrets](/legion/broker/guides/manage-a-secret/). Put both binaries on your `PATH`,
write your Dispatch login to the operator file, and install the helper as a `systemd --user`
service with the broker's address:

```sh
mkdir -p ~/.config/agent-secrets
echo ada@example.com > ~/.config/agent-secrets/operator
agent-secrets-helper install --url https://secrets.internal.example
```

`install` writes `~/.config/systemd/user/agent-secrets-helper.service` (under `$XDG_CONFIG_HOME`
when that is set), which runs this binary's `serve` with `AGENT_SECRETS_URL` set to `--url` and
restarts it whenever it exits, then reloads systemd and enables and starts the unit. Running it
again with the same binary path and URL while the unit runs changes nothing, so binaries upgraded
in place need `--restart-with-sessions` (or `systemctl --user restart agent-secrets-helper`) to
take effect; an unchanged unit that is not running is started. A changed unit restarts a running
helper only when no session is registered with it, since a restart ends the sessions' grants.
With sessions registered it lists them and leaves the running helper as it is:

```console
$ agent-secrets-helper install --url https://secrets.internal.example
2881325	example-host-devbox:2881325:39550474	91964d34-8269-4679-a870-d48403369076	enrolled
not restarted: 1 sessions are registered; a restart ends their grants. Re-run with --restart-with-sessions.
```

The unit is written by then, so a later run without the flag changes nothing: once those sessions
end, or to restart it anyway, re-run with `--restart-with-sessions`, which restarts the running
helper whether or not the unit changed. A
helper with no sessions refuses new ones from the moment `install` checks until it restarts, so
none starts in between. A running helper from a release before that refusal (`drain`) cannot
hold new sessions off, so `install` never restarts it on its own: it lists its sessions and
prints `not restarted: the running agent-secrets-helper predates drain, …`. Re-run with
`--restart-with-sessions` at a quiet moment. If the restart itself fails, `install` lets the
helper take sessions again and exits with systemd's error.

A helper that answers on the socket but that the unit does not run (one started by hand) makes
`install` stop before it writes anything, even when the unit file already matches: it lists that helper's sessions and exits 2 with `an
agent-secrets-helper not run by agent-secrets-helper.service answers at …; stop it, then re-run
install`, since starting the unit would put a second helper on the socket.

The unit carries `AGENT_SECRETS_URL` and each of `AGENT_SECRETS_HELPER_SOCK`,
`AGENT_SECRETS_OPERATOR_FILE` and `AGENT_SECRETS_MACHINE_FILE` that is set where you run
`install`, so run it with the same exports your shells use. One helper holds a machine file at a
time: a second `serve` on the same file, whatever its socket, exits naming the file.

Put `export AGENT_SECRETS_URL=https://secrets.internal.example` in the profile your shells and
agents start from, so every agent session gets it: every `agent-secrets` form that calls the
broker needs it.

The helper listens on `$XDG_RUNTIME_DIR/agent-secrets/helper.sock`, where `agent-secrets` finds it
without being told, and logs `agent-secrets-helper listening` (`journalctl --user -u
agent-secrets-helper` shows its log). Set `AGENT_SECRETS_HELPER_SOCK` only to use another socket,
and then in the helper's environment and every agent's alike. `agent-secrets-helper --help` lists
its settings.

## 2. Start the login on the machine

```console
$ agent-secrets machine login
machine login code: EAGD-7372
enter it at https://dispatch.example.com/credentials/machine — approve only if the code matches this terminal
```

The command waits for the decision. The second line names the page the broker is configured with
(`BROKER_MACHINE_LOGIN_URL`); set `AGENT_SECRETS_APPROVE_URL` to your Dispatch address to name
`<that address>/credentials/machine` instead. A helper from before it relayed the broker's page
says to enter the code on the Dispatch credential page.
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

A Legion daemon does not log in this way: it logs in with its own Kubernetes ServiceAccount token,
which no person approves ([Concepts](/legion/broker/concepts/#machine-login)), so no daemon login
waits on this page. A pending login that names a service is one a broker opened before services
logged in with their tokens; the page says no one can approve it, and you may deny it or let it
expire. A pod has no operator, so its requests for your secrets come to you for approval. When the
broker binds `legion-daemon` to the service account its pods run as (`BROKER_SERVICES`), each of
its pods running as that account gets every secret `legion-daemon` owns at once, with no further
approval ([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)).

Back on the machine, `agent-secrets machine login` exits 0 and the helper logs
`machine login issued; the helper holds a launcher credential`: the machine credential the approval
minted, which the helper now enrolls sessions with. The helper writes the login to
`~/.config/agent-secrets/machine.json` (`AGENT_SECRETS_MACHINE_FILE`), mode 0600, and resumes it
whenever it starts, logging `the helper resumed the machine login it kept`, so a restart, a reboot
or an upgrade keeps the machine logged in. It refreshes the login at start, before it resumes it,
and then at most a day apart
(sooner for a credential that lives less than two days),
logging `machine login refreshed` with the new expiry. A machine revoked while its helper was
stopped is refused by the broker from then on, and the helper drops the login at its first refresh
or launcher call the broker refuses. Check it at any time; it also says when the
login lapses if no refresh reaches the broker:

```console
$ agent-secrets machine login-status
issued
agent-secrets machine login-status: the machine login renews itself; without a refresh it lapses at 2026-11-21T03:21:40Z (in 29d23h59m)
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

A login lapses `BROKER_LAUNCHER_CREDENTIAL_SECONDS` after its last refresh (the
[configuration reference](/legion/broker/reference/config/) gives its default), so a machine whose
helper keeps running and reaching the broker stays logged in. Log in again after the helper went
that long without a refresh, after you revoke the machine's login, or when its machine file is
gone. A new login retires the login it replaces and leaves the sessions running on the machine
alone: they keep their enrollments and grants. `agent-secrets machine login-status` exits 1 and
says why when the machine needs it:

```console
$ agent-secrets machine login-status
none
agent-secrets machine login-status: no machine login since the helper started, and none kept on this machine; run: agent-secrets machine login
```

A login nobody approves [expires](/legion/broker/concepts/#machine-login); start a new one. When
the credential reaches its expiry, the broker refuses it, or the broker refuses its refresh (as it
does once the login is revoked), the helper drops it and logs why at ERROR, and `login-status`
prints `expired` with that reason. A refused refresh also deletes the machine file, so the next
start holds no login.
