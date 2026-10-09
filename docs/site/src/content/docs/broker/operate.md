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

A secret owned by a service rather than a person needs that service registered.
`BROKER_SERVICES` lists each service with the Kubernetes service account its pods run as,
whitespace-separated `name=<service account>` entries, for example
`BROKER_SERVICES=legion-daemon=system:serviceaccount:legion:legion-worker` for the Legion daemon's
worker pods.

A pod gets the service's agent-tier secrets at once when two things hold: a machine login for the
listed service enrolled it, and its projected token proved that service account. Every other
session is denied them. Binding the account matters because a machine login names its service
itself, and whoever its login names approves it, so the service name alone proves nothing. For the
same reason each account is bound to one service: the broker refuses to start, naming both
services, if `BROKER_SERVICES` binds one account to two of them.

While a secret's owner tag names a service the list leaves out, the broker refuses the secret as
`owner-tag-malformed` ([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)).

## Signing in to RDS by IAM token

On Amazon RDS or Aurora the broker can sign in to its database with an
[IAM auth token](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/UsingWithRDS.IAMDBAuth.html)
instead of a password, so no database password exists for it to hold or for RDS to rotate under
it. It does so when `BROKER_DATABASE_URL` names a user and no password and its host is an RDS
endpoint, one ending in `.rds.amazonaws.com`:

```sh
BROKER_DATABASE_URL='postgres://broker@<cluster endpoint>:5432/broker?sslmode=verify-full&sslrootcert=/etc/ssl/rds/global-bundle.pem'
AWS_REGION=<region>
```

Each new connection signs in with a token the broker mints for that user and host, signed with
the AWS SDK's default credentials in the region `AWS_REGION`, `AWS_DEFAULT_REGION` or the shared AWS
config names; with none, the broker refuses to start. A token is good for 15 minutes from its mint and
is checked only when a connection signs in, so a connection the broker holds longer keeps working,
and the next connection it opens brings a fresh token. The broker's AWS identity needs
`rds-db:connect` on the database user,
`arn:aws:rds-db:<region>:<account>:dbuser:<cluster resource id>/<user>`, and the database user
must be a member of `rds_iam`. The cluster needs IAM database authentication turned on.

A token is a password to the database until it expires, so the broker sends one only to a server
it has verified. The URL must name that one host, with `sslmode=verify-full` and an `sslrootcert`
file (the Envoy image ships the RDS CA bundle at `/etc/ssl/rds/global-bundle.pem`); otherwise the
broker refuses to start, naming the host: `sslmode=require` encrypts but verifies nothing, and
`sslrootcert=system` (in the URL or `PGSSLROOTCERT`) names the system trust store, which holds no RDS
CA, so every sign-in would fail. The bundle sits outside the system trust store, so nothing else in
the image trusts it. It is the
[global bundle](https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem) AWS publishes,
vendored as `packages/envoy/docker/rds-global-bundle.pem` with the date it was fetched and its
checksum.

A URL with a password, the `${BROKER_DATABASE_PASSWORD}` placeholder, a passwordless URL whose
password libpq supplies (`PGPASSWORD` or a passfile), or a host that is not an RDS endpoint (a
local Postgres that trusts its clients, say) connects as given and mints nothing.

## What it depends on

