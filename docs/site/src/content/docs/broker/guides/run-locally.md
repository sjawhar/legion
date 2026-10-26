---
title: Run the broker locally
description: Start a whole Secrets Broker stack on your machine with one script and drive a machine login, a session and an approval by hand.
sidebar:
  order: 13
---

`packages/envoy/scripts/dev-broker.sh` creates a Postgres database of its own, builds the broker,
its clients and `dev-oidc-issuer`, writes a fake secrets file, which stands in for Secrets Manager,
starts `dev-oidc-issuer`, which stands in for a cluster's ServiceAccount token issuer, and starts
the broker on a free port, trusting that issuer. It needs Go, and a Postgres server: by default it
starts one in Docker through `packages/envoy/scripts/dev-postgres.sh` (a container named
`dispatch-pg`, reused when it is there; one made from another image than that script's is
refused, with the commands that move its data onto the right one). Without Docker, run Postgres
yourself and set `DEV_BROKER_POSTGRES_URL` to `postgres://<user>@<host>:<port>/<database>`, for a
role that may create databases; the script then creates and drops its database there with `psql`.

## Start the stack

With Docker the command is the script alone; this run used a Postgres of its own:

```console
$ DEV_BROKER_POSTGRES_URL='postgres://broker:<password>@127.0.0.1:5433/postgres?sslmode=disable' packages/envoy/scripts/dev-broker.sh
dev-broker: workdir /tmp/agent-secrets-dev.Tp2tQV
dev-broker: created isolated database dev_broker_tp2tqv
dev-broker: building broker, dev-oidc-issuer, agent-secrets, agent-secrets-helper and agent-secrets-devrelay...
dev-broker: starting dev-oidc-issuer...
dev-oidc-issuer: /tmp/agent-secrets-dev.Tp2tQV/account-token holds system:serviceaccount:example-ns:example-dispatch's token for audience legion-broker-pod (pod f3b3a12a-56ff-4e89-ac85-889b20d971a9)
dev-oidc-issuer: /tmp/agent-secrets-dev.Tp2tQV/exchange-token holds system:serviceaccount:example-ns:example-worker's token for audience legion-broker-exchange (pod a829f93b-ad87-4efb-8d03-74b51e97c810)
dev-oidc-issuer: issuer listening url=http://127.0.0.1:36930
dev-broker: starting broker...
2026/10/25 10:49:22 INFO database sign-in method=password
2026/10/25 10:49:22 INFO secret store store=aws-secrets-manager credentials="fake secrets file"
2026/10/25 10:49:22 INFO broker listening addr=127.0.0.1:62083
2026/10/25 10:49:22 INFO token exchange state=on kubernetes=true github=false accounts=1 workflows=0

dev-broker: ready.

  export AGENT_SECRETS_URL=http://127.0.0.1:62083
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=/tmp/agent-secrets-dev.Tp2tQV/account-token
  export AGENT_SECRETS_APPROVER=ada@example.com
  export AGENT_SECRETS_HELPER_SOCK=/tmp/agent-secrets-dev.Tp2tQV/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=/tmp/agent-secrets-dev.Tp2tQV/operator
  export AGENT_SECRETS_MACHINE_FILE=/tmp/agent-secrets-dev.Tp2tQV/machine.json
  export DEV_BROKER_DIR=/tmp/agent-secrets-dev.Tp2tQV
  export PATH=/tmp/agent-secrets-dev.Tp2tQV/bin:$PATH
...
```

With Docker, the script says `dispatch-pg is already running, reusing it` (or starts it) before it
creates the database.

