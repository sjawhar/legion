---
title: Operating the broker
description: Running the Secrets Broker - its configuration, its Postgres and secret-store dependencies, Dispatch's connection to it, health, logs and the audit record.
sidebar:
  order: 3
---

The broker is one stateless HTTP process. Everything it knows lives in Postgres; it reads who may
have which secret, and the secrets' values, from AWS Secrets Manager, and it starts nothing else.

## What to run

The broker ships as `envoy-broker` in the Envoy image, `ghcr.io/sjawhar/legion/envoy:<commit>`,
which every push to `main` that touches the image's sources publishes (the image's default
entrypoint is the Envoy listener, so run it with `--entrypoint envoy-broker`). To build it yourself:

```sh
cd packages/envoy
go build -o envoy-broker ./cmd/broker
```

It takes no flags (it refuses any flag it is given) and reads all of its configuration from the
environment. The [configuration reference](/legion/broker/reference/config/), generated from the
loader, lists every variable with its default and accepted values. A minimal production
configuration is:

```sh
BROKER_LISTEN_ADDR=0.0.0.0:13380
BROKER_PUBLIC_URL=https://secrets.internal.example
BROKER_DATABASE_URL='postgres://broker:${BROKER_DATABASE_PASSWORD}@db.internal.example:5432/broker'
BROKER_DATABASE_PASSWORD=<placeholder>
BROKER_UI_TOKEN_FILE=/run/secrets/broker-ui-token
BROKER_SECRETS_PREFIX=production/agent-secrets/
BROKER_SECRETS_KMS_KEY_ARN=arn:aws:kms:<region>:<account>:key/<key id>
AWS_REGION=<region>
```

`AWS_REGION` is the AWS SDK's own setting, not the broker's: the SDK takes the region of Secrets
Manager and KMS only from `AWS_REGION`, `AWS_DEFAULT_REGION` or the shared AWS config file, never
from the instance it runs on. Without one, the broker's first read of the namespace fails and it
exits at startup with `broker: fatal error="list secrets under production/agent-secrets/: operation
error Secrets Manager: ListSecrets, failed to resolve service endpoint, endpoint rule error, Invalid
Configuration: Missing Region"`.

The broker refuses to start, naming the variable, when one is missing, malformed or out of range,
and also while a variable it no longer reads is still set.

## What it depends on

| Dependency | What the broker needs from it |
| --- | --- |
| Postgres | A database the broker owns (`BROKER_DATABASE_URL`). The broker applies its own migrations at startup; they only move forward, so never run an older broker against a database a newer one has migrated. |
| AWS Secrets Manager | The agent secrets: every secret whose name starts with `BROKER_SECRETS_PREFIX`, tagged `owner` and `tier`, encrypted with the key `BROKER_SECRETS_KMS_KEY_ARN` names, and holding a non-empty string. The broker reads the namespace at startup and every five minutes; [Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) describes the tags and the name each secret is asked for by. |
| AWS credentials | The broker calls AWS with the SDK's default credential chain (environment, shared config, or the workload's role), in a region (`AWS_REGION`, `AWS_DEFAULT_REGION` or the shared config). It needs `secretsmanager:ListSecrets` (which takes no resource, so on `*`), `secretsmanager:GetSecretValue` on the namespace's secrets with `kms:Decrypt` on the agent-secrets key, and `kms:ListAliases` (on `*`, called only when a secret names its key by an alias), and nothing else. |
| A local stand-in (development only) | `BROKER_FAKE_SECRETS_FILE` names a JSON file the broker reads in place of both AWS services, `{"secrets": [{"name", "kms_key_id", "tags", "value"}]}`: each secret's whole name, the key it is on, its tags and its value. |
| Dispatch | Dispatch's server calls the broker's approval routes. Set Dispatch's `DISPATCH_AGENT_SECRETS_URL` to the broker's URL and `DISPATCH_AGENT_SECRETS_TOKEN` (or `DISPATCH_AGENT_SECRETS_TOKEN_FILE`) to the same value as the broker's `BROKER_UI_TOKEN`. Without them, Dispatch hides its credential pages. |
| Kubernetes (optional) | To enroll pods, `BROKER_K8S_OIDC_ISSUER` and `BROKER_K8S_OIDC_AUDIENCE` name the cluster's service-account token issuer and the audience the pods' projected tokens carry. The broker fetches the issuer's discovery document at startup and refuses to start if it cannot. |
| Envoy (optional) | With `BROKER_ENVOY_URL`, the broker tells a waiting agent session, best effort, when its pending request expires. |

