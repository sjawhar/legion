---
title: Concepts
description: Sessions and enrollment, machine logins, owner and tier, grants and their lifetime, approvals, and the audit record.
sidebar:
  order: 2
---

This page explains the ideas the rest of the broker's documentation leans on. Each section names
the code that implements it, so you can check the page against the source. A `BROKER_*` name is one
of the broker's settings; the [configuration reference](/legion/broker/reference/config/), generated
from the loader, gives each one's default and accepted values.

## Sessions and enrollment

An **agent session** is one running agent and everything it starts. On a machine, it is the
process `agent-secrets register --exec` starts (a coding agent, or a shell) and every process below
it; in a container or a Kubernetes pod, it is whatever holds the session's key directory. The
broker never deals with a person's or a machine's long-lived key on the agent's behalf. Instead,
every session has a P-256 signing key of its own that lives only as long as the session does, and
an **enrollment** that binds that key to the broker.

Every call a session makes carries a `Proof` header: a JSON Web Signature over the call's method
and URL, signed with the session's key, used once, and valid only within
`BROKER_PROOF_SKEW_SECONDS` of the broker's clock. There is no bearer token for a session to leak.

A **launcher** is the program that enrolls sessions: `agent-secrets-helper`, a per-user daemon on
a machine, for that machine's sessions, the Legion daemon for the pods it runs, or a Kubernetes
pod for itself. An enrollment has one of two kinds:

| Kind | What it is | Who holds its key | Who enrolls it |
| --- | --- | --- | --- |
| `session` | An agent session on a machine a person logged in: running directly on the machine, or in a container on it (a **box**). | For a session on the machine, the machine's helper, in memory; for a box, the container, in `key.pem` under `AGENT_SECRETS_KEY_DIR`. | The machine's helper: when the session registers with `agent-secrets register`, or, for a box, when whoever starts the container runs `agent-secrets enroll --helper` ([run an agent in a container](/legion/broker/guides/run-an-agent-in-a-container/)). |
| `pod` | A Kubernetes pod: a Legion worker pod, or one an operator launches by hand. | The pod, in its key directory. | The Legion daemon, which proves the pod with a projected service-account token the broker verifies against `BROKER_K8S_OIDC_ISSUER`; or the pod itself, with `agent-secrets enroll --pod`, when its ServiceAccount is one `BROKER_SERVICE_LOGINS` names. An enrollment's runtime id must be the pod UID of the token presented with it, which the broker verifies against the cluster's issuer and refuses otherwise, so a pod enrolling itself enrols the pod whose token it holds. |

A machine holding a Google identity token enrols a `session` too: it trades that token at the
broker's token exchange, which answers a Dispatch access token and the machine login that trade
minted, and the machine enrols with that login. That login names the machine's fleet and no
person, so the session acts for the fleet's service: it reads the secrets that service owns and
the shared agent-tier ones, and no person's. The person a fleet's
`BROKER_VM_EXCHANGE_ACCOUNTS` entry names reaches the Dispatch token alone; that person reaches
their own secrets on the machine only by signing in on it.

A person signs in on a machine with `agent-secrets login`, which runs Google's device flow and then
trades the person's own identity token at the exchange with the machine's as the actor token. That
trade mints a machine login of its own, carrying the person rather than the fleet, so a session
enrolled under it is that person's: it reads the secrets that person owns and the shared agent-tier
ones, and no other person's and none of the fleet's. Each sign-in is bound to a box key the command
makes there, so one person signed in on two machines holds a login on each, and two people signed
in on one machine hold one each. Revoking a sign-in on Dispatch ends its machine login with it:
every session it enrolled ends, and the box signs in again to work on.

An enrollment is leased for `BROKER_LEASE_SECONDS` and renewed while its session runs: the helper
renews the sessions it registered, `agent-secrets renew` renews a box, and Legion's pod shim renews
a pod. A pod's renewal carries the pod's current projected token, and the broker refuses it
(`POD_IDENTITY_MISMATCH`) unless the token names the pod UID and service account the enrollment was
made for. The token is not yet required. The broker ratchets each pod UID instead: once any
enrollment naming a pod UID, live or ended, has renewed with that pod's token, the broker refuses a
renewal without one (`POD_TOKEN_REQUIRED`) from every enrollment naming that pod, whichever machine
login made it.
So the copied-token gap closes for every pod UID a current-image pod has renewed under: no
enrollment naming that pod renews without its token, and a token copied out of the pod stops
renewing anything once it expires. It stays open for the UID of a pod on an older worker image,
whose `agent-secrets renew` never sends a token: such a pod renews as it did before, the broker
logging each such renewal, until the release that requires the token. One window is left even for a
current-image pod: a copied token used to renew before the pod's own first renewal with its token.
That is seconds while the Legion daemon holds an approved machine login, but after a daemon restart
its pods enroll only once a person approves the daemon's new login, and a pod that ends before it
enrolls never renews with its token, so its UID stays open as an old-image pod's does. A grant lasts
until its session ends, so a copy that keeps renewing keeps the grant one approval gave it.

A machine's own session is held to its machine login as well: the broker reads that login on every
renewal, and refuses one whose login has ended — revoked, past the hour's expiry its Google
identity token gave it, or its account no longer listed for its service in `BROKER_VM_SERVICES`.
So a box dropped from its fleet's settings, and one that stops being able to prove its identity,
lose their sessions within one `BROKER_LEASE_SECONDS`: the renewal is refused
`ENROLLMENT_NOT_LIVE`, the broker logs `broker: machine session renewal refused: its machine login
has ended`, and the sweep ends the enrollment at its lease. A copy of such a session's key and
enrollment id taken off the box renews nothing once that box's login has ended. Only a machine's
own session is held this way: a pod's renewal proves the pod's own projected token, and a person's
session on such a box runs until they revoke the sign-in, which ends it at once.

