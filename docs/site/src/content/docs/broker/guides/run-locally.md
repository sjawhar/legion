---
title: Run the broker locally
description: Start a whole secrets broker stack on your machine with one script and drive a machine login, a session and an approval by hand.
---

`packages/envoy/scripts/dev-broker.sh` starts Postgres in Docker (a container named `dispatch-pg`,
reused when it is there), creates a database of its own, builds the broker and its clients, writes
example rules and fake secret values, and starts the broker on a free port. It needs Docker and Go.

## Start the stack

```console
$ packages/envoy/scripts/dev-broker.sh
dev-broker: workdir /tmp/agent-secrets-dev.U0LbRq
dev-broker: dispatch-pg is already running, reusing it
dev-broker: created isolated database dev_broker_u0lbrq
dev-broker: building broker, agent-secrets, agent-secrets-helper and agent-secrets-devrelay...
dev-broker: starting broker...
2026/10/03 02:06:33 INFO broker listening addr=127.0.0.1:37607

dev-broker: ready.

  export AGENT_SECRETS_URL=http://127.0.0.1:37607
  export AGENT_SECRETS_UI_TOKEN=dev
  export AGENT_SECRETS_APPROVER=ada@example.com
  export PATH=/tmp/agent-secrets-dev.U0LbRq/bin:$PATH
...
```

Leave it running (Ctrl-C stops the broker and drops its database), and paste the four exports into
a second shell. The rules grant `DEMO_READ_TOKEN` automatically and need `ada@example.com` to
approve `DEMO_API_KEY`, for a box or host session `ada@example.com` operates. Every command below
runs in that second shell.

## Run a helper and log it in

Give the helper its own socket, so it never touches a helper already running for your user:

```sh
export AGENT_SECRETS_HELPER_SOCK=$PWD/helper.sock
echo ada@example.com > operator
AGENT_SECRETS_OPERATOR_FILE=$PWD/operator agent-secrets-helper serve &
agent-secrets launcher login
```

`launcher login` prints a code and waits. In a third shell with the same exports, approve it as
Dispatch would:

```console
$ agent-secrets-devrelay machine-approve --code EAGD-7372
{"credential_id":"4d583b6e-61b2-4bb4-bcec-7e01d985992d","grant_id":null,"state":"approved"}
```

and `launcher login` exits 0.

## Use secrets from a session

Start a shell as a registered session and use the secrets:

```console
$ agent-secrets register --wait 10 --exec -- bash
$ agent-secrets self
enrollment_id: 5a60201a-2c18-488e-9e67-10b3e41390e3
kind: host
operator: ada@example.com
lease_expires_at: 2026-10-03T02:21:40Z
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

`agent-secrets request NAME --json` prints a request's record id directly, and
`agent-secrets-devrelay deny --record <id>` denies one.

## What is real and what is not

The broker, its database, its rules and every client are the real ones. Two things stand in for
production: `agent-secrets-devrelay` plays Dispatch's server (it holds the UI token and names the
approver itself), and the secret values come from the fake secrets file (`source=value` lines)
instead of a secret store.
