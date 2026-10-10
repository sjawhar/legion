---
title: Run the broker locally
description: Start a whole Secrets Broker stack on your machine with one script and drive a machine login, a session and an approval by hand.
sidebar:
  order: 13
---

`packages/envoy/scripts/dev-broker.sh` creates a Postgres database of its own, builds the broker and
its clients, writes a fake secrets file, which stands in for Secrets Manager, and starts the broker
on a free port. It needs Go, and a Postgres server: by default it starts one in Docker (a container
named `dispatch-pg`, reused when it is there). Without Docker, run Postgres yourself and set
`DEV_BROKER_POSTGRES_URL` to `postgres://<user>@<host>:<port>/<database>`, for a role that may
create databases; the script then creates and drops its database there with `psql`.

## Start the stack

With Docker the command is the script alone; this run used a Postgres of its own:

```console
$ DEV_BROKER_POSTGRES_URL=postgres://postgres@127.0.0.1:47567/postgres packages/envoy/scripts/dev-broker.sh
dev-broker: workdir /tmp/agent-secrets-dev.SGUyqq
dev-broker: created isolated database dev_broker_sguyqq
dev-broker: building broker, agent-secrets, agent-secrets-helper and agent-secrets-devrelay...
dev-broker: starting broker...
2026/10/03 03:20:19 INFO broker listening addr=127.0.0.1:45355

dev-broker: ready.

  export AGENT_SECRETS_URL=http://127.0.0.1:45355
  export AGENT_SECRETS_UI_TOKEN=dev
  export AGENT_SECRETS_APPROVER=ada@example.com
  export AGENT_SECRETS_HELPER_SOCK=/tmp/agent-secrets-dev.SGUyqq/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=/tmp/agent-secrets-dev.SGUyqq/operator
  export DEV_BROKER_DIR=/tmp/agent-secrets-dev.SGUyqq
  export PATH=/tmp/agent-secrets-dev.SGUyqq/bin:$PATH
...
```

With Docker, the script says `dispatch-pg is already running, reusing it` (or starts it) before it
creates the database.