| Dependency | What the broker needs from it |
| --- | --- |
| Postgres | A database the broker owns (`BROKER_DATABASE_URL`). The broker applies its own migrations at startup; they only move forward, so never run an older broker against a database a newer one has migrated. On Amazon RDS it can sign in by IAM token instead of a password ([Signing in to RDS by IAM token](#signing-in-to-rds-by-iam-token)). |
| AWS Secrets Manager | The agent secrets: every secret whose name starts with `BROKER_SECRETS_PREFIX`, tagged `owner` and `tier`, encrypted with the key `BROKER_SECRETS_KMS_KEY_ARN` names, and holding a non-empty string. [Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) describes the tags, the name each secret is asked for by, and how often the broker rereads them. |
| AWS credentials | The broker calls AWS with the SDK's default credential chain (environment, shared config, or the workload's role), in a region (`AWS_REGION`, `AWS_DEFAULT_REGION` or the shared config). It needs `secretsmanager:ListSecrets` (which takes no resource, so on `*`), `secretsmanager:DescribeSecret` (to reread one secret) and `secretsmanager:GetSecretValue` on the namespace's secrets, `kms:Decrypt` on the agent-secrets key, `kms:ListAliases` (on `*`, called only when a secret names its key by an alias), and, when it signs in to RDS by IAM token, `rds-db:connect` on its database user; nothing else. |
| A local stand-in (development only) | `BROKER_FAKE_SECRETS_FILE` names a JSON file the broker reads in place of both AWS services; the [configuration reference](/legion/broker/reference/config/#variables-the-broker-reads) gives its format. |
| Dispatch | Dispatch's server calls the broker's approval routes. Set Dispatch's `DISPATCH_AGENT_SECRETS_URL` to the broker's URL and `DISPATCH_AGENT_SECRETS_TOKEN` (or `DISPATCH_AGENT_SECRETS_TOKEN_FILE`) to the same value as the broker's `BROKER_UI_TOKEN`. Without them, Dispatch hides its credential pages. |
| Kubernetes (optional) | To enroll pods, `BROKER_K8S_OIDC_ISSUER` and `BROKER_K8S_OIDC_AUDIENCE` name the cluster's service-account token issuer and the audience the pods' projected tokens carry. The broker fetches the issuer's discovery document at startup and refuses to start if it cannot. |
| Envoy (optional) | With `BROKER_ENVOY_URL`, the broker tells a waiting agent session, best effort, when its pending request expires. |

## Network and trust

- Agents, their helpers and launchers, and Dispatch's server reach the broker at
  `BROKER_PUBLIC_URL`. Every signed proof names that URL, so clients' `AGENT_SECRETS_URL` must be
  exactly it, and a proxy in front of the broker must not change the host or path.
- The UI token is an approval credential: whoever holds it can approve as anyone. Give it to
  Dispatch's server and nothing else, and never to an agent.
- Behind a reverse proxy, set `BROKER_TRUSTED_PROXY_HEADER` so the machine-login and secret-reread
  rate limiters see each caller's address rather than the proxy's.
- The settings and reread routes take no credential. `GET /v1/settings` answers the namespace
  prefix, the agent-secrets key's ARN, and that key's AWS account and region.
  `POST /v1/secrets/{name}/reread` makes the broker read one secret from Secrets Manager at once,
  and answers whether it now serves it and, if not, why. Neither releases a value. What a reread
  does tell any caller who can reach the broker is whether a name under the namespace exists and,
  when the broker leaves it out, which of its reasons applies — `absent`, or a refusal such as
  `owner-tag-missing` or `not-on-agent-secrets-key`. That is more than `UNKNOWN_SECRET`, which says
  only that no agent secret has the name, says it only to an enrolled session, and never separates a
  secret that does not exist from one the broker refuses; it is what anyone allowed to list the
  namespace reads from the secrets' own names and tags.
  Rereads are limited twice, by fixed limits rather than settings (`DefaultRereadLimit` and
  `DefaultRereadOverallLimit` in `packages/envoy/internal/broker/api/limits.go`): each source
  address gets a burst of 30, refilled one every 2 seconds, and the broker as a whole takes 4 a
  second, burst 10, however many addresses the rereads come from — each one takes the policy's
  writer lock while it reads the secret, so that lock, not the caller, is what the second limit
  bounds. A reread counts against either limit only when both let it run, so one address flooding
  past its own limit leaves the broker-wide one to everyone else. Past either, the broker answers
  `429 RATE_LIMITED` with a `Retry-After` header naming the limit that refused.
  `agent-secrets secret` sends one reread after each write it makes, from the person's own
  address. When a limit refuses it, the write stands in Secrets Manager and the CLI exits 1 saying
  so ([manage a secret](/legion/broker/guides/manage-a-secret/#what-the-broker-answers-after-a-write)).
- Agents never see the broker's database or the secret store; whoever can write the database can
  forge a record, so its access control is part of the broker's. Whoever can tag a secret under the
  namespace decides who gets it, so the tags' write access is part of the broker's too.

## People who manage secrets

The broker writes no secret. A person creates, changes and deletes agent secrets with
`agent-secrets secret` ([manage a secret](/legion/broker/guides/manage-a-secret/)), which calls
Secrets Manager itself under that person's own AWS sign-in, so IAM in the broker's account is what
decides who may change which secret, and with the tags, who gets it. The CLI asks the broker only
for its settings (`GET /v1/settings`) and, after each write, for a reread. Before anything else it
refuses a sign-in in another account than the agent-secrets key's, and for a write any sign-in but
a person's own IAM Identity Center one; those checks are the CLI's, and IAM is the boundary. A
person's access needs each form's permissions on the namespace's secrets, and the table says what
each request carries that a policy's conditions can match:

| Form | Permissions | What a condition can match |
| --- | --- | --- |
| every form | `sts:GetCallerIdentity`, which needs no permission | |
| `list` | `secretsmanager:ListSecrets` (on `*`: it takes no resource) | |
| `show` | `secretsmanager:DescribeSecret` | The secret's own tags (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`) |
| `create` | `secretsmanager:CreateSecret` and `secretsmanager:TagResource`, since it tags the secret as it creates it; `kms:GenerateDataKey` and `kms:Decrypt` on the agent-secrets key | The new secret's name under the namespace (its ARN, and `secretsmanager:Name`); both request tags, `aws:RequestTag/owner` (the person's email, from `--owner me`, or `shared`) and `aws:RequestTag/tier` (`aws:TagKeys` is the two); the key, named by its ARN (`secretsmanager:KmsKeyArn`); the `TagResource` check made on the new secret sees the requested tags as its own (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`), so a policy that reserves a shared secret's tags for administrators still needs a statement that admits a shared create (request and resource owner both `shared`, the same tier) |
| `set` | `secretsmanager:PutSecretValue`; `kms:GenerateDataKey` on the agent-secrets key | The secret's own tags (`aws:ResourceTag/owner`) |
| `retag` | `secretsmanager:DescribeSecret` and `secretsmanager:TagResource` | Both request tags in every request, the unchanged one re-sent as the secret holds it, and the secret's own tags, so a policy can let a person tag their own secret (`aws:ResourceTag/owner` their email) to themselves or `shared`, and reserve a shared secret's tags for administrators |
| `delete` | `secretsmanager:DeleteSecret` | The secret's own tags; `secretsmanager:RecoveryWindowInDays` is 30, and `secretsmanager:ForceDeleteWithoutRecovery` is never set |
| `restore` | `secretsmanager:RestoreSecret` | The secret's own tags |

Secrets Manager makes the KMS calls itself, on the person's behalf, so the agent-secrets key's
policy can allow them only through Secrets Manager (`kms:ViaService` of
`secretsmanager.<region>.amazonaws.com`, with `kms:CallerAccount`). The person's email in
`aws:RequestTag/owner` is their Identity Center session name in lowercase, so a condition that
compares it with a principal tag holding their email matches only where that tag is lowercase too.
No form calls `GetSecretValue` or prints a value. `delete` and `restore` are permissions of their
own: an access that grants a person every other form on their own secret but not these refuses
those two with `AccessDeniedException`.

## Health and logs

`GET /healthz` answers `200 {"status":"ok"}` while the broker can reach Postgres, and
`503 DATABASE_UNAVAILABLE` when it cannot. Use it for liveness and readiness.

The broker logs text lines to stderr. The ones worth alerting or searching on:

| Log line | Meaning |
| --- | --- |
| `broker listening addr=<host:port>` | Startup finished: configuration, migrations and the first read of the namespace all succeeded, and the address is bound. |
| `broker: fatal error=…` | Startup refused; the error names the variable or dependency. The process exits 1. |
| `agent secret policy refused name=<secret name> reason=<reason>` | At ERROR, on every read of the namespace, once for each secret the broker leaves out, and on every reread of one secret it leaves out: `owner-tag-missing`, `owner-tag-malformed`, `tier-tag-missing`, `tier-tag-malformed`, `name-malformed`, `service-owner-human-tier`, `not-on-agent-secrets-key` or `no-current-value` (no version carries `AWSCURRENT`: the secret was created without a value; once its value is put it is served at its next reread, or within about ten minutes, as the broker rereads the namespace every five and Secrets Manager's listing can lag a change by up to five more). A secret with no value and another fault is logged for the other fault. `name` is the secret's whole Secrets Manager name. Every other secret is still served. Two things make this line more frequent than one per refused secret per five minutes, and an alarm on it has to allow for both: a refused secret that was reread in the last five minutes is logged twice by each read of the namespace, once from the listing and once from the reread of that one name the read makes; and any caller who can reach the broker writes one by asking for a reread of a refused name, at up to the rate the reread route allows (above). |
| `agent secret policy load failed; previous policy kept error=…` | At ERROR: a reread of the namespace failed (Secrets Manager or KMS out of reach, or refusing the broker), and the broker keeps serving the policy from its last good read; or, with `name=<NAME>` (the name a request asked for), the request named a secret the broker did not serve and the broker's reread of that one secret failed, so the request is refused `UNKNOWN_SECRET`. A reread of the namespace in flight when the broker shuts down is not one of these and logs nothing. |
| `broker: <operation> failed error=…` | A request failed with a 500 or 503; the line carries the cause the response does not. A reread whose own request ended before it finished - its caller went away, or it reached the 45-second bound - is not one of these: it answers `503 REQUEST_ENDED` and logs nothing, since anyone can end a request. |
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