## Network and trust

- Agents, their helpers and launchers, and Dispatch's server reach the broker at
  `BROKER_PUBLIC_URL`. Every signed proof names that URL, so clients' `AGENT_SECRETS_URL` must be
  exactly it, and a proxy in front of the broker must not change the host or path.
- The UI token is an approval credential: whoever holds it can approve as anyone. Give it to
  Dispatch's server and nothing else, and never to an agent.
- Behind a reverse proxy, set `BROKER_TRUSTED_PROXY_HEADER` so the machine-login rate limiter sees
  each caller's address rather than the proxy's.
- Agents never see the broker's database or the secret store; whoever can write the database can
  forge a record, so its access control is part of the broker's. Whoever can tag a secret under the
  namespace decides who gets it, so the tags' write access is part of the broker's too.

## Health and logs

`GET /healthz` answers `200 {"status":"ok"}` while the broker can reach Postgres, and
`503 DATABASE_UNAVAILABLE` when it cannot. Use it for liveness and readiness.

The broker logs text lines to stderr. The ones worth alerting or searching on:

| Log line | Meaning |
| --- | --- |
| `broker listening addr=<host:port>` | Startup finished: configuration, migrations and the first read of the namespace all succeeded, and the address is bound. |
| `broker: fatal error=…` | Startup refused; the error names the variable or dependency. The process exits 1. |
| `agent secret policy refused name=<secret name> reason=<reason>` | At ERROR, on every read of the namespace, once for each secret the broker leaves out: `owner-tag-missing`, `owner-tag-malformed`, `tier-tag-missing`, `tier-tag-malformed`, `name-malformed`, `service-owner-human-tier` or `not-on-agent-secrets-key`. `name` is the secret's whole Secrets Manager name. Every other secret is still served. |
| `agent secret policy load failed; previous policy kept error=…` | At ERROR: a reread of the namespace failed (Secrets Manager or KMS out of reach, or refusing the broker). The broker keeps serving the policy from its last good read. |
| `broker: <operation> failed error=…` | A request failed with a 500 or 503; the line carries the cause the response does not. |
| `broker sweeper: ended an enrollment whose lease lapsed enrollment_id=… kind=… runtime_id=… slot=… lease_expired_at=… grants_revoked=… requests_cancelled=…` | A session stopped renewing (a pod that is gone, a box whose `agent-secrets renew` stopped); the sweep ended it, revoking its grants and cancelling its pending requests. |
| `broker shutting down` | SIGTERM or SIGINT: in-flight requests get 10 seconds to finish. |

Each request is bounded to 45 seconds. Request bodies are JSON of at most 1 MiB.

## The audit record

Requests, records, decisions and the audit trail live in the broker's Postgres database
([Concepts](/legion/broker/concepts/#the-audit-record) explains them). To read the latest events:

```sql
select at, kind, actor, enrollment_id, request_id, grant_id, detail
from audit
order by at desc
limit 50;
```

and the decisions on credential requests:

```sql
select cr.id, cr.kind, cr.approver, r.state as request_state, e.event, e.actor, e.at
from credential_requests cr
left join requests r on r.record_id = cr.id
left join credential_request_events e on e.record_id = cr.id
order by cr.created_at desc
limit 50;
```

A secret request's state is its request's (`request_state`), which is what the broker goes by: a
request an older broker cancelled when its session ended carries no event on its record. A machine
login has no request, and its events alone say how it was decided.

Neither holds a secret value.

## Running it locally

[Run the broker locally](/legion/broker/guides/run-locally/) starts a whole stack (Postgres, the
broker on a fake secrets file, and its clients) with one script.