Leave it running, and paste the exports into a second shell. They point the clients at this broker
and name its UI token, the credential with which Dispatch's server, and here
`agent-secrets-devrelay` in its place, decides for an approver
([Concepts](/legion/broker/concepts/#approvals)). They also keep the helper's socket and its
operator file (the Dispatch login the helper's machine login names, `ada@example.com`) in the
stack's workdir, so the helper never touches one already running for your user and nothing lands in
your current directory. Check that `type agent-secrets` names the workdir's `bin/`: a tool manager
that puts its own directories first on `PATH` at every prompt runs an installed `agent-secrets`
instead.

The fake secrets file holds two secrets of `ada@example.com`'s, under the namespace
`example/agent-secrets/` and on its example key: `DEMO_READ_TOKEN`, tagged `tier=agent`, which her
own box or host sessions ([the kinds of session](/legion/broker/concepts/#sessions-and-enrollment))
get without asking, and `DEMO_API_KEY`, tagged `tier=human`, which she approves. A grant lives an
hour (`BROKER_MAX_GRANT_SECONDS=3600`). Every command below runs in that second shell.

## Run a helper and log it in

Start the helper in the background, with its log in the workdir, and log it in:

```sh
agent-secrets-helper serve 2>"$DEV_BROKER_DIR/helper.log" &
agent-secrets machine login
```

`machine login` prints a code and waits. In a third shell with the same exports, approve it as
Dispatch would:

```console
$ agent-secrets-devrelay machine-approve --code YE9L-5FM5
{"credential_id":"3bc28cd1-d04b-4481-92a8-ff10350fe795","grant_id":null,"state":"approved"}
```

and `machine login` exits 0. The helper's log, `$DEV_BROKER_DIR/helper.log`, says
`machine login issued; the helper holds a launcher credential`.

## Use secrets from a session

Start a shell as a registered session and use the secrets:

```console
$ agent-secrets register --wait 10 --exec -- bash
$ agent-secrets self
enrollment_id: 2b8627f4-9a2a-4c12-85bf-910144bc0b5c
kind: host
operator: ada@example.com
lease_expires_at: 2026-10-03T03:36:40Z
$ agent-secrets DEMO_READ_TOKEN -- printenv DEMO_READ_TOKEN
demo-read-token-value
$ agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- sh -c 'echo "deploying with a ${#DEMO_API_KEY}-character key"'
```

The last command waits for approval, and names who approves it (`waiting for ada@example.com to
approve it under Credential requests in their Dispatch Inbox`). In the third shell, list what waits
on the approver and approve it ([approving a request](/legion/broker/guides/approve-a-request/#without-dispatch)
shows the output), and the command runs:

```console
deploying with a 18-character key
```

A request has two ids: the request id `agent-secrets` prints, and the record id the approver's list
shows and `agent-secrets-devrelay` takes. `agent-secrets request NAME --json` prints both, and
`agent-secrets-devrelay deny --record <record id>` denies a request.

## List and revoke from your own shell

In the third shell, which is outside every session, list your machine logins and the live grants
as Dispatch's pages would, under the helper's machine login
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/#revoke-a-grant-from-your-shell)):

```console
$ agent-secrets machine list
CREDENTIAL_ID                         HOST                 APPROVED_BY      ISSUED                EXPIRES               STATE
a0324632-0cc0-4113-bb26-d4123fab9da6  example-host-devbox  ada@example.com  2026-10-10T15:01:31Z  2026-10-17T15:01:31Z  ok
$ agent-secrets grant list
GRANT_ID                              SECRETS          GRANTED    APPROVER  SESSION                                    OPERATOR         EXPIRES
8fe92a26-b355-4c28-8679-60e8ce98b1d9  DEMO_READ_TOKEN  automatic  -         host/example-host-devbox:2856922:35395441  ada@example.com  2026-10-10T16:01:37Z
```

`machine list` shows your own machines' logins alone; Dispatch's machine-login page also lists
every service's. `agent-secrets grant revoke <grant id>` ends a grant there, and
`agent-secrets machine revoke <credential id>` one of your logins. Run in the session's shell, they
are refused `IN_SESSION`, so the session's own commands cannot act as you:

```console
$ agent-secrets machine list
agent-secrets machine list: IN_SESSION: pid 2857619 is inside a registered host session, which acts on itself alone; run machine and grant commands from your own shell
```

The refusal covers the session's process tree only: a process the session sends out of it (with
`( cmd & )`, `setsid -f` or a tmux server it started) passes, and any process running as your user
can stop the helper. Through these commands such a process can list and revoke your own machine
logins, never a service's machine login, and your grants, which include grants you approved on any
session. `agent-secrets enroll --helper` is open to any process of your user, so such a process can
also enroll a box and read your agent secrets.

## Write a secret and see it served

The broker reads the fake secrets file again each time it lists the secrets, describes one or
reads a value, so an edit to the file is what a write to Secrets Manager is in production, where a
person writes with `agent-secrets secret` ([manage a secret](/legion/broker/guides/manage-a-secret/));
that CLI calls AWS itself, so it cannot write this file. Add a secret of `ada@example.com`'s to it,
here with `jq`:

```sh
jq '.secrets += [{"name": "example/agent-secrets/demo-new-token",
  "kms_key_id": "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
  "tags": {"owner": "ada@example.com", "tier": "agent"}, "value": "demo-new-token-value"}]' \
  "$DEV_BROKER_DIR/fake-secrets.json" >"$DEV_BROKER_DIR/fake-secrets.json.new"
mv "$DEV_BROKER_DIR/fake-secrets.json.new" "$DEV_BROKER_DIR/fake-secrets.json"
```

and ask the broker to reread it, as `agent-secrets secret` does right after each write
([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)). The same call
answered `{"name":"DEMO_NEW_TOKEN","served":false,"reason":"absent"}` before the edit:

```console
$ curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/DEMO_NEW_TOKEN/reread"
{"name":"DEMO_NEW_TOKEN","served":true}
```

The registered session gets it at once:

```console
$ agent-secrets DEMO_NEW_TOKEN -- printenv DEMO_NEW_TOKEN
demo-new-token-value
```

Writing the file whole, as the `mv` does, keeps it readable throughout: while it does not parse,
each of those calls fails, naming the file.

## Stop

1. In the first shell, press Ctrl-C: the script stops the broker, drops its database, and prints
   the workdir it kept.
2. In the second shell, leave the registered session (`exit`), then stop the helper: `kill %1`.
3. Remove the workdir, which holds the binaries, the fake secrets and the logs:
   `rm -rf "$DEV_BROKER_DIR"`.

## What is real and what is not

The broker, its database, its policy and every client are the real ones. Two things stand in for
production: `agent-secrets-devrelay` plays Dispatch's server (it holds the UI token and names the
approver itself), and the fake secrets file (`BROKER_FAKE_SECRETS_FILE`: each secret's name, key,
tags and value, as JSON) plays Secrets Manager and KMS. The broker reads that file again on every
call it makes to it, so editing it is how you write a secret here.
