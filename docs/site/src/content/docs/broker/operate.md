---
title: Operating the broker
description: Running the Secrets Broker - its configuration, its Postgres and secret-store dependencies, Dispatch's connection to it, health, logs and the audit record.
sidebar:
  order: 3
---

The broker is a stateless HTTP process, and a deployment may run more than one of them behind one
address ([Running more than one pod](#running-more-than-one-pod)). Everything it knows lives in
Postgres; it reads who may have which secret, and the secrets' values, from its secret store, AWS
Secrets Manager or Google Cloud Secret Manager, and it starts nothing else.

## What to run

The broker ships as `envoy-broker` in the Envoy image, `ghcr.io/sjawhar/legion/envoy:<commit>`,
which every push to `main` that touches the image's sources publishes (the image's default
entrypoint is the Envoy listener, so run it with `--entrypoint envoy-broker`). Pin it by digest
and verify it as [Running Dispatch](/legion/dispatch/running-dispatch/) shows. To build it yourself:

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
BROKER_MACHINE_LOGIN_URL=https://dispatch.internal.example/credentials/machine
BROKER_DATABASE_URL='postgres://broker:<password>@db.internal.example:5432/broker'
BROKER_UI_TOKEN_FILE=/run/secrets/broker-ui-token
BROKER_SECRETS_PREFIX=production/agent-secrets/
BROKER_SECRETS_KMS_KEY_ARN=arn:aws:kms:<region>:<account>:key/<key id>
AWS_REGION=<region>
```

`BROKER_SECRETS_STORE` picks the secret store: `aws-secrets-manager`, the default when it is unset,
as above, or `gcp-secret-manager`, which takes the Google Cloud forms of the namespace and the key
and reads nothing from AWS:

```sh
BROKER_SECRETS_STORE=gcp-secret-manager
BROKER_SECRETS_PREFIX=projects/<project number>/secrets/agent-secrets-
BROKER_SECRETS_KMS_KEY_NAME=projects/<project>/locations/global/keyRings/<ring>/cryptoKeys/<key>
```

The prefix names the project by its number, not its id, because Secret Manager answers every
resource name with the number and the broker matches what the API answers (`gcloud projects
describe <id> --format='value(projectNumber)'` finds it); the id after `secrets/` ends in `-`, so
the secret a session asks for as `DEEL_API_KEY` is `projects/<number>/secrets/agent-secrets-deel-api-key`.
The key is in the `global` location, the only one a secret with automatic replication can be
encrypted with. Each store refuses the other's key variable, so a deployment moved between stores
cannot keep the old key set and silently mean nothing by it.

`AWS_REGION` is the AWS SDK's own setting, not the broker's, read by the AWS store alone: the SDK
takes the region of Secrets Manager and KMS only from `AWS_REGION`, `AWS_DEFAULT_REGION` or the
shared AWS config file, never from the instance it runs on. Without one, the broker's first read of
the namespace fails and it exits at startup with `broker: fatal error="list secrets under
production/agent-secrets/: operation error Secrets Manager: ListSecrets, failed to resolve service
endpoint, endpoint rule error, Invalid Configuration: Missing Region"`.

The broker refuses to start, naming the variable, when one is missing, malformed or out of range,
and also while a variable it no longer reads is still set. `BROKER_DATABASE_URL` is the only
database variable: a password goes in the URL itself, URL-escaped, and on Amazon RDS the broker
needs none ([Signing in to RDS by IAM token](#signing-in-to-rds-by-iam-token)).

A secret owned by a service rather than a person needs that service registered.
`BROKER_SERVICES` lists each service with the Kubernetes service account its pods run as,
whitespace-separated `name=<service account>` entries, for example
`BROKER_SERVICES=legion-daemon=system:serviceaccount:legion:legion-worker` for the Legion daemon's
worker pods.

A pod gets the service's agent-tier secrets at once when two things hold: a machine login for the
listed service enrolled it, and its projected token proved that service account. Every other
session is denied them. For the same reason each account is bound to one service: the broker
refuses to start, naming both services, if `BROKER_SERVICES` binds one account to two of them.

The account must be one the service's pods name. The broker refuses to start, naming the entry,
if `BROKER_SERVICES` binds a namespace's `default` account
(`system:serviceaccount:<namespace>:default`). Every pod in a namespace whose spec names no account
runs as `default`, and any of those pods can get a token for the broker's audience. A `default`
binding would therefore prove nothing about which pods are the service's. Create a dedicated
account, such as `legion-worker`, set it as the pods' `serviceAccountName`, and bind that account.

While a secret's owner tag names a service the list leaves out, the broker refuses the secret as
`owner-tag-malformed` ([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)).

A service's launcher, such as the Legion daemon, logs in with its own projected ServiceAccount
token, and no person approves it. `BROKER_SERVICE_LOGINS` lists which ServiceAccounts may log in
as which service, whitespace-separated `<service>=<service account>` entries, for example
`BROKER_SERVICE_LOGINS=legion-daemon=system:serviceaccount:legion-system:legion-daemon-legion`,
one entry per daemon. Each account is named exactly, with no wildcard, and logs in as the one
service its entry names. The broker refuses to start on an entry naming a namespace's `default`
account, which every pod in that namespace whose spec names no account runs as, or an account
`BROKER_SERVICES` binds for a service's pods, which every such pod holds a token for: either would
let a pod log in as the daemon. The broker verifies the token as it verifies a pod's, against
`BROKER_K8S_OIDC_ISSUER` and `BROKER_K8S_OIDC_AUDIENCE`, so the setting needs both, and refuses a
token that lives longer than an hour from its issue (`exp` minus `iat`, beside a minute of skew) as
`TOKEN_TOO_LONG_LIVED`: give the daemon's projected token an `expirationSeconds` of at most 3600,
and `kubectl create token` a `--duration` of at most `1h`. Each account holds one launcher
credential: every login moves it to the login's key and to the token's expiry, at most an hour
away. A running daemon proves the same key at each login, so nothing about it changes but its
expiry; a restart's login brings a new key, so the daemon's earlier key stops working at once and
the pods it enrolled before the restart stay its own. A device-code login that names a service is
refused `SERVICE_LOGS_IN_WITH_TOKEN`. Dispatch's machine-login page lists a daemon's token login
for everyone signed in, naming its account and no approver, and anyone may revoke it, which ends
every pod it enrolled at once; the daemon then logs in again with its token under a new credential,
which enrolls the pods it launches next, while a pod already running stays without secrets until it
is replaced. To stop a daemon logging in, remove its entry and redeploy the broker: its credential
stops authenticating at its next call, and its next login is refused.

Whoever can get a token for a listed account logs in as its service. Kubernetes access control on
the daemon's namespace is that boundary: anyone who may create pods there naming the account,
create `serviceaccounts/token` for it, or exec into the daemon's pod and read its token file can
get one. Deleting the daemon's ServiceAccount stops new tokens, but a token already issued keeps
verifying until it expires, at most an hour after it was minted, because the broker checks tokens
against the cluster's published keys rather than asking the API server. A token login spends its
source address's rate-limit bucket before its token is verified and its account's own after, so a
daemon behind an address other callers share is held to that address's bucket. Set
`BROKER_TRUSTED_PROXY_HEADER` behind a proxy, so each caller is keyed on its own address, and
keep the daemon's and its pods' traffic to the broker on their own pod addresses rather than
translated to a node's or a gateway's. The daemon renews halfway through its credential's life,
which carries it through brief contention for its address's bucket; a caller that keeps that
bucket drained for the rest of the credential's life stops it logging in.

A service login a person approved through a device code before this broker keeps working until
it expires, at most `BROKER_LAUNCHER_CREDENTIAL_SECONDS` (30 days by default) after its approval:
nothing refreshes it.

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
must be a member of `rds_iam`. The cluster needs IAM database authentication turned on. Give the
broker a database user of its own, and never grant `rds_iam` to the RDS master user, directly or
through a role it belongs to: once a user holds it, IAM authentication takes precedence over that
user's password
([AWS's limitations of IAM database authentication](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/UsingWithRDS.IAMDBAuth.html#UsingWithRDS.IAMDBAuth.Limitations)),
so the master could no longer sign in with its password.

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

A URL with a password, a passwordless URL whose password libpq supplies (`PGPASSWORD` or a
passfile), or a host that is not an RDS endpoint (a local Postgres that trusts its clients, say)
connects as given and mints nothing. The broker substitutes no password into the URL: it refuses
to start while the removed `BROKER_DATABASE_PASSWORD` is set or the URL names its
`${BROKER_DATABASE_PASSWORD}` placeholder.

## What it depends on

| Dependency | What the broker needs from it |
| --- | --- |
| Postgres | A database the broker owns (`BROKER_DATABASE_URL`). The broker applies its own migrations at startup; they only move forward, so never run an older broker against a database a newer one has migrated. Each process also holds one connection outside its pool, for as long as it runs, to hear what the other processes reread ([Running more than one pod](#running-more-than-one-pod)). On Amazon RDS it can sign in by IAM token instead of a password ([Signing in to RDS by IAM token](#signing-in-to-rds-by-iam-token)). |
| Rolling back | Roll forward past a broker that serves kind secrets, never back. A broker before kinds reads no `kind` tag and would serve a kind secret's stored GitHub App key or refresh token as its value. `agent-secrets secret` writes `mint-service-account: kind-secret` beside every kind, which every broker from legion-envoy-v13.19.0 to the release before kinds refuses as `mint-target-malformed`; a broker older than legion-envoy-v13.19.0 reads no such tag and serves the value. A kind secret written without the guard (by hand, or by a deployment that does not set it) is served by every earlier broker. To go back anyway, first disable every kind secret's versions. |
| The secret store | The agent secrets: every secret whose name starts with `BROKER_SECRETS_PREFIX`, carrying `owner` and `tier` (AWS tags, or Secret Manager annotations), encrypted with the agent-secrets key, and holding a current value. A `kind` tag makes the broker mint a release from that value. [Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) describes the tags, the name each secret is asked for by, and how often the broker rereads them. `BROKER_SECRETS_STORE` names which store, and the two rows below say what each needs. |
| AWS Secrets Manager (`aws-secrets-manager`) | The broker calls AWS with the SDK's default credential chain (environment, shared config, or the workload's role), in a region (`AWS_REGION`, `AWS_DEFAULT_REGION` or the shared config). It needs `secretsmanager:ListSecrets` (which takes no resource, so on `*`), `secretsmanager:DescribeSecret` (to reread one secret) and `secretsmanager:GetSecretValue` on the namespace's secrets, `kms:Decrypt` on the agent-secrets key (`BROKER_SECRETS_KMS_KEY_ARN`), `kms:ListAliases` (on `*`, called only when a secret names its key by an alias), and, when it signs in to RDS by IAM token, `rds-db:connect` on its database user; nothing else. |
| Google Cloud Secret Manager (`gcp-secret-manager`) | The broker calls Secret Manager under [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials): on GKE, Workload Identity Federation for the pod's Kubernetes ServiceAccount, with no key file and no Google service account; the boot log's `secret store … credentials=application-default` line says so. Its principal needs `secretmanager.secrets.list` on the project (the listing takes the project as its parent) and `secretmanager.secrets.get`, `secretmanager.versions.get` and `secretmanager.versions.access` on the namespace's secrets: `roles/secretmanager.viewer` on the project and `roles/secretmanager.secretAccessor` with an IAM condition `resource.name.startsWith("projects/<number>/secrets/<id prefix>")` cover them. The condition narrows only the accessor binding, so the broker reads values from the namespace alone; the viewer role reaches every secret's metadata in the project (its annotations, its versions' state and its IAM policy), never a value. A secret is served while its `versions/latest` is `ENABLED`, encrypted with the key `BROKER_SECRETS_KMS_KEY_NAME` names (`replication.automatic.customerManagedEncryption`; a secret on Google's default encryption is refused `not-on-agent-secrets-key`); one with an `expire_time` is scheduled for deletion and left out, as a Secrets Manager secret in its recovery window is, and a grant made before it no longer reads its value, though Secret Manager still would until that time. Every Secret Manager call but reading a value counts against the project's read quota, [600 a minute, shared by everything in the project](https://docs.cloud.google.com/secret-manager/quotas) (reading a value has its own 90,000). The broker's worst case against it is the sum of: a reload every five minutes, one listing plus one call per secret (a 200-secret namespace, about 40 a minute); the public reread route, two each, capped at one reread every 4 seconds across all callers of **each process** (at two processes that is the same 120 metadata reads a minute for the fleet, which is what the per-process figure is derived from — see [Running more than one pod](#running-more-than-one-pod)); each session's rereads for names it was not served, two each, at most one per session every ten seconds; and value releases, a kind secret's among them, one per distinct secret per 30 seconds (two for a secret whose latest version is disabled), since the broker trusts its last check of a secret, readable or not, for that long (so a delete made outside `agent-secrets secret` stops releases at most 30 seconds after Secret Manager's own metadata shows it, which Google documents as eventually consistent; `agent-secrets secret delete` rereads the name, and the broker then releases nothing more). A busy fleet of sessions requesting unknown names can still spend the quota, and other workloads in the same project draw on it too, so give the agent secrets a project of their own. On the metadata calls the broker retries an `UNAVAILABLE` answer and never a `RESOURCE_EXHAUSTED` one, which would only spend more of that quota; reading a value keeps the client library's own retry of both, against its separate quota. On Secret Manager the agent-secrets key gates no caller: Secret Manager's service agent makes the KMS calls, so writing a secret onto the key and reading it need no KMS permission, and being on the key is the namespace's convention, not a second permission check as it is on AWS. Secret Manager makes the KMS calls itself, so the broker needs no KMS permission; the Secret Manager service agent needs `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key. |
| Kind secrets | A `github-app` secret needs outbound HTTPS access to `api.github.com`, and a `github-owners` tag naming the GitHub accounts it mints for, which the deployment sets when it creates the secret. A `google-login` secret needs outbound HTTPS access to `oauth2.googleapis.com`. The broker reads the stored value for each release, as it reads any secret, and returns only the minted token. It needs no IAM permission to mint these values. A mint GitHub or Google refuses answers the session `502 MINT_FAILED`, naming their reason, and logs `broker: mint failed` (below). |
| A local stand-in (development only) | `BROKER_FAKE_SECRETS_FILE` names a JSON file the broker reads in place of the store, in Secrets Manager's shape whatever `BROKER_SECRETS_STORE` says, each secret's `kms_key_id` naming the agent-secrets key as the selected store configures it (`BROKER_SECRETS_KMS_KEY_ARN`, or `BROKER_SECRETS_KMS_KEY_NAME` with the GCP store); the [configuration reference](/legion/broker/reference/config/#variables-the-broker-reads) gives its format. |
| Dispatch | Dispatch's server calls the broker's approval routes. Set Dispatch's `DISPATCH_AGENT_SECRETS_URL` to the broker's URL and `DISPATCH_AGENT_SECRETS_TOKEN` (or `DISPATCH_AGENT_SECRETS_TOKEN_FILE`) to the same value as the broker's `BROKER_UI_TOKEN`. Without them, Dispatch hides its credential pages. |
| Kubernetes (optional) | To enroll pods, and to let a service log in with its token (`BROKER_SERVICE_LOGINS`), `BROKER_K8S_OIDC_ISSUER` and `BROKER_K8S_OIDC_AUDIENCE` name the cluster's service-account token issuer and the audience the pods' and the services' projected tokens carry. The broker fetches the issuer's discovery document at startup and refuses to start if it cannot. |
| Envoy (optional) | With `BROKER_ENVOY_URL`, the broker tells a waiting agent session, best effort, when its pending request expires. |

## Network and trust

- Agents, their helpers and launchers, and Dispatch's server reach the broker at
  `BROKER_PUBLIC_URL`. Every signed proof names that URL plus the request's path, so clients'
  `AGENT_SECRETS_URL` must be exactly it, and a proxy in front of the broker must not change the
  path. The broker checks each proof against `BROKER_PUBLIC_URL`, never against the `Host`,
  `X-Forwarded-Proto` or `X-Forwarded-Host` a request arrives with: a proxy may rewrite `Host` or
  terminate TLS without setting `X-Forwarded-Proto`, and a client that sends those headers itself
  changes nothing. That holds for the machine login's token endpoint (`POST /v1/oauth/token`)
  too, whose DPoP proofs name `BROKER_PUBLIC_URL/v1/oauth/token`.
- The UI token is an approval credential: whoever holds it can approve as anyone. Give it to
  Dispatch's server and nothing else, and never to an agent.
- Behind a reverse proxy, set `BROKER_TRUSTED_PROXY_HEADER` so the machine-login, token-endpoint
  and secret-reread rate limiters see each caller's address rather than the proxy's. The token
  endpoint (`POST /v1/oauth/token`) has its own per-address bucket in each process, a burst of 60
  refilled at two a second, far above a machine's poll every five seconds and refresh about once a
  day; a refused request is `429` `invalid_request` (`RATE_LIMITED: …`) with `Retry-After`.
- The settings and reread routes take no credential. `GET /v1/settings` answers the store, the
  namespace prefix and, per store, the agent-secrets key's ARN with its AWS account and region, or
  the Cloud KMS key's name with the project number.
  `POST /v1/secrets/{name}/reread` makes the broker read one secret from the store at once,
  and answers whether it now serves it, as which kind (`kind`: `github-app` or `google-login`,
  absent for a stored secret) and, if not, why. Neither releases a value. What a reread
  does tell any caller who can reach the broker is whether a name under the namespace exists, the
  kind it is served as, and, when the broker leaves it out, which of its reasons applies — `absent`,
  or a refusal such as `owner-tag-missing` or `not-on-agent-secrets-key`. That is more than
  `UNKNOWN_SECRET`, which says
  only that no agent secret has the name, says it only to an enrolled session, and never separates a
  secret that does not exist from one the broker refuses; it is what anyone allowed to list the
  namespace reads from the secrets' own names and tags (or annotations).
  Rereads are limited twice, by fixed limits rather than settings (`DefaultRereadLimit`,
  `DefaultRereadOverallLimit` and `GCPRereadOverallLimit` in
  `packages/envoy/internal/broker/api/limits.go`): each source address gets a burst of 30,
  refilled one every 8 seconds, and the broker as a whole takes one a second, burst 5, however
  many addresses the rereads come from — each one takes the policy's writer lock while it reads
  the secret, so that lock, not the caller, is what the second limit bounds. On Google Secret
  Manager, where a reread spends two reads of the project's shared metadata quota, that
  broker-wide limit is one reread every 4 seconds, burst 5. A reread counts against either limit
  only when both let it run, so one address flooding past its own limit leaves the broker-wide
  one to everyone else. Past either, the broker answers `429 RATE_LIMITED` with a `Retry-After`
  header naming the limit that refused: 1 second for the broker-wide limit, 4 seconds for Secret
  Manager's, and 8 seconds for an address's own.
  Each of these buckets is one process's, and the broker-wide pair is sized from what the whole
  fleet spends of the secret store, which is not the same as what it spends in HTTP rereads: one
  reread the broker accepts costs the store **one read per pod**, because the pod that served it
  rereads the secret and then tells every other pod to reread it too. So the fleet reads the
  store `replicas × this rate × replicas` times a second, and each bucket is the fleet's whole
  budget divided by the replica count **squared**. At two pods: `2 pods × 1 reread/s × 2 store
  reads = 4 store reads a second`, which is what one pod alone was allowed, and on Secret
  Manager `2 pods × 0.25 reread/s × 2 store reads × 2 metadata reads = 2 a second`, the same 120
  metadata reads a minute as before. Read those as the arithmetic rather than as the number two:
  a third replica divides the fleet's budget by nine, not by four.
  Their **burst** is divided by the replica count alone, not by its square, because a burst is a
  depth and what it has to admit is one person's write burst, which the replica count does not
  change; what does grow is each pod's own transient, its own burst plus every peer's, so
  `10 / replicas = 5` leaves that where one pod's always was. The **per-address** bucket is
  derived from the shared pair rather than chosen: it refills at half the rate of the tighter of
  the two, so it always takes at least two addresses to hold the fleet's budget at zero. That
  ordering matters because the route takes no credential and a refusal costs a caller nothing,
  so a caller that merely keeps succeeding at the shared refill rate would otherwise starve the
  fan-out for everyone. All three are re-derived together when the replica count changes.
  A reread one pod's notification drives on another spends nothing from either bucket, since the
  pod that served the original call already paid for it — which is what makes the figures above
  the whole of the fleet's store traffic rather than a floor under it
  ([Running more than one pod](#running-more-than-one-pod)). Every other limiter is per process
  and unchanged, so a fleet of N pods allows N times each: this per-address reread bucket, the
  two machine-login buckets, the token endpoint's, and each enrollment's budget of rereads for
  names it was not served.
  `agent-secrets secret` sends one reread after each write it makes, from the person's own
  address, so writing more than five secrets at once meets the broker-wide burst.
  When a limit refuses it, the write stands in the secret store and the CLI exits 1 saying
  so ([manage a secret](/legion/broker/guides/manage-a-secret/#what-the-broker-answers-after-a-write));
  only the narrowing of who may read that secret waits for the next token.
- Agents never see the broker's database or the secret store; whoever can write the database can
  forge a record, so its access control is part of the broker's. Whoever can tag (or annotate) a
  secret under the namespace decides who gets it, so that write access is part of the broker's too.

## People who manage secrets

The broker writes no secret. A person creates, changes and deletes agent secrets with
`agent-secrets secret` ([manage a secret](/legion/broker/guides/manage-a-secret/)), which calls
the store itself under that person's own cloud sign-in - AWS, their own IAM Identity Center
sign-in; GCP, their own Google sign-in: their Application Default Credentials, or their gcloud
login where those are absent or, for a write, a machine's own identity - so that cloud's own IAM
is what decides who may change which secret, and with the tags (AWS) or annotations (GCP), who
gets it. The CLI asks the broker only for its settings (`GET /v1/settings`) and, after each write,
for a reread. Before anything else it refuses, on AWS, a sign-in in another account than the
agent-secrets key's (GCP needs no equivalent: every GCP call already names the project
explicitly, so the API itself refuses one in another project), and it writes under no sign-in but
a person's own; those checks are the CLI's, and the cloud's IAM is the boundary. A person's access
needs each form's permissions on the namespace's secrets, and the tables below say what each
request carries that a policy's conditions can match.

### AWS Secrets Manager

| Form | Permissions | What a condition can match |
| --- | --- | --- |
| every form | `sts:GetCallerIdentity`, which needs no permission | |
| `list` | `secretsmanager:ListSecrets` (on `*`: it takes no resource) | |
| `show` | `secretsmanager:DescribeSecret` | The secret's own tags (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`) |
| `create` | `secretsmanager:CreateSecret` and `secretsmanager:TagResource`, since it tags the secret as it creates it; `kms:GenerateDataKey` and `kms:Decrypt` on the agent-secrets key | The new secret's name under the namespace (its ARN, and `secretsmanager:Name`); the request tags: `aws:RequestTag/owner` (the person's email, from `--owner me`, or `shared`) and `aws:RequestTag/tier` always; `aws:RequestTag/kind` and `aws:RequestTag/mint-service-account` (the rollback guard, `kind-secret`, written beside every kind) with `--kind`; `aws:RequestTag/github-owners` (GitHub logins separated by spaces) with `--kind github-app`, which needs it; and `aws:RequestTag/max-grant` with `--max-grant` (`aws:TagKeys` is the tags sent, so a policy that limits it must allow `owner`, `tier`, `kind`, `mint-service-account`, `github-owners` and `max-grant`); the key, named by its ARN (`secretsmanager:KmsKeyArn`); the `TagResource` check made on the new secret sees the requested tags as its own (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`), so a policy that reserves a shared secret's tags for administrators still needs a statement that admits a shared create (request and resource owner both `shared`, the same tier). A secret created with `--owner shared` and `--tier agent` is served to every agent session without approval, so only a person whose access allows a shared create can create one. |
| `set` | `secretsmanager:PutSecretValue`; `kms:GenerateDataKey` on the agent-secrets key | The secret's own tags (`aws:ResourceTag/owner`) |
| `retag` | `secretsmanager:DescribeSecret`, and `secretsmanager:TagResource`, or `secretsmanager:UntagResource` for `--kind none`, `--github-owners none` or `--max-grant none`. A retag that finds another writer raced it needs both `secretsmanager:TagResource` and `secretsmanager:UntagResource`, whichever it sent, since the restore re-sends the tags it read and removes any the race added | A retag reads the secret's tags, sends one request, then reads them back. A tag request carries owner and tier, the unchanged one re-sent as the secret holds it, plus `kind` and `mint-service-account` (the rollback guard) when the command sets a kind, `github-owners` when it names owners, and `max-grant` when it sets one. An untag request's `aws:TagKeys` is the tags `none` removes: `--kind none` removes `kind` and `mint-service-account` together, `--github-owners none` removes `github-owners`, and `--max-grant none` removes `max-grant`; a retag that removes a tag changes nothing else, so a refused call leaves the secret as it was. A retag that would leave a `github-app` secret without `github-owners` is refused before any write. When the read-back finds another writer's change left a `github-app` secret without `github-owners`, the retag restores the tags it read: a tag request re-sending them, as the secret held them, and an untag request for any tag the race added; then it exits 1. The secret's own tags let a policy allow a person to tag their own secret (`aws:ResourceTag/owner` their email) to themselves or `shared`, and reserve a shared secret's tags for administrators |
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

### GCP Secret Manager

| Form | Permissions |
| --- | --- |
| `list` | `secretmanager.secrets.list` on the project and `secretmanager.versions.get` on each secret (to tell whether its `versions/latest` is `ENABLED`) |
| `show` | `secretmanager.secrets.get` and `secretmanager.versions.list`, `secretmanager.versions.get` (for its versions and whether `versions/latest` is `ENABLED`) |
| `create` | `secretmanager.secrets.create` and `secretmanager.versions.add`. Automatic replication encrypts with the CMEK key through Secret Manager's own service agent, not the person's identity: that service agent (`service-<project number>@gcp-sa-secretmanager.iam.gserviceaccount.com`) needs `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key, granted once at setup, not per person |
| `set` | `secretmanager.versions.add` |
| `retag` | `secretmanager.secrets.get` and `secretmanager.secrets.update`: owner and tier annotations in one call, plus `kind` and `mint-service-account` (the rollback guard) when the command sets a kind, `github-owners` when it names owners, and `max-grant` when it sets one; or, for `none`, the annotations it removes (`--kind none` removes `kind` and `mint-service-account` together), and nothing else |
| `delete` | `secretmanager.secrets.update` (sets `expire_time` to 30 days out) |
| `restore` | `secretmanager.secrets.update` (clears `expire_time`) |

Unlike AWS's tags, a GCP IAM condition cannot test an annotation's value, so GCP has no per-secret
counterpart to AWS's "a person may tag only their own secret" conditions above, and nothing stops
a write from reaching another person's secret: whoever is granted `secretmanager.secrets.update`
on the namespace can retag any secret under it to any owner, tier or kind, shared secrets included,
and whoever is granted `secretmanager.versions.add` can replace any secret's value under it. On
Secret Manager, grant write under the namespace - `secrets.create`, `secrets.update` and
`versions.add` alike - to administrators only, and let people ask an administrator to write on
their behalf.

## Health and logs

`GET /healthz` answers `200 {"status":"ok"}` while the broker can reach Postgres, and
`503 DATABASE_UNAVAILABLE` when it cannot. From the signal that stops it, it answers
`503 SHUTTING_DOWN` instead, for the whole drain, while every other route goes on being served
exactly as before. Use it as a readiness probe and never as a liveness probe: a liveness probe
reads a draining pod as a dead one, so the kubelet would kill the container in the middle of the
drain and drop the requests the drain exists to finish
([Running more than one pod](#running-more-than-one-pod)).

The broker logs text lines to stderr. The ones worth alerting or searching on:

| Log line | Meaning |
| --- | --- |
| `broker stop budget drain=<drain> shutdown=60s total=<their sum>` | The first line of the run, before the database or the secret store is reached for: the whole budget a stop spends, `BROKER_DRAIN_SECONDS` and the 60 seconds in-flight requests get after it. `total` is what a deployment's `terminationGracePeriodSeconds` has to cover, and with the defaults the line reads `drain=5s shutdown=60s total=65s` ([Running more than one pod](#running-more-than-one-pod)). |
| `database sign-in method=<method>` | At startup, before the broker reaches for AWS or its database: `rds-iam` when each connection signs in with an RDS IAM token the broker mints, `password` when it signs in as `BROKER_DATABASE_URL` and libpq's defaults say (a password in the URL, `PGPASSWORD`, a passfile, or none for a server that trusts it). |
| `secret store store=<store> credentials=<source>` | At startup, once the store is built: `aws-secrets-manager` under the AWS SDK's credential chain (`aws-sdk`), `gcp-secret-manager` under Application Default Credentials (`application-default`), or either name over `BROKER_FAKE_SECRETS_FILE` (`fake secrets file`). |
| `broker listening addr=<host:port>` | Startup finished: configuration, migrations and the first read of the namespace all succeeded, and the address is bound. |
| `broker: fatal error=…` | Startup refused; the error names the variable or dependency. The process exits 1. |
| `agent secret policy refused name=<secret name> reason=<reason>` | At ERROR, on every namespace read and each refused reread. Reasons include `owner-tag-missing`, `owner-tag-malformed`, `tier-tag-missing`, `tier-tag-malformed`, `name-malformed`, `service-owner-human-tier`, `not-on-agent-secrets-key`, `no-current-value`, `kind-unknown`, `github-owners-missing`, `github-owners-malformed` and `max-grant-malformed`. |
| `agent secret policy load failed; previous policy kept error=…` | At ERROR: a reread of the namespace failed (the secret store out of reach, or refusing the broker; a Secret Manager failure names its gRPC status code alone, never the status message), and the broker keeps serving the policy from its last good read; or, with `name=<NAME>` (the name a request asked for), the request named a secret the broker did not serve and the broker's reread of that one secret failed, so the request is refused `UNKNOWN_SECRET`. A reread of the namespace in flight when the broker shuts down is not one of these and logs nothing. |
| `broker: <operation> failed error=…` | A request failed with a 500 or 503; the line carries the cause the response does not. A reread whose own request ended before it finished - its caller went away, or it reached the 45-second bound - is not one of these: it answers `503 REQUEST_ENDED` and logs nothing, since anyone can end a request. |
| `broker: mint failed name=<NAME> kind=<kind> class=<class> error=…` | At ERROR, for each release of a kind secret its minter could not make. `class` says whose fix it is: `stored-value`, the secret's value is not a key or login the kind takes, so its owner stores a new one; `refused`, GitHub or Google answered and refused, such as a Google login that needs a new sign-in or a GitHub App with no installation, so its owner signs in again or fixes the App; `upstream`, GitHub or Google could not be asked or answered no usable reply, so check the broker's egress and the upstream's status. The error never holds the stored value or a token. The session is answered `502 MINT_FAILED` with the same error. |
| `broker sweeper: ended an enrollment whose lease lapsed enrollment_id=… kind=… runtime_id=… slot=… lease_expired_at=… grants_revoked=… requests_cancelled=…` | A session stopped renewing (a pod that is gone, a box whose `agent-secrets renew` stopped); the sweep ended it, revoking its grants and cancelling its pending requests. |
| `broker: pod enrollment renewed without its projected token enrollment=… pod_uid=… slot=…` | At WARN: a pod enrollment renewed without its token, and no enrollment naming its pod UID has ever renewed with one, so the broker accepted it; for that pod a token copied out of it still keeps an enrollment alive. It is expected from a pod on a worker image whose `agent-secrets renew` predates `--pod-token-file`. A pod on the current image sends its token from its first renewal, so a line naming such a pod means a holder of an enrollment key for it is renewing outside the cluster before the pod's own first token renewal. List pod images below and replace an older-image pod. If its pod UID is absent or every image is current, end the login that made the line's enrollment, which ends every enrollment it made, once its `subject` shows whether it is a Legion daemon's ([the renew token check](#the-renew-token-check) gives the query and both ways to end it); never pick a machine-login row by its machine name. |
| `broker: pod enrollment renewal refused: projected token required enrollment=…` | At WARN: a pod enrollment renewed without its token after an enrollment naming the same pod UID, this one or another, had renewed with one, and was refused. One cause is someone holding its key, possibly enrolled under another machine login with a token copied out of the pod, renewing outside the cluster. The other is an old-image pod refused because someone else marked its UID first: whoever holds an unexpired copy of an old-image pod's token and a launcher credential that enrolls pods can enroll a key of their own on that UID and renew it with the copy, which marks the UID, and from then on the real old-image pod, whose renewer sends no token, is refused here until its lease lapses. That is weaker than what the copy already allowed, which was to enroll and renew as the pod. The refused enrollment's pod UID names the pod: list pod images (below). If that pod runs an older image, the copy's rows are the marked rows of that pod UID: end each one's login once its `subject` shows whether it is a Legion daemon's ([the renew token check](#the-renew-token-check)), never picking a machine-login row by its machine name, and replace the pod with a current image. Otherwise the copy is the line's own enrollment, as that section says. A current-image renewer that cannot read its token file sends nothing at all, so the broker logs nothing for it; `agent-secrets renew` prints `ERROR: enrollment <id> cannot read its pod token` instead, which the worker shim relays into the pod's log. Its lease and grants end one `BROKER_LEASE_SECONDS` after its last valid renewal. |
| `broker draining drain=<drain>` | SIGTERM or SIGINT: from here on `GET /healthz` answers `503 SHUTTING_DOWN` while every other route is served and the listener stays open, for `BROKER_DRAIN_SECONDS`. A second signal ends the wait at once, logged as `broker: a second signal ended the drain early`. |
| `broker shutting down` | The drain is over: the broker stops accepting, and the requests still in flight get 60 seconds to finish. That is the server's own `WriteTimeout` — the 45-second request deadline plus the 15 seconds a handler needs to write its answer — because it is the longest exchange the broker allows: a request admitted in the drain's last instant still gets its whole deadline. |
| `broker: listening for the other pods' secret rereads channel=…` | The reread fan-out's receiving half has its `LISTEN` connection. The boot waits up to five seconds for this line before it binds, so on a healthy pod it comes just before `broker listening`; after that it reappears on every redial. |
| `broker: a Postgres listener has no connection; redialing channels=… error=… attempt=… wait_ms=…` | At WARN, from `pglisten`: this pod has lost its `LISTEN` connection and is redialing with a backoff that reaches thirty seconds. **This is the only signal that a pod's fan-out is dead** — it goes on answering `/healthz` 200 and serving the whole time, so a retag another pod rereads does not reach it until its own five-minute reload. Repeating lines mean the database is not carrying `LISTEN` sessions for this pod; one or two around a failover are expected. |
| `broker: binding before the reread listener has had an answer waited=5s` | At WARN: the boot's five-second wait for that `LISTEN` ran out, so this pod began serving before it could hear its peers. It serves normally and the listener keeps redialing; what it loses is the notifications sent in the gap, with its own five-minute reload as the backstop. |
| `broker: the other pods were not told to reread a secret secret=… error=…` | At WARN, **not a failed request**: the reread this pod was asked for succeeded and answered 200, and only the Postgres `NOTIFY` that tells the other pods failed. The retag is in force here and reaches the others at their own next reload. |
| `broker: a notification's reread of a secret did not run secret=… sender=… error=…` | At WARN, on no request at all: another pod told this one to reread a secret and the reread failed. This pod goes on serving the tags it last read until its own five-minute reload. |
| `broker: a second signal arrived while the broker was still starting up, so it is stopping without a drain: it has served nothing` | At ERROR: two signals reached a process that had not finished booting, so it exited non-zero without draining. The lines above it say how far the boot got. Expected only when someone or something stopped a pod twice during startup. |
| `broker: signalled while starting up, before it had bound anything stage=…` | One signal reached the process before it bound its socket, so it finished the stage it was in and stopped cleanly, serving nothing and running no drain. |

Each request is bounded to 45 seconds. Request bodies are JSON of at most 1 MiB.

## Running more than one pod

Nothing in a broker process is private to it but its cache of the policy and its rate-limit
buckets, so a deployment may run several identical pods behind one address. What it has to set,
and what it gets:

**The stop budget its grace period must cover.** On SIGTERM the broker drains for
`BROKER_DRAIN_SECONDS` (5 by default, 0 to 120; a second signal ends the wait at once): it answers
`GET /healthz` with `503 SHUTTING_DOWN` while serving every other route exactly as before, and
keeps its listener open, because a pod is removed from its Service's EndpointSlice concurrently
with the kubelet beginning termination rather than before it, so for a while after the signal new
connections are still routed to it. Only then does it stop accepting and give the requests still
in flight 60 seconds. Those 60 are the server's own `WriteTimeout`, the 45-second request deadline
plus the 15 seconds a handler needs to write its answer, because that is the longest exchange the
broker itself allows: a request admitted in the drain's last instant still gets its whole
deadline. So `terminationGracePeriodSeconds` must be at least any preStop pause, plus
`BROKER_DRAIN_SECONDS`, plus 60 seconds — 65 seconds with the defaults and no pause. A preStop
pause and the drain do the same job and are both paid out of the one grace period, so size the
period for their sum, or set the pause to 0 and let the drain do it alone. Past the grace period
the kubelet kills the container whatever the broker still believes it is allowed, and the
in-flight requests the budget exists for are dropped anyway. The broker logs the whole figure at
boot, so nobody adds it up by hand: `broker stop budget drain=5s shutdown=60s total=65s`.

**`/healthz` as the readiness probe, and as nothing else.** A liveness probe on it has the kubelet
kill the container in the middle of the drain, which is what the drain exists to prevent
([Health and logs](#health-and-logs)).

**One more Postgres connection per pod, on a session-mode path.** The fan-out below listens on a
connection of its own, held for as long as the process runs and outside whatever `pool_max_conns`
sizes, since a `LISTEN` ends when a pooled connection is recycled. Count one extra connection per
pod against the database's connection limit, and give the broker a session-mode path to Postgres:
a transaction pooler (pgbouncer in transaction mode, RDS Proxy) hands each transaction whichever
backend is free and carries no long-lived `LISTEN`. A pod that cannot hold the connection still
boots and still serves; its listener redials for as long as it runs.

**A reread that reaches every pod, draining ones included.** `POST /v1/secrets/{name}/reread`
rereads the secret on the pod the load balancer picked and then, on its `200` answer alone, tells
every other pod serving the same database to reread the same name, over Postgres `LISTEN`/`NOTIFY`
on a channel of its own. So one call puts a retag in force across the fleet within seconds rather
than at each pod's own next five-minute reload, and it needs no infrastructure beyond the database
the broker already has. A pod that is draining is still one of those pods: it goes on being sent
requests for the whole drain, so its listener and its five-minute reload both run until the
shutdown has answered the last request, not until the signal. A pod waits for its first successful `LISTEN`
before it binds, up to five seconds, so an address the load balancer can reach is normally an
address a retag can reach; a pod whose `LISTEN` has not succeeded by then logs a warning and
serves anyway, its listener still redialing.
The exception, plainly: while a pod's listener is redialing it hears nothing, and nothing is
caught up when it reconnects, so that pod goes on serving what it last read until its own
five-minute reload. The promise is therefore "within seconds, except while a pod's listener is
redialing, where the next five-minute reload is the backstop". A reread a notification drives
spends no rate-limit token: the pod that served the original call already paid for it.

**The reread limits are three numbers derived from one budget, not one number halved.** One
reread the broker accepts costs the secret store one read on every pod — the pod that served it,
and each peer the fan-out tells — so the fleet's store traffic is `replicas² × the per-pod rate`,
and the two broker-wide buckets' **rate** is the fleet's whole budget divided by that: at two pods
one reread a second, or one every 4 seconds against Google Secret Manager, which leaves the store
exactly the 4 reads a second, and Secret Manager the 120 metadata reads a minute, that one pod
alone was allowed. Their **burst** is divided by the replica count alone, 5 at two pods, because a
burst is a depth and what it admits is one person's write burst, which the replica count does not
change. The **per-address** bucket is derived from them in turn — it refills at half the rate of
the tighter of the two, one every 8 seconds at two pods — so no single address can hold the
fleet's budget at zero on a route that takes no credential. Every other limiter is per process, so
a fleet of N pods allows N times each ([Network and trust](#network-and-trust)). All three are
re-derived together when the replica count changes.

## The renew token check

A pod enrollment is not required to renew with its projected token yet. The broker ratchets each
pod UID instead: every enrollment starts unmarked, its first renewal carrying the pod's token marks
it, and once any enrollment naming a pod UID, live or ended, is marked, a renewal without the token
from every enrollment naming that pod, whichever machine login made it, is refused
`POD_TOKEN_REQUIRED`. A pod on a worker image whose `agent-secrets renew` sends the token
(`--pod-token-file`) marks its UID on its first renewal, so the copied-token gap closes for every
pod UID a current-image pod has renewed under. Until that first renewal a copied token is used
without refusal: seconds, since the Legion daemon logs in with its own token at every start and
enrolls a pod as soon as it is up. A pod that ends before it enrolls never renews with its token,
so its UID stays open as an old-image pod's does. A
pod on an older image never marks its UID: it renews as before, each renewal logged as the first
WARN line above, and the gap stays open for it until the release that requires the token. A pod
UID names one pod for ever, so a mark refuses nothing of another pod, and it keeps refusing a copy
between the pod's role generations and after the pod is gone.

Every deploy order keeps every pod renewing: an older broker ignores the token a newer renewer
sends, and this broker accepts a renewal without one from a pod no enrollment of which has sent
one. That is liveness, not exposure. A grant lasts until its session ends, so a copy renewing an
old-image pod's UID keeps the grant one approval gave it for as long as it renews. So, before or
right after the broker deploys, replace every old-image issue pod: a new worker image does not
replace a running issue pod, so list the pods' images to find them.

```sh
kubectl get pods -n <namespace> -l legion.dev/issue \
  -o custom-columns=POD:.metadata.name,UID:.metadata.uid,IMAGE:.spec.containers[*].image
```

Once that list shows no old-image pod, a token-less WARN line no longer names a pod of yours but a
copy, and for either line the copy is the line's own enrollment: a pod on the current image renews
with its token, so its own rows are the marked ones, and the line names the row that renewed
without it. (Marked rows are a copy's only when the refused pod runs an older image, as the
`projected token required` row above says.) List every enrollment of the line's pod UID with the
machine login that made it; `own` is the line's enrollment, and `subject` is the ServiceAccount a
token login logged in as (none for a login a person approved through a code):

```sql
select o.id = e.id as own, o.id as enrollment, o.pod_token_renewed_at is not null as marked,
       o.revoked_at is null and o.lease_expires_at > now() as live,
       c.id as credential_id, c.service, c.subject, c.host,
       to_char(c.created_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI:SS') || ' UTC' as issued_at,
       (select ev.login from credential_request_events ev
        where ev.record_id = r.id and ev.event = 'approved') as approved_by
from enrollments e
join enrollments o on o.kind = 'pod' and o.runtime_id = e.runtime_id
join launcher_credentials c on c.id = o.launcher_credential_id
left join credential_requests r on r.id = c.record_id
where e.id = '<the line''s enrollment>'
order by o.created_at;
```

Whose login the line's enrollment came from is its `subject`. A Legion daemon logs in only with
its own ServiceAccount's token, so its login's `subject` is an account `BROKER_SERVICE_LOGINS`
names for its service; a login whose `subject` is none, or an account the setting does not name,
is never the daemon's. A login with no `subject` is one a person approved through a code: a
service login from before token logins (it lasts at most `BROKER_LAUNCHER_CREDENTIAL_SECONDS`
after its approval), since a person's own machine login enrolls no pod. Revoke it on Dispatch's
machine-login page (`/credentials/machine`), signed in: find the row whose **Credential** is the
query's `credential_id`, check that its **Approved by** is the query's `approved_by` and that its
**Issued** time, in the tooltip, is the query's `issued_at` (the tooltip shows that second in the
browser's time zone), then click **Revoke** and confirm. That ends every enrollment the login made
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)), and the
broker records the signed-in person as who revoked it. Never pick the row by its machine name: a
login's host is the name its machine reported.

A line whose own enrollment's `subject` is a daemon's account names an enrollment made under that
daemon's one credential: by the daemon itself, renewed by someone holding the enrollment's key from
that pod's role container, or by someone holding a token for the daemon's ServiceAccount. Each
login under the account is in the audit table, with the pod the token was bound to; the daemon's
own token is bound to the daemon's own pod, so a login whose `pod_uid` is another pod's, or none
(a token `kubectl create token` minted), did not use the daemon's own token unless you ran the
daemon off the cluster with such a token:

```sql
select at, detail->>'pod_uid' as pod_uid, detail->>'key_thumbprint' as key, detail->>'expires_at' as expires_at,
       detail->>'token_expires_at' as token_expires_at
from audit
where kind = 'launcher_credential.token_login' and detail->>'credential_id' = '<the query''s credential_id>'
order by at;
```

and compare `pod_uid` with the daemon's pod (`kubectl get pods -n <daemon namespace> -l <the
daemon's labels> -o custom-columns=POD:.metadata.name,UID:.metadata.uid`). A login under the
daemon's account moves the credential to its own key, so a copy's login also turns the daemon's
next call into a refusal and a fresh login of its own, logged by the daemon as `agent-secrets: the
token login failed; retrying` or `logged in with the daemon's ServiceAccount token`; a run of those
while nothing restarted the daemon is a copy fighting it. Replacing the line's pod makes the daemon
revoke its enrollment as it lets the process go. If the pod is gone and the enrollment is still
live (the daemon logs `supervise: agent-secrets: enrollment not revoked after every attempt; the
lease ends it`, which a copy that keeps renewing never lets happen), or a copy holds the daemon's
token, end the daemon's login at once: revoke it on Dispatch's machine-login page by its
**Credential** (the query's `credential_id`, **Approved by** reading `No one:` and the daemon's
account), which ends every pod it enrolled, and, while a copy holds the daemon's token, remove its
entry from `BROKER_SERVICE_LOGINS` and redeploy the broker so neither can log in again. Find and
remove what copied the token before you restore the entry: the broker refuses a token that lives
longer than an hour from its issue, so the copy's token ends at most an hour after it was minted,
and the latest `token_expires_at` above among the logins that are not the daemon's says when.
Pods the revoke ended stay without secrets until they are replaced.

To end a login you know by its code instead, such as a person's machine or a service login from
before token logins, find its row by that code:

```sql
select c.id as credential_id, c.service, c.host,
       to_char(c.created_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI:SS') || ' UTC' as issued_at,
       (select ev.login from credential_request_events ev
        where ev.record_id = r.id and ev.event = 'approved') as approved_by,
       c.revoked_at is not null as revoked
from credential_requests r
join launcher_credentials c on c.record_id = r.id
where r.kind = 'launcher_credential' and r.code = '<the code>';
```

and revoke the row whose **Credential** is that `credential_id`, as above.

The release that requires the token from every pod is safe to deploy only when all three of these
hold:

- this query answers 0. A pod's row is unmarked until its first renewal, seconds after it enrolls,
  so the query leaves out rows made in the last minute, which a busy fleet always holds:

  ```sql
  select count(*) from enrollments
  where kind = 'pod' and revoked_at is null and pod_token_renewed_at is null
    and created_at < now() - interval '1 minute';
  ```

- the broker has logged no `broker: pod enrollment renewed without its projected token` line for
  one full `BROKER_LEASE_SECONDS`.
- the command above lists no issue pod on a worker image older than the one whose renewer sends
  the token. An old-image issue pod between role generations holds no enrollment, so it counts as
  0 and logs nothing, yet its next phase enrolls an unmarked row.

That release refuses every pod renewal without a token, so it also removes what only the ratchet
needs: the pod-UID lookup in `Renew` (`podTokenRatcheted`) and its partial index
`enrollments_pod_token_renewed`.

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