An enrollment has **ended** once its launcher revokes it (the helper does
as soon as a registered session's process exits), whoever may revoke its machine login revokes that
login
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)), or its
lease lapses. A `pod` enrollment's launcher is the one machine login that made it: another login,
even another login of the same service, such as the Legion daemon's after a restart or after its
login reached its expiry and it logged in again, is refused
`NOT_ENROLLING_CREDENTIAL`, and that pod's enrollment ends with its lease or with that machine
login. From the moment the lease lapses, its next renewal is refused
`ENROLLMENT_NOT_LIVE`; other session calls remain `PROOF_INVALID`. The broker's sweep, which runs
every `BROKER_SWEEP_SECONDS`, then ends the enrollment on its first run after the lapse.
What an end does to what the enrollment still holds depends on who ended it.
An end a person makes, revoking the machine login or their sign-in on a box, revokes every grant
the enrollment held and cancels every request it still had pending, so those leave the approver's
Inbox and an approval can never land on a session that is gone.
An end of the box leaves both where they are: when the enrollment names a session, records the
principal that session belongs to, and still holds a live grant or a pending request, the
launcher's own revoke and a lapsed lease mark the row revoked and **dormant**, revoking nothing
(`packages/envoy/internal/broker/enroll/enroll.go`, `endEnrollment`).
The row has to record the principal because only the pair, the session name and the principal, is
ever adopted: a row that records no principal can be taken by no later enrollment, so it ends as
every end did before the carry, its grants revoked and its pending requests cancelled
(`enroll.go`, `adopt`).
Every enrollment made before the broker recorded principals has none, among them every enrollment
live on the day the carry ships, so each of those ends as it always did and the enrollment that
replaces it carries.
A dormant enrollment proves nothing from the moment it goes dormant, as any ended enrollment proves
nothing, and its next renewal is refused `ENROLLMENT_NOT_LIVE`; what it holds waits for the next
enrollment of that session name under the same principal to take.
An approval given while the box is gone still mints a grant: the broker hangs it off the row the
carry is keeping for that session, and the session's next enrollment takes it with the rest
(`packages/envoy/internal/broker/requests/machine_state.go`, `ApplyDecision`).
A box is replaced far more often than the session that runs on it, so an end of the box is no end of
the session.
[Requests and grants](#requests-and-grants) says what that next enrollment takes, and what ends a
grant no enrollment comes back for.

Every `session` enrollment records an **operator**: the person whose machine it runs on. The
operator is the person who approved the machine login that enrolled it, never a value the launcher
chooses. A `pod` enrollment records none, since the Legion daemon logs in as a service with its own
Kubernetes token rather than as a person; it records the service account the pod's projected token
proved instead.

### When no broker is serving

A lease that runs out while no broker is serving is not ended. A session renews through a
broker, so while every broker process is down, or none can reach the database, no session can
renew. Every serving broker records a beat every `BROKER_SWEEP_SECONDS`, and a gap of more than
three sweeps between beats counts as time no broker was serving. A lease that was still running
at the last beat stays live through that gap, so the sweep does not end it. The first beat after
the gap gives the gap back to every such lease that was not revoked, at most
`BROKER_LEASE_SECONDS` from then, and the broker logs
`broker: gave back the time no broker was serving …`
(`packages/envoy/internal/broker/enroll/serving.go`, `ServingClock`). The session's renewer then
renews as before. A revoked enrollment stays ended, and so does one whose lease ran out while a
broker was serving. A renewal with another pod's or service account's token is still refused
`POD_IDENTITY_MISMATCH`. A gap of three sweeps is given back to nobody, so a lease has to
outlive one and then the retries its renewer takes to renew once a broker serves again. The
broker refuses to start on a `BROKER_LEASE_SECONDS` too short for its `BROKER_SWEEP_SECONDS`,
and names the shortest lease that is long enough: 91 seconds at a sweep of 1 or 5 seconds, 105
at a sweep of 10, 225 at 30 and 360 at 60.
It walks both renewers' own retry schedules
(`packages/envoy/internal/broker/leasetime`), counting each renewal the outage refuses as
failing at whatever instant costs the renewer most: anywhere from at once, when the connection
is refused, to the 30-second client timeout, when nothing behind a front door ever answers,
with a proxy that gives up or a terminating pod that resets ending one between the two.
Each renewer waits from the instant its failure comes back, so that instant is what decides
where its next renewal falls.
Which renewer sets the floor changes with the sweep: `agent-secrets renew`'s floor grows steadily
with the sweep, while `agent-secrets-helper`, which waits at least 30 seconds after any failed
renewal, stays at 91 seconds up to a 10-second sweep, where an outage can refuse only its first
renewal, and rises faster after that. So `agent-secrets renew` is the stricter of the two only
at sweeps of 7 to 10 seconds, and the helper at every other.
The default lease
of 900 seconds passes at every sweep the broker takes.
A deployment whose `BROKER_LEASE_SECONDS` is below its sweep's floor has to raise it before it
rolls a broker carrying this refusal, since each new pod exits at boot instead of serving.
This covers only an outage of the broker
itself:

- A broker that runs but that sessions cannot reach (ingress, DNS, a NetworkPolicy, a stale
  `AGENT_SECRETS_URL`) still counts as serving, so a lease that runs out then ends as before.
- A grant's lifetime and a pending request's expiry still run on wall-clock time.
- The first deploy of a broker with this behavior restores nothing that ran out before it.
- A pod whose renewer already exited 76 has no renewer until the pod is relaunched.

## Machine login

A launcher (a person's machine helper, or the Legion daemon) cannot enroll anything until the
broker has issued it a **launcher credential**: a credential bound to a key the launcher holds, never
a token, which the launcher uses by signing with that key. A person's machine gets one when that
person approves a **machine login**, an OAuth 2.0 device authorization grant (RFC 8628) on the
broker's own endpoints:

1. The machine generates a fresh key and asks the broker to log in (`agent-secrets machine login`
   asks the helper to, which calls `POST /v1/oauth/device_authorization`), naming the person who
   should approve it (the helper reads that person's Dispatch login from
   `AGENT_SECRETS_OPERATOR_FILE`).
2. The broker answers with an eight-character confirmation code, `XXXX-XXXX`, which the machine
   prints, and the machine polls `POST /v1/oauth/token` for the decision.
3. The person types the code into Dispatch, sees which machine is asking, and approves or denies it.
   Only the typed code selects a machine login: no link can approve one, and anyone else is refused
   `NOT_APPROVER`, since the machine's sessions act as that person.
4. On approval the broker mints the launcher credential bound to the machine's key, and the
   machine's next poll gets a refresh token for it, bound to the same key (DPoP, RFC 9449).

The device grant is served only while the broker is given a page for a person to type the code
into (`BROKER_MACHINE_LOGIN_URL`). A deployment whose machines hold a Google identity leaves that
unset: nothing prints a code, Dispatch serves no page to type one into, and each machine gets its
launcher credential from the trade above instead, with nobody to approve it.

The helper keeps the login (its key, the credential's id and expiry, and the current refresh
token) in `AGENT_SECRETS_MACHINE_FILE` (`~/.config/agent-secrets/machine.json` by default),
readable only by its user, so a restarted helper resumes it. It refreshes the login when it starts,
before it resumes it, and at most a day apart after: each refresh spends the refresh token for a new one and moves the
credential's expiry to `BROKER_LAUNCHER_CREDENTIAL_SECONDS` from then. A refresh token it has
replaced still works for five minutes, so a refresh the broker did not answer (its answer lost, a
timeout, the broker unreachable) or answered with a 5xx is retried every 15 s for as long as that
lasts. Spent after the five minutes, it is a reuse: the broker ends every refresh token of
the login and revokes the machine's credential as a revoke does, ending every session it enrolled,
and the helper holding it drops it at its next refresh. A newer login of the same machine instead
retires the one it replaced: that login can no longer refresh or enroll a session, and the
sessions it enrolled keep running.

A service, such as the Legion daemon, logs in with its own projected Kubernetes ServiceAccount
token instead, and no person approves it: the broker verifies the token as it verifies a pod's,
refuses one that lives longer than an hour from its issue, and issues the credential when the
token's account is one `BROKER_SERVICE_LOGINS` names for that service
([Operate](/legion/broker/operate/#what-to-run)). Each such account holds one credential, which
every login moves to the login's key and to the token's expiry, at most an hour away, so the
service logs in again before then and at every restart; a running daemon proves the same key at
each login, and a restart brings a new one. A device-code login that names a service is refused
`SERVICE_LOGS_IN_WITH_TOKEN`.

A person's launcher credential lapses `BROKER_LAUNCHER_CREDENTIAL_SECONDS` after its approval or
its last refresh, so a machine whose helper keeps reaching the broker stays logged in. It may be
revoked from Dispatch's machine-login page by the person who approved it and no one else, or from
any of their machines under its machine login (`agent-secrets machine revoke`), before or after it
expires, which also ends every session it enrolled and every refresh token it holds
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)). Its
sessions outlive its expiry: a session renews its lease with its own key, and the broker reads no
credential to renew one a person's machine login enrolled, so a box keeps working after the
machine's credential expires, and the page lists an expired login, marked as expired with sessions
still running, until its last session ends. (A machine's own session, enrolled under the login its
Google identity traded for, is the one exception: it renews only while that login is live, as
[Sessions and enrollment](#sessions-and-enrollment) says.) Whoever
approved a person's machine login is its operator. A lapsed or revoked login means logging the
machine in again. A machine login nobody decides expires after 15 minutes, a fixed time rather than
a setting (`machineLoginPendingTTL` in `packages/envoy/cmd/broker/main.go`). A person's machine
logs in through the device grant alone: the earlier `POST /v1/launcher-credentials` login answers
`405 Method Not Allowed` and its poll `GET /v1/launcher-credentials/{pending}` `404`. A service's login has no
operator, so the machine-login page lists it for everyone signed in, and anyone may revoke it, which
ends every pod it enrolled at once: a service login a person approved through a code before
services logged in with their tokens, until it expires, and a service's token login, which names no
approver. A service whose token login was revoked logs in again with its token at its next call,
under a new credential, which enrolls the pods it launches next; a pod the revoke ended stays
without secrets until it is replaced. To stop it logging in, remove its account from
`BROKER_SERVICE_LOGINS` and redeploy the broker.

A credential enrolls sessions only for its own operator: an enrollment naming anyone else is refused
`OPERATOR_MISMATCH`. The broker's rate limiter caps how often anyone can start a machine login, per
source address and per named person, and how often anyone can try a token login: per source
address before the token is verified, and per account once it is. A token login the broker could
not complete (a 5xx, such as while its database is away) spends neither.

## Owner and tier: who may have which secret

The broker reads who may have a secret from the secret itself
(`packages/envoy/internal/broker/policy/`). Every agent secret is a secret in the broker's secret
store (`BROKER_SECRETS_STORE`: AWS Secrets Manager, or Google Cloud Secret Manager) under one
namespace, `BROKER_SECRETS_PREFIX` (such as `production/agent-secrets/`, or
`projects/<project number>/secrets/agent-secrets-`), encrypted with one agent-secrets key
(`BROKER_SECRETS_KMS_KEY_ARN` in AWS KMS, or `BROKER_SECRETS_KMS_KEY_NAME` in Cloud KMS), and
marked with tags or annotations:

- **Its name** under the prefix is the name a session asks for, in lowercase with each underscore a
  hyphen: `production/agent-secrets/deel-api-key`, or
  `projects/<number>/secrets/agent-secrets-deel-api-key`, is `DEEL_API_KEY`. It is lowercase
  letters, digits and single hyphens, starting with a letter.
- **`owner`** is `shared`, a person's email in lowercase, the email they sign in to Dispatch with,
  or a service the broker is configured with (`BROKER_SERVICES` or `BROKER_VM_SERVICES`).
- **`tier`** is `agent` or `human`.
- **`kind`** is absent for a stored secret. It is `github-app` for a GitHub App key,
  `google-login` for a Google authorized-user login, or `google-service-account` for one Google
  service account the broker mints a token as, the account the deployment binds the secret to
  (`BROKER_SERVICE_ACCOUNT_SECRETS`). A secret with a kind must have a current value.
- **`github-owners`** is required on a `github-app` secret, and read on no other kind. It lists
  the GitHub accounts the broker mints installation tokens for, separated by spaces
  (`acme-org ada`) and compared without regard to case, a form both stores hold. A comma, or an
  entry no GitHub login can be (one that does not start with an ASCII letter or digit, holds
  anything but ASCII letters, digits and hyphens, is over 39 characters, or is the word `none`), is
  refused `github-owners-malformed`. A public App can be installed by any account, so an
  installation on an account the list does not
  name gets no token. A `github-app` secret without the tag, or with one that names no account, is
  refused `github-owners-missing`.
- **`requesters`** is optional. It names the only people and services that may ask for the secret
  at all: people by the lowercase email they sign in with, services by the name the deployment
  registers them under, separated by spaces (`ada@example.com legion-daemon`). A session whose
  person the list does not name, and a pod whose service it does not name, is denied
  `requester-not-listed`. A comma, a list that names nobody, an email with a capital letter, or a
  service the deployment does not register is refused `requesters-malformed`. The list narrows
  who may ask and bounds no grant by itself: a secret that is neither `admin` nor
  `google-service-account` keeps whatever `max-grant` it has, or none, so a person's own mail and
  Drive logins stay at their tier and are capped by nothing. A `google-service-account` secret's
  list names its own person owner, once, and the secret is refused `requesters-not-owner`
  otherwise, so a secret owned by `shared` or by a service carries no such kind. A human-tier one
  may name registered services beside that owner, so a service's pod can ask and the owner
  approves each request; an agent-tier one may not, and neither may a secret the deployment binds
  to a `dev-<name>` account, at either tier, since that token is one person's own. Set the list
  with `agent-secrets secret create|retag --requesters "<email> <service>"`.
- **`scopes`** is optional, read on a `google-service-account` secret and refused on any other
  kind (`scopes-kind`). It lists the OAuth scopes the minted token carries, each the part of a
  Google scope after `https://www.googleapis.com/auth/`, separated by spaces
  (`cloud-platform bigquery`). A secret without it mints the `cloud-platform` scope alone, which
  is what every such secret minted before the tag existed, and a changed list reaches the token
  at the next release. The names are the short ones because an AWS tag value holds at most 256
  characters, which five whole scope URLs are longer than. A comma, a list naming no scope, a
  whole scope URL, a name with a capital letter, and a name given twice are each refused
  `scopes-malformed`. Set it with `agent-secrets secret create|retag --scopes "<scope> <scope>"`.
- **`admin`** is optional, and `true` is its only value; anything else is refused
  `admin-malformed`. It marks a secret whose grant is an administrator's, which the broker cannot
  read off the kind, an admin console password and a personal API key both being stored strings.
  Such a secret is human tier, names its `requesters` and bounds its grants to an hour, so a
  person approves every release, only someone the secret names may ask, and the grant is short
  enough to review while it is live; one that is not is refused `admin-tier`,
  `admin-requesters` or `max-grant-required`. Set it with
  `agent-secrets secret create|retag --admin true`.
- **`max-grant`** is optional, except on an `admin` secret and on a `google-service-account` one:
  each of those needs it, of an hour or less, and is refused `max-grant-required` without it. It
  is a Go duration of at least one second. A request for that secret must ask for a lifetime no
  longer than the tag; a request with no `--ttl` asks for the default, 15 minutes when any
  secret it names is human tier and 12 hours otherwise.
- **`default-grant`** is optional. Its one value is `session`: a request with no `--ttl` whose
  secrets all carry it gets a grant that lasts until its session ends instead of that default. Any other
  value is refused `default-grant-malformed`. Set it with `agent-secrets secret create|retag
  --default-grant session`.
- **`mint-service-account`** is written by `agent-secrets secret` beside every kind, with the value
  `kind-secret`, and this broker never reads it. It makes the brokers before kinds, from
  legion-envoy-v13.19.0 on, refuse a kind secret rather than serve its stored key.

In Secret Manager these fields are annotations rather than labels because a label's value cannot
hold an email (`@` and `.` are outside its alphabet), and the broker compares the owner with a
Dispatch login byte for byte. An annotation holds the email as it is.

People write the secrets and their tags, not the broker: `agent-secrets secret` creates and
changes them under the person's own cloud sign-in, both stores
([manage a secret](/legion/broker/guides/manage-a-secret/)).

A `requesters` list is read before owner and tier. Where a secret carries one, a session whose
person it does not name, and a pod whose service it does not name, is denied
`requester-not-listed`, whatever the table below would have given it. An entry holding an `@` is a
person and is matched against the session's person alone; one holding none is a service and is
matched against the service the broker verified alone. A pod has no person and a session has no
service, so each is matched only by an entry of its own sort, and a person whose login reads like
a service name matches no service entry. A secret with no list is every session's to ask for.

Who gets it then follows from those two tags alone:

| The secret | Its owner's own session | Any other session or pod |
| --- | --- | --- |
| A person's, `tier=agent` | Granted at once. | Sent to the owner for approval. |
| A person's, `tier=human` | Sent to the owner for approval. | Sent to the owner for approval. |
| `owner=shared`, `tier=agent` | Granted at once. | Granted at once. |
| `owner=shared`, `tier=human` | Approved by anyone who signs in to the broker. | Approved by anyone who signs in to the broker. |
| A service's, `tier=agent` | Granted at once. | Denied; no one can approve it. |

A session is its owner's own when its operator is the owner: the owner approved the machine login
it enrolled under. A pod has no operator, so a pod asking for a person's agent-tier secret sends it
to that person for approval.

An owner may also be a service the broker is configured with. `BROKER_SERVICES` binds each
service's name to the Kubernetes service accounts its pods run as, one entry per account, for
example `legion-daemon=system:serviceaccount:legion:legion-worker`. A service's own sessions are the
pods that a launcher logged in as that service enrolled, and whose projected token proved one of
that service's accounts: the Legion daemon's worker pods, for `legion-daemon`. A launcher logs in
as a service with its own token, for an account `BROKER_SERVICE_LOGINS` names; the pod's own token
proving the pods' account is what makes a pod the service's.

`BROKER_VM_SERVICES` binds a service to the Google service accounts its machines run as, one entry
per account, each account bound to one service; for each account, `BROKER_VM_PROJECTS` and
`BROKER_VM_INSTANCES` may pin the one Compute Engine project its machines run in and the glob
their instance names match. Those entries are what the broker's token exchange decides a machine's
Google identity token on, and a machine of a bound account is then that service's own session, so
the service's secrets go to it at once.

A service's secrets go to its own sessions and to no one else. That includes a person's own
session, and a pod run under another service account even when a launcher logged in as the service
enrolled it. A service's secret is agent tier, since no person approves it: the broker refuses one
tagged `tier=human`, and one whose owner names a service it is not configured with.

One thing besides the tags changes what a session gets at once: a session's own person, its
operator, who revokes a grant the session got without asking **withholds** its secrets from that
session, even when the grant had already ended by then
(`packages/envoy/internal/broker/requests/machine_state.go`, `RevokeByApprover`). Each
withheld secret is human tier to that session for as long as the session lives: every other grant
the session got it on without asking ends with the revoked one, and the session's requests for it
are sent to the owner for approval, or to anyone who signs in to the broker for a shared secret, so the
session asks before it gets the secret again. Only the session's own person withholds: anyone else
who revokes one of its grants, the person who approved it, ends that grant alone. Other sessions,
the person's own included, still get the secrets at once.

The broker reads the namespace when it starts, and refuses to start when it cannot, then again every
five minutes, a fixed time rather than a setting (`policyRefresh` in
`packages/envoy/cmd/broker/main.go`). A change is served at once when something asks the broker to
reread it: `POST /v1/secrets/{name}/reread`, which `agent-secrets secret` sends after each write it
makes and any caller may send right after writing a secret, and a request naming a secret the
broker does not serve yet, which makes it reread that one name before refusing it (up to ten such
rereads at once per session in each broker process, refilled one every ten seconds:
`DefaultMissRereads` in `packages/envoy/internal/broker/requests/machine_state.go`), so a secret
created a moment ago is served on its first request.

A reread asked for over `POST /v1/secrets/{name}/reread` is the whole broker's, not one process's.
A deployment may run several broker processes against one database, and the one such a reread
reaches tells the others to read that same name again,
so a retag that narrows who may have a secret is in force everywhere within seconds rather than at
each process's own next five-minute read. The miss-path reread above is the other way round: it
stays in the process that served the request, deliberately, since fanning it out would spend every
peer a store read for a name that peer may never be asked for. One exception is worth stating: a process whose
connection to the database has dropped hears nothing while it is reconnecting, and nothing is
replayed to it when it reconnects, so until its own five-minute read it goes on serving what it
last read. That read is the backstop for everything the telling can miss.

A reread reads the one secret from
the store (on AWS, `DescribeSecret`, which shows a write at once; on Google Secret Manager,
`GetSecret`, which Google documents as eventually consistent, so a reread made right after a write
can still find the old state, usually for seconds) and serves it, or stops serving it,
as it finds it; a read of the whole namespace for as long as the store's listing may lag after (five minutes on
AWS Secrets Manager, six hours on Google Secret Manager, whose metadata Google documents as
eventually consistent for up to a few hours) reads that secret again alone, so a listing that has not caught up never undoes it (`Current.Refresh` in
`packages/envoy/internal/broker/policy/current.go`). A reread that finds a secret as the broker
already serves it, or no secret under a name the broker was not serving, changed nothing, and is
not read again — which is the one case that
promise does not cover: a secret created and then deleted before any read served it is anchored by
nothing, so the next read of the namespace can serve it from a listing that shows the creation and
not the deletion, for up to that lag after the delete, and a request for it is granted while a
grant's read of its value is refused `SECRET_NOT_IN_STORE`. A change nothing rereads, such
as a tag edited in the AWS console, takes effect within about ten minutes on AWS: at the next read
of the namespace, or the one after, since Secrets Manager's listing can lag a change by up to five
minutes. On Google Secret Manager it usually takes effect at the next read of the namespace too,
but Google bounds its listing's lag only at a few hours, so a change made any other way than
`agent-secrets secret` (`gcloud`, the console) should be
followed by a reread. A read that fails (Secrets Manager or KMS out of reach) is logged, and the policy from the
last good read stays in force. The broker leaves out a secret it cannot serve, and logs it by name
on every read
([Operating the broker](/legion/broker/operate/#health-and-logs)): a missing or malformed `owner`
or `tier` tag (an email with a capital letter is malformed), an unknown `kind`, a `github-app`
secret whose `github-owners` names no account or holds a comma, a `requesters` list the broker
cannot read (`requesters-malformed`), an `admin` tag set to anything but `true`
(`admin-malformed`), an `admin` secret that is not human tier (`admin-tier`) or that names no
`requesters` (`admin-requesters`), an `admin` or `google-service-account` secret with no
`max-grant` or one longer than an hour (`max-grant-required`), a `google-service-account` secret
whose `requesters` are not its one person owner, or that lists a service where it may not
(`requesters-not-owner`), a `scopes` list the broker cannot read (`scopes-malformed`), a `scopes`
tag on any other kind (`scopes-kind`), a malformed
`max-grant`, a name that is not in the form above, a secret encrypted with any key but the
agent-secrets key, the AWS-managed key included, or a secret with no current value (created without
one, so none of its versions carries the `AWSCURRENT` label). A request for a secret left out is
refused `UNKNOWN_SECRET` before anyone is asked to approve it, and a live grant of it releases
nothing until the broker serves the secret again.

The policy's **version** is the SHA-256 of every served secret's name, owner, tier, source, max
grant, `requesters` list, `scopes` list and `admin` mark, recorded on every request. A kind
secret's source names its kind, so a kind change moves the version too, and so does any change to
a list. A secret with no list writes the same bytes into the digest that it always did.

## Requests and grants

A session asks for one or more secrets by name in a **request**, signed with its key and carrying a
reason of at most 400 characters. The broker evaluates every name
(`packages/envoy/internal/broker/requests/machine_state.go`):

- A name the policy does not serve, even after the reread above, refuses the whole request with
  `UNKNOWN_SECRET`, and nothing is recorded.
- A name the policy denies denies the whole request; nothing is ever half-granted. Only a service's
  secret, asked for by anyone but that service, is denied.
- Names that need approval must all need the same approver, else the request is refused
  `MIXED_APPROVERS`; request them separately.
- A **kind secret** has `kind=github-app`, `kind=google-login` or `kind=google-service-account`,
  and a current stored value. A GitHub App value holds an App identifier and private key.
  Its release returns a JSON object with
  one installation token for each installation of the App that is not suspended and is on an
  account its `github-owners` tag lists. A release can narrow that token, with the `github_app`
  body of `POST /v1/grants/{id}/values` or the exec form's `--github-owner`, `--github-repo` and
  `--github-read-only`: the token of one listed account alone, limited to named repositories of
  that account, to `contents: read`, `metadata: read`, `pull_requests: read` and `actions: read`,
  or to both. An App whose installation has not been granted Actions gets the other three. A token limited to
  repositories alone keeps every permission the App holds, its organization permissions among
  them, so a token for a machine that must only read is limited to read access too. A narrowing
  to an account the tag does not list is refused `NARROWING_OWNER_NOT_LISTED`, and one GitHub
  refuses, such as a repository the installation does not reach, `NARROWING_REFUSED`; the broker
  releases a narrowed token only once GitHub's answer shows it holds no more. It narrows that
  release alone; the grant's next release is owner-wide unless it narrows too. A Google login
  value holds an `authorized_user` refresh-token credential. Its release returns a bare access
  token. A Google service-account value names one account,
  `{"service_account": "<id>@<project>.iam.gserviceaccount.com"}`, and its release returns a bare
  access token for that account: the broker impersonates it with its own Google credential
  through the IAM Service Account Credentials API. The account is the deployment's to choose,
  not the value's: `BROKER_SERVICE_ACCOUNT_SECRETS` binds each such secret to the one account the
  broker may mint for, and a secret it binds to no account, or whose stored value names another
  account than the bound one, releases nothing: the session is answered `MINT_FAILED` and the
  broker logs the failure as `class=binding`, before any call reaches
  Google. The token carries the scopes the secret's `scopes` tag names, or
  `https://www.googleapis.com/auth/cloud-platform` alone where it names none, and lasts an hour
  whatever lifetime the grant that released it has, since Google bounds the token itself: a
  shorter grant stops the next release rather than cutting the token short. That token acts as
  the account itself, which is why the secret is served to the one person who owns it, whom its
  `requesters` tag names, beside the registered services a human-tier one may list, and for
  grants of an hour at most. The broker reads the stored value for each release,
  as it reads any secret, and mints from
  it. It never returns the stored App key or refresh token. A token lasts about an hour, so a client
  keeps the release until its `expires_at` rather than asking again for each use. A deleted kind
  secret stops releasing as any deleted secret does. A release GitHub or Google refuses, such as a
  Google login that needs a new sign-in, is refused `MINT_FAILED` with the upstream's HTTP status
  code, Google's `invalid_rapt` or `rapt_required` subtype (any other is named `unknown`), or the
  class of a failed call, never the upstream's own text, and the broker logs it for its operator.
  A grant ends for good when a release finds its secret's kind changed
  (removed, or changed to another kind) or its lifetime over the current `max-grant`: that
  release answers `GRANT_NOT_LIVE` and writes a `grant.revoked` audit row by
  `broker:release-recheck`, and the session asks again. A release of a stored secret checks the
  policy again once it has read the store, so a retag to a kind reread during that read releases
  nothing and ends the grant the same way: retag a stored secret to a kind, and wait for the broker
  to serve it, before storing its App key or login. A change reverted before any release finds
  it leaves the grant as it was: it was approved for that secret, kind and source, and its next
  release mints a token of that same kind from that same frozen source, never the stored value. To
  end a grant, revoke it. A kind secret the broker stops serving, such as one whose
  `github-owners` is removed or malformed, stops releasing until the secret is served again.
- A secret with `max-grant` needs a lifetime no longer than that duration. A request whose `--ttl`
  (or, with none, the default of 15 minutes for a human-tier secret and 12 hours otherwise, or session length under `default-grant=session`) is longer is refused `GRANT_REQUIRES_LIFETIME` before the broker writes a request. A
  `max-grant` set or lowered later holds what came before it to the new bound: a release that finds
  a live grant lasting longer ends it for good (`GRANT_NOT_LIVE`), and a pending request that asked
  for longer is no one's to approve (`NOT_APPROVER`), so its session cancels it and asks again
  within the bound.
- When every name is automatic, the request is granted at once.
- Otherwise it is **pending**, and the broker writes a credential-request record for the approver to
  decide. A session has at most one pending request for each secret: an identical request from the
  same session while one is pending (the same names and the same lifetime, or none) joins it
  rather than asking twice, and any other request naming one of its secrets, with or without
  `--ttl`, is refused `REQUEST_PENDING`, naming the pending request. The session waits for that
  request to be decided, or cancels it (`agent-secrets cancel`) and asks again. The broker's answer
  names the approver the record names and the request's page on the broker (`approve_url`), and
  `agent-secrets` prints both, so the agent can tell that person where to approve it. The record
  fixes its approver when the request is made, and the request stays in that
  person's Inbox after the secret's tags change, though they may then be unable to approve it
  ([what an owner change does](#approvals)).

A granted request yields a **grant**: the session's right to read those values until the grant
ends. A grant lasts the lifetime its request asks for
(`agent-secrets NAME --ttl 2m -- command`, or `agent-secrets request NAME --ttl 2m`), counted from
the moment it is granted. A request that names no lifetime gets 15 minutes when any secret it names is human tier and 12
hours otherwise, or, when every secret it names carries the tag `default-grant=session`, a grant
that lasts until its session ends. The short human-tier default is the window in which the broker
releases and reuses that credential, so a session that needs it for longer names a `--ttl`, which
the approver reads on the card before deciding. A grant
also always ends with its session, and its session is a session name under one principal,
not the box the request came from.
Any other value of `default-grant` is refused
(`default-grant-malformed`). A secret with `max-grant` requires that shorter
lifetime and bounds it. The approver sees the lifetime the request asked for before deciding it.
While a grant lives, a new request from the same session for exactly the same names and the same
lifetime gets the same grant back, without asking anyone again, as long as the current policy still
allows it.

A grant binds to a session name under one principal.
The box is still there: the grant hangs off one enrollment at a time, and a session reads it only by
proving that enrollment's key.
The session name, with the principal beside it, is what decides which enrollment that is.
The principal is what the enrolling box proved, never anything its launcher states: the person, for
a box of theirs and for their sign-in on a machine; the fleet's Google account, for a machine's own
session; the service account its projected token proved, for a pod
(`packages/envoy/internal/broker/enroll/enroll.go`, `carryPrincipal`).
The broker records which principal holds a name, on the row the token exchange binds every token's
name in, so the name is one principal's whichever of the two claimed it first.
An enrollment that names no session belongs to no principal at all.

Enrolling the same session name again under the same principal takes what every earlier enrollment
of that pair still holds.
Its grants move to the new enrollment, with the requests it is still waiting on a person for and the
names its operator withheld from it, and the move revokes and cancels none of them
(`enroll.go`, `adopt`).
An approval given after the move lands on the session as it stands then, because a decision grants to
the enrollment the request row names
(`packages/envoy/internal/broker/requests/machine_state.go`, `ApplyDecision`).
That is how a grant follows a session to the next box it comes up on: a helper restarts, a machine is
rebuilt, a person signs in on a second machine, a Legion pod is replaced.
Each move is in the audit record, one `enrollment.adopted` row naming both enrollments and how much
moved, and one `grant.moved` row per grant, naming the grant, the enrollment holding it now and the
one it came from.

Three things take nothing.
A session name another principal holds is refused outright: the enrollment is refused
`SESSION_HELD`, naming the session and no holder, and writes nothing
(`enroll.go`, `bindSessionName`).
A token exchange under that name is refused the same way, on the same row, so the name stays its
principal's for 90 days past the last lease or token under it
(`packages/envoy/internal/broker/oauthstore/exchange.go`, `ErrSessionHeld` and `SessionRetention`).
A box whose key a person revoked trades nothing, so it enrols nothing: a launcher proof signed with a
revoked credential does not authenticate, and an enrollment already under way finds the credential
revoked and is refused `LAUNCHER_INVALID`.
A session enrolling under a name of its own has no earlier enrollment of that pair, so it starts with
nothing but the grants it asks for.

A grant that moves still ends, three ways.
It ends when its own lifetime runs out, whichever box holds it by then.
It ends when the carry window closes: an enrollment the box's end left dormant holds what it holds
for `BROKER_SESSION_CARRY_SECONDS`, 12 hours by default, and the sweep then revokes those grants and
cancels those pending requests for good, with an `enrollment.carry_expired` audit row
(`enroll.go`, `EndCarried`).
Those 12 hours are the longest lifetime a request naming none gets (one naming a human-tier secret
gets 15 minutes), so the default carry is never longer than the longest grant given by default; 0
turns the carry off, and the broker refuses a window longer than a day.
And it ends at once when a person revokes the grant itself, the machine login behind it, or their
sign-in on a box.
The two login revokes reach the dormant enrollments that login made as well as its live ones, so a
grant a person has just taken back never waits on the session's next box
(`enroll.go`, `endCredential`).

Within one principal the session name is asserted, not proved.
The name rides in the enrollment request's body, outside the launcher proof, which signs the call's
method and URL and nothing else
(`packages/envoy/internal/broker/proof/proof.go`, `Verify`).
So any holder of a launcher credential of the same person, or of the same Google account, can enrol
under that session's name and take what the session holds.
The session it takes from need not have ended: a carry takes from every enrollment of the pair that
still holds something and has not been ended for good, a live one included
(`enroll.go`, `adopt`).
The box it took from keeps its enrollment and loses the grant: the grant hangs off the new
enrollment now, and the old enrollment's read of it is refused `NOT_YOURS`
(`packages/envoy/internal/broker/requests/machine_read.go`, `Values`).
Taking a grant releases its values at once and asks nobody, where a request for the same human-tier
secret under a name of its own would wait for the person who owns the secret
(`packages/envoy/internal/broker/policy/policy.go`, `Evaluate`).
So a holder of that person's or that account's credential who knows a session's name gets what that
session was approved for, with no approval of its own.
Within one principal what stands between a caller and a secret is the sign-in on a box, or the
account, and never the session name.
Three things bound it.
Another principal is refused the name outright.
Every adoption is in the audit record, naming both enrollments.
And a person's revoke ends the grant at once.

The broker never stores a value. Each time a session reads a grant, the broker checks that the
session is still enrolled, the grant is live, its approval still verifies, and the current policy
still allows every name; then it reads the value from the secret store (AWS Secrets Manager in
production) and returns it to that session alone. `agent-secrets NAME -- command` puts each value
in the command's environment under its name and replaces itself with the command. `agent-secrets`
itself never prints a value, but the command is the agent's choice, and a command can print or send
what its environment holds (`printenv NAME` prints it). A session holding a grant can therefore read
the value: approve a secret only for a session you would trust with the value itself.

A grant ends when its lifetime runs out, when its session revokes it (`agent-secrets revoke`), when
its approver or its enrollment's operator revokes it in Dispatch or from one of their machines
(`agent-secrets grant revoke`), and when the enrollment holding it ends for good: a person's revoke
of the machine login or the sign-in behind it, or the carry window closing on an enrollment the
box's end left dormant.
A grant the session got without asking
is revoked like any other, and when its
operator revokes it the session asks for approval before it gets the same secrets again (the operator
[withholds](#owner-and-tier-who-may-have-which-secret) them, even when the grant had already
ended); a session revoking its own grant, and anyone but the operator revoking one, withholds
nothing. To end every session's automatic access to a secret, change its tags
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/#end-every-sessions-access-to-a-secret)).

## Approvals

A secret request is approved on the broker's own approval page, never in Dispatch. The broker holds
no Dispatch credential and sends Dispatch nothing: Dispatch's server reads the broker's pending list
for the person signed in, and its Inbox lists each secret request as a link to that request's page
on the broker, `<broker>/approve/<record>`, which the broker's answers name as `approve_url`. The
page signs the person in to the broker itself, with OpenID Connect and the broker's own client (the
deployment's Google Workspace, `BROKER_SIGNIN_*` in
[Operate](/legion/broker/operate/#approval-sign-in)), only when they hold no live sign-in there,
and brings them back to the same request. It prints the request from the broker's own record: the
secret names, the enrollment the broker verified asked (its kind, operator, runtime and worker
slot), the session the request names when it differs, marked as the request's own claim, the
reason, the lifetime, the times, the approver and the state; then every other request waiting on
that person. Each request the person may decide has **Approve** and **Deny**, and one **Approve
all N shown** decides every request printed. A decision is posted to the broker's own origin and
is bound to that sign-in and to the requests the page printed, so a page on another origin,
Dispatch's among them, cannot make one, and no route a service calls decides a secret request: the
broker refuses it `DECIDE_ON_BROKER_PAGE`, naming the page.

The broker checks the signed-in person against the record's approver: anyone else gets a page
saying who may decide it, with a link to sign in as someone else, and no buttons. A request for a
shared
human-tier secret, or for a shared agent-tier secret its session's operator withheld from it,
names the approver `anyone`: it waits in every person's Inbox, anyone who signs in to the broker may
decide it, and the
broker records the email of whoever did. A withheld secret is its owner's to approve for that
session even in a request that was already waiting when the operator withheld it: a request that
waited on anyone for a shared human-tier secret and the operator's own withheld secret is then the
operator's alone to approve, and anyone else's approval is refused `NOT_APPROVER`. While the broker
does not serve the withheld secret to that session, no one may approve such a request: not while
the secret is out of the broker's policy (deleted, or its tags refused), since its owner cannot be
read, nor while it is a service's, since no person approves a service's secret. It waits until the
secret is a person's or shared again, or expires. The approval it gets lasts as any approval does
(below): a change to another secret's tags does not end it, nor does the operator's later revoke of
another grant, unless that revoke withholds another secret the request got without asking.

An approval belongs to the person who gave it, so a change to a secret's tags reaches what was
approved before it wherever the new tags want the secret approved for that session:

- An approved grant keeps releasing its value only while the person who approved it may still
  approve the secret for its session. Once a shared human-tier secret becomes a person's, or a
  person's secret becomes another's, every such grant the new owner must approve and did not stops
  with `GRANT_NOT_LIVE`, whatever value the secret holds by then, and the session's next request is
  decided under the new tags. A grant keeps working when the secret becomes shared (anyone may
  approve a shared human-tier secret, and a shared agent-tier one needs no approval), and when the
  new tags give its session the secret without asking, as they do once an agent-tier secret is the
  session's operator's. A secret the operator withheld from the session stays human tier there
  under any tags, so a grant of it someone else approved stops once the secret is the operator's.
- A pending request is approved by whomever the new tags name to approve it for its session. One
  waiting on anyone for a secret that has become a person's is that person's alone to approve;
  anyone else's approval is refused `NOT_APPROVER`, the requester's own operator included. One
  waiting on a person for a secret another person must now approve can be approved by no one. One
  waiting on a person stays that person's when the secret becomes shared, or when the new tags give
  its session the secret without asking. A denial releases nothing, so the approver a request waits
  on can still deny it, which takes it off the pending list: for a request waiting on anyone, any
  signed-in person may deny it. Otherwise it expires, or its session cancels it
  (`agent-secrets cancel`) and asks again.

A record is decided once. A second click, a concurrent one, or one after the record expired changes
nothing (`RECORD_TERMINAL`), and the page says what became of the record. A pending request nobody
decides expires after 12 hours, a fixed time rather than
a setting (`agentSecretPendingTTL` in `packages/envoy/cmd/broker/main.go`).

A machine login is still decided in Dispatch, with its typed code
([Machine login](#machine-login)), and a person's revoke of a grant or a machine login is still made
through Dispatch: Dispatch's server calls the broker as its own Kubernetes ServiceAccount, which the
broker lists as a caller (`BROKER_API_ACCOUNTS`), with that person's email from their Dispatch
session, and the broker takes that name on Dispatch's word. A listed account's token can therefore
approve a machine login and revoke in any person's name, though never approve a secret request:
list Dispatch's own account and nothing else.

## The audit record

Everything a person decides rests on a **credential-request record**
(`packages/envoy/internal/broker/record/record.go`): a fixed, line-by-line text holding the
session's signed request verbatim, the approver, the enrollment's kind, runtime id and operator,
the lifetime (`-` for a grant that lasts until its session ends), the policy version, the expiry,
and, for a machine login, its confirmation code. The
record's id is the SHA-256 of that text, so a record cannot change without changing its id.
Decisions are events on the record (`approved`, `denied`, `expired`, `cancelled`, `revoked`), each
naming who made it.

Every time the broker releases a value or authenticates a launcher credential, it re-verifies the
whole chain: the record still reproduces its id, the session's signature inside it still verifies,
and exactly one approval by the record's approver stands behind it. A grant or credential with no
approved record behind it releases nothing.

Beside the records, the broker keeps an append-only `audit` table with one row per event:
`enrollment.created`, `enrollment.revoked`, `enrollment.expired`, `request.created`,
`request.granted`, `request.denied`, `request.cancelled`, `request.expired`, `grant.used` (each time
a session reads a grant, naming the secrets released), `grant.revoked` (naming, under `withheld`,
the secrets an operator's revoke withheld from the session, and, on each other grant of the session
that revoke ended, the withheld secrets that grant held), `grant.withheld` (an operator's revoke of
a grant already revoked, naming under `withheld` the secrets it withheld from the session) and
`launcher_credential.revoked` (naming the machine login's `credential_id`, its `host`, its
`service` for a service's login, and under `enrollments` the sessions the revoke ended) and
`launcher_credential.token_login` (a service's token login: the `credential_id`, its `service`, the
token's `subject`, the `pod_uid` the token is bound to when it is, the login's `key_thumbprint`,
the credential's `expires_at` and the token's own `token_expires_at`). Each row names its actor:
`human:<email>` for a decision a person made on the broker's approval page or a change Dispatch
made for its signed-in person, `session:<enrollment id>` for
a session's own, `launcher:<credential id>` for one
a machine login made under its own credential, a person's revoke from their machine and a
service's token login included, or `broker`. No
record, event or audit row ever holds a secret value.
[Operating the broker](/legion/broker/operate/#the-audit-record) shows how to read them.

A quarterly **access review** reads those rows over `GET /v1/grant-log`, the one route the caller
name `access-review` reaches (`BROKER_API_ACCOUNTS`; the names `dispatch` and `listener` do not
reach it, and the route changes nothing). It answers every `audit` row of a closed period, in the
order the rows were written, each event naming the session that asked, the person behind it, the
service the broker verified for that session and the one its credential claimed, the secrets and
how each was decided, the approver, who decided, the lifetime, the reason the session signed, and
the detail of a refusal. A period's end must be at least an hour ago, so that every row of it has
committed before the review reads it. Each decision and each grant end is also one INFO line in
the broker's log, `broker: grant log`, so a period can be read from the log pipeline without the
database.

Three things the log does not say, which a reviewer has to be told:

- A grant that ended because its enrollment ended writes no `grant.revoked` row. The end is the
  enrollment's own row, `enrollment.revoked` or `enrollment.expired`, which does not name the
  grants one by one.
- A grant that reaches its own `expires_at` writes no row at all. What says when it ended is its
  `request.granted` row and that row's `lifetime_seconds`.
- A minted token — `github-app`, `google-login` and `google-service-account` alike — lives its own
  hour from the moment it was minted. Revoking the grant stops the next release; it does not
  revoke a token the broker has already released.

The review reads the `audit` table as it stands. It adds no audit kind and writes no audit row of
its own: every row it reads is one the broker wrote when it decided.
