---
title: Run the broker locally
description: Start a whole secrets broker stack on your machine with one script and drive a machine login, a session and an approval by hand.
sidebar:
  order: 13
---

`packages/envoy/scripts/dev-broker.sh` creates a Postgres database of its own, builds the broker and
its clients, writes example rules and fake secret values, and starts the broker on a free port. It
needs Go, and a Postgres server: by default it starts one in Docker (a container named
`dispatch-pg`, reused when it is there). Without Docker, run Postgres yourself and set
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

The rules grant `DEMO_READ_TOKEN` automatically and need `ada@example.com` to approve
`DEMO_API_KEY`, for a box or host session ([the kinds of
session](/legion/broker/concepts/#sessions-and-enrollment)) whose operator is `ada@example.com`.
Every command below runs in that second shell.

## Run a helper and log it in

Start the helper in the background, with its log in the workdir, and log it in:

```sh
agent-secrets-helper serve 2>"$DEV_BROKER_DIR/helper.log" &
agent-secrets launcher login
```

`launcher login` prints a code and waits. In a third shell with the same exports, approve it as
Dispatch would:

```console
$ agent-secrets-devrelay machine-approve --code YE9L-5FM5
{"credential_id":"3bc28cd1-d04b-4481-92a8-ff10350fe795","grant_id":null,"state":"approved"}
```

and `launcher login` exits 0. The helper's log, `$DEV_BROKER_DIR/helper.log`, says
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

The last command waits for approval. In the third shell, list what waits on the approver and
approve it ([approving a request](/legion/broker/guides/approve-a-request/#without-dispatch)
shows the output), and the command runs:

```console
deploying with a 18-character key
```

A request has two ids: the request id `agent-secrets` prints, and the record id the approver's list
shows and `agent-secrets-devrelay` takes. `agent-secrets request NAME --json` prints both, and
`agent-secrets-devrelay deny --record <record id>` denies a request.

## Stop

1. In the first shell, press Ctrl-C: the script stops the broker, drops its database, and prints
   the workdir it kept.
2. In the second shell, leave the registered session (`exit`), then stop the helper: `kill %1`.
3. Remove the workdir, which holds the binaries, the rules, the fake secrets and the logs:
   `rm -rf "$DEV_BROKER_DIR"`.

## What is real and what is not

The broker, its database, its rules and every client are the real ones. Two things stand in for
production: `agent-secrets-devrelay` plays Dispatch's server (it holds the UI token and names the
approver itself), and the secret values come from the fake secrets file (`source=value` lines)
instead of a secret store.