Leave it running, and paste the exports into a second shell. They point the clients at this broker
and name the caller token, the ServiceAccount token with which Dispatch's server, and here
`agent-secrets-devrelay` in its place, decides for an approver
([Concepts](/legion/broker/concepts/#approvals)). The broker lists that token's account,
`system:serviceaccount:example-ns:example-dispatch`, as its caller `dispatch`, and
`dev-oidc-issuer` rewrites the file before its token lapses, as a kubelet rewrites a pod's
projected token, so the exports stay good for as long as the stack runs. They also keep the
helper's socket, its operator file (the Dispatch login the helper's machine login names,
`ada@example.com`) and its
machine file (the login the helper keeps across restarts) in the stack's workdir, so the helper
never touches one already running for your user, or your own machine's login, and nothing lands in
your current directory. Check that `type agent-secrets` names the workdir's `bin/`: a tool manager
that puts its own directories first on `PATH` at every prompt runs an installed `agent-secrets`
instead.

The fake secrets file holds two secrets of `ada@example.com`'s, under the namespace
`example/agent-secrets/` and on its example key: `DEMO_READ_TOKEN`, tagged `tier=agent`, which her
own sessions ([the kinds of session](/legion/broker/concepts/#sessions-and-enrollment)) get without
asking, and `DEMO_API_KEY`, tagged `tier=human`, which she approves. A grant lasts until its session
ends, or for the `--ttl` its request names. Every command below runs in that second shell.

## Run a helper and log it in

Start the helper in the background, with its log in the workdir, and log it in. It runs from this
shell rather than through `agent-secrets-helper install`, since installing would rewrite your own
helper's `systemd --user` unit to point at the development broker:

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
`machine login issued; the helper holds a launcher credential`, and a helper you stop and start
again with the same exports resumes that login from `$AGENT_SECRETS_MACHINE_FILE`.

## Use secrets from a session

Start a shell as a registered session and use the secrets:

```console
$ agent-secrets register --wait 10 --exec -- bash
$ agent-secrets self
enrollment_id: 2b8627f4-9a2a-4c12-85bf-910144bc0b5c
kind: session
operator: ada@example.com
lease_expires_at: 2026-10-03T03:36:40Z
source: the broker at 127.0.0.1:62083 (AWS Secrets Manager account 111122223333 in us-east-1)
$ agent-secrets DEMO_READ_TOKEN -- printenv DEMO_READ_TOKEN
agent-secrets: DEMO_READ_TOKEN from the broker at 127.0.0.1:62083 (AWS Secrets Manager account 111122223333 in us-east-1)
demo-read-token-value
$ agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- sh -c 'echo "deploying with a ${#DEMO_API_KEY}-character key"'
```

Every line about a secret names where it came from: the broker this stack runs, and the store that
broker reads. The local stack's store is `BROKER_FAKE_SECRETS_FILE`, which stands in for AWS
Secrets Manager, so the lines name the account and region its key ARN carries.

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

Each grant so far lasts until the session ends (`agent-secrets self` lists it `until this session
ends`). A request may ask for less with `--ttl`, and the grant then ends on its own once that has
passed, while the session lives on:

```console
$ agent-secrets DEMO_READ_TOKEN --ttl 2m -- printenv DEMO_READ_TOKEN
agent-secrets: DEMO_READ_TOKEN from the broker at 127.0.0.1:62083 (AWS Secrets Manager account 111122223333 in us-east-1)
demo-read-token-value
$ agent-secrets self
enrollment_id: 7cf72ec3-d10f-46fd-984f-ddd856b229c2
kind: session
operator: ada@example.com
lease_expires_at: 2026-10-11T01:51:13Z
source: the broker at 127.0.0.1:62083 (AWS Secrets Manager account 111122223333 in us-east-1)
grant: f81251ab-6552-450c-9eca-12a793a29f4c (request 86237327-1f78-4c79-82a5-4f71174e71c5, expires 2026-10-11T01:38:14Z)
```

Two minutes later that grant releases nothing (`GRANT_NOT_LIVE`), `self` no longer lists it, and
the same command asks again. `--ttl` is a whole number of seconds, `2m` or `90s`; anything else
exits 2 before asking. When the broker or the machine's helper is from before `--ttl`, which would
grant for longer, `agent-secrets` ends the grant it got (or cancels the request still waiting),
says so, runs nothing and exits 1.

## List and revoke from your own shell

In the third shell, which is outside every session, list your machine logins and the live grants
as Dispatch's pages would, under the helper's machine login
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/#revoke-a-grant-from-your-shell)):

```console
$ agent-secrets machine list
CREDENTIAL_ID                         HOST                 APPROVED_BY      ISSUED                EXPIRES               STATE
a0324632-0cc0-4113-bb26-d4123fab9da6  example-host-devbox  ada@example.com  2026-10-10T15:01:31Z  2026-10-17T15:01:31Z  ok
Every row is a login to the broker at 127.0.0.1:62083.
$ agent-secrets grant list
GRANT_ID                              SECRETS          GRANTED    APPROVER  SESSION                                       OPERATOR         EXPIRES
8fe92a26-b355-4c28-8679-60e8ce98b1d9  DEMO_READ_TOKEN  automatic  -         session/example-host-devbox:2856922:35395441  ada@example.com  session-end
Every row is a grant from the broker at 127.0.0.1:62083 (AWS Secrets Manager account 111122223333 in us-east-1).
```

Each list says on standard error which broker and store its rows are from, so the table on
standard output stays one row per login or grant for a script.

`machine list` shows your own machines' logins alone; Dispatch's machine-login page also lists
every service's. `agent-secrets grant revoke <grant id>` ends a grant there, and
`agent-secrets machine revoke <credential id>` one of your logins. Run in the session's shell, they
are refused `IN_SESSION`, so the session's own commands cannot act as you:

```console
$ agent-secrets machine list
agent-secrets machine list: IN_SESSION: pid 2857619 is inside a registered machine session, which acts on itself alone; run machine and grant commands from your own shell
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

## Exchange a pod's token

The broker's RFC 8693 token exchange is on, for one account,
`system:serviceaccount:example-ns:example-worker`, writing the project `EXAMPLE` for
`ada@example.com` under a session name the pod chooses. `$DEV_BROKER_DIR/exchange-token` holds that
account's pod token for the exchange's own audience, `legion-broker-exchange`. A `POST
/v1/oauth/token` with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`,
`client_id=dispatch-token-exchange`, that token as `subject_token`,
`subject_token_type=urn:envoy:params:oauth:token-type:kubernetes-service-account` and a `session`,
signed with a DPoP proof of a key of the caller's own, mints a DPoP-bound access token, and the
caller token reads it back
(`POST /v1/oauth/introspect` with the token in the form):

```console
$ curl -s -H "Authorization: Bearer $(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")" -d "token=<access token>" "$AGENT_SECRETS_URL/v1/oauth/introspect"
{"active":true,"session":"example-worker-session","person":"ada@example.com","project_scope":"named","project":"EXAMPLE","exp":1792929646,"client_id":"dispatch-token-exchange","jkt":"o__snCYl91_F2gKf-cLiLfkVmEfGOrpysCbt-j_FOJA"}
```

## Stop

1. In the first shell, press Ctrl-C: the script stops the broker and `dev-oidc-issuer`, drops its
   database, and prints the workdir it kept. A token the issuer minted verifies nowhere else, and no
   longer verifies once it stops.
2. In the second shell, leave the registered session (`exit`), then stop the helper: `kill %1`.
3. Remove the workdir, which holds the binaries, the fake secrets, the token files and the logs:
   `rm -rf "$DEV_BROKER_DIR"`.

## What is real and what is not

The broker, its database, its policy and every client are the real ones. Three things stand in for
production: `agent-secrets-devrelay` plays Dispatch's server (it presents the caller token and
names the approver itself); `dev-oidc-issuer` plays the cluster's ServiceAccount token issuer
(the Go tests' own local issuer, `internal/oidc/oidctest`, served on a loopback port: it publishes
a key it holds in memory and mints the two token files, each bound to a pod of its own); and the
fake secrets file (`BROKER_FAKE_SECRETS_FILE`: each secret's name, key, tags and value, as JSON)
plays Secrets Manager and KMS. The broker reads that file again on every call it makes to it, so
editing it is how you write a secret here.

One thing is left out rather than stood in for: the stack sets no `BROKER_APPROVAL_*` group, so
this broker serves no approval page and takes every approval on its listed caller's word, which
is what lets `agent-secrets-devrelay approve` decide at all. A deployment sets that group, and
there an approval is applied only by the approving person's own sign-in
([Approvals](/legion/broker/concepts/#approvals)).
