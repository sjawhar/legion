---
title: Running the listener
description: Getting the Envoy listener, configuring it, the NATS state it owns, GitHub webhooks, health checks, deploys, and the NATS grant a Legion daemon needs.
sidebar:
  order: 3
---

## Getting it

The listener is `envoy-listener`, the entrypoint of the `ghcr.io/sjawhar/legion/envoy:<commit>`
image. Each `legion-envoy-v*` GitHub release also carries it as `legion-envoy-amd64.tar.gz` and
`legion-envoy-arm64.tar.gz`. To build it from a clone of the repository, with Go 1.26 or newer:

```sh
go -C packages/envoy build -o "$PWD/envoy-listener" ./cmd/listener
```

## Configuring it

The listener reads its settings from the environment, and the
[settings reference](/legion/envoy/reference/settings/) lists every one. `envoy-listener settings`
prints the same table from the binary you have. Only two are required: `ENVOY_MACHINE_ID`, the
listener's name, and `NATS_URLS`.

A listener on loopback with no token configured answers every caller of `/v1`. One that listens on
any other address refuses to start until `/v1` has a credential: `ENVOY_API_TOKEN`, which callers
send as a bearer token, or the `ENVOY_OIDC_ISSUER` and `ENVOY_OIDC_AUDIENCE` pair, which accepts
Kubernetes service-account tokens. Keep `/v1` private either way: only the `/webhook/...` routes need
to be reachable from GitHub or Slack.

## The NATS state it owns

Every start of the listener creates or reconciles the `ENVOY_NOTIFICATIONS` stream on its NATS:
the subjects under `notifications.` that are kept, with retention and the duplicate window both 72
hours, in file storage. It also creates the key-value buckets it keeps its state in:

| Bucket | What it holds |
| --- | --- |
| `envoy_interests` | Each session's registered topics. |
| `envoy_sessions` | Each live session's registration, which lapses five minutes after its last write. |
| `envoy_roles` | Each role's claim. |
| `envoy_ci_state` | Each commit's check runs, kept seven days. Only a listener that mounts the GitHub webhook route opens it. |

The listener reads the stream through a durable consumer named `listener-<machine id>` with explicit
acknowledgement, a 60-second acknowledgement wait, at most 256 messages in flight and 20 deliveries
of each. NATS deletes the consumer once no listener has used it for seven days.

`ENVOY_NATS_REPLICAS` sets the replicas of the stream and the buckets it creates. A `NATS_URLS`
naming another machine is refused unless `ENVOY_ALLOW_REMOTE_NATS=1` is set, and
`NATS_NKEY_SEED_FILE` names the NATS user the listener connects as.

## How GitHub events arrive

Set `ENVOY_WEBHOOKS` to include `github`, and point one of these at the listener's public route
`/webhook/github`, for example `https://envoy.internal.example/webhook/github`:

- **The GitHub App's webhook.** Set the App's webhook URL to that route. This is the usual setup:
  one webhook covers every repository the App is installed on, and Dispatch, which holds the App's
  key, asks GitHub to redeliver deliveries that failed.
- **A repository webhook**, for a repository the App is not on.

Either way the webhook's secret must equal the listener's `ENVOY_GITHUB_WEBHOOK_SECRET`, and
`ENVOY_REVIEWER_APP_ID` names the App whose `tester` and `architect` check runs count as verdicts.
The listener checks each delivery's signature, turns it into an event, and publishes it under the
repository's topic:

```text
notifications.github.<owner>.<repo>.pr.<n>            pull request opened, updated, closed, merged
notifications.github.<owner>.<repo>.pr.<n>.comment    comments
notifications.github.<owner>.<repo>.pr.<n>.review     reviews
notifications.github.<owner>.<repo>.pr.<n>.mention    comments that mention the trigger (@legion)
notifications.github.<owner>.<repo>.pr.<n>.checks     a commit's checks have all finished
notifications.github.<owner>.<repo>.push.branch.<ref> a branch push
notifications.github.<owner>.<repo>.workflow.<file>.<action>  a workflow run with no pull request
```

A dot in an owner, repository, or ref name is written `_` in the subject, so `acme/site.io` is
`notifications.github.acme.site_io`. A redelivered webhook is recognised and stored once. Check runs
are recorded rather than published: once a commit's checks have been quiet for `ENVOY_CI_DEBOUNCE`
(five seconds unless set), the listener publishes one `pr.<n>.checks` settlement for that commit.

## How Dispatch's events arrive

Dispatch publishes every change it records to NATS itself, under the thing it belongs to:

```text
notifications.dispatch.issue.<KEY>.<type>                an issue, e.g. issue.updated, ask.answered
notifications.dispatch.document.<PROJECT>.<slug>.<type>  a project document
notifications.dispatch.project.<PROJECT>.<type>          project settings
```

All of them are kept in the stream. Some are also delivered straight to an agent:

- An issue with a route sends its waking events to that role or session.
- Every session that follows an ask gets its answer, edits, resolution, and replies on its own
  inbox topic.

Anything else on an issue reaches an agent only if the agent subscribes to it.
[Dispatch for agents](/legion/dispatch/for-agents/#what-reaches-you) covers this from the agent's
side.

## Health and readiness

`GET /healthz` needs no credential:

| Answer | Meaning |
| --- | --- |
| `200` `starting` | The listener is still binding its durable consumer. |
| `200` `healthy` | Everything answers. The body includes the consumer's lag. |
| `200` `degraded` | A passing key-value failure the listener is retrying. |
| `503` `unhealthy` | NATS is unreachable, the subscription or a cache watcher stopped, or the durable consumer is gone. |

`/v1` opens before `/healthz` leaves `starting`: as soon as NATS is connected and the session and
subscription caches are loaded. Until then `/v1` answers `503` `service starting`. During a rolling
deploy the new listener therefore serves `/v1` while the old one still holds the durable consumer,
and takes the consumer within two seconds of the old one exiting.
[Concepts](/legion/envoy/concepts/#during-a-deploys-overlap) says why no role message is forwarded
twice meanwhile. `GET /metrics` serves Prometheus metrics, also without a credential.

Give the listener about 30 seconds to stop after `SIGTERM`. It stops taking requests, lets
deliveries in progress finish, then exits with a non-zero code so a restart policy brings it back.

## The NATS grant a Legion daemon needs

Legion's daemon reads Dispatch and GitHub events from the stream through two durable consumers
per project, `legion-go-<project>-dispatch` (filtering `notifications.dispatch.issue.>`) and
`legion-go-<project>-github` (filtering each of the project's repositories). It sends its own
notices to roles and to the controller through the listener's HTTP API, not over NATS.

So the NATS user the daemon connects as needs exactly these permissions:

| Publish | Subscribe |
| --- | --- |
| `$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS` | `notifications.envoy.exceptions.notifications.role.>` |
| `$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>` | `_INBOX.>` |
| `$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>` | |
| `$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>` | |
| `$JS.ACK.ENVOY_NOTIFICATIONS.>` | |

The last row is the subject a consumer acknowledges a message on. Replies to the API requests come
back on `_INBOX.>`.

The `$JS.API` subjects are chosen by the NATS client library, not by Legion's code: the same two
consumers written against another client library ask for different ones. So a grant is proven by
running the daemon's consumers under exactly that grant, against a real server, and watching for a
refusal: create both consumers, restart onto them, and have Dispatch and GitHub events
acknowledged. The daemon logs every refusal at error as
`NATS refused the daemon a permission: its NATS user lacks that grant`, with the operation and the
subject. `docs/kubernetes.md` in the repository records the run that verified the list above and
how the daemon's credential is configured.
