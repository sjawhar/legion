---
title: "Envoy: how events reach Dispatch, Legion and the agents"
description: The Envoy listener and NATS under it, how GitHub webhooks and Dispatch events reach subscribers, topics, the NATS grant a Legion daemon needs, and running the listener.
sidebar:
  label: Envoy
  order: 8
---

Envoy carries events between GitHub, Dispatch, Legion, and agent sessions. It has two parts:

- **NATS JetStream** holds the events. Every event is a message on a subject under
  `notifications.`, kept for 72 hours in the `ENVOY_NOTIFICATIONS` stream. Envoy also keeps its
  own state in JetStream key-value buckets: which session is subscribed to what (`envoy_interests`),
  which sessions are live (`envoy_sessions`), who holds each role (`envoy_roles`), and pull
  request check results (`envoy_ci_state`).
- **The listener** is an HTTP service in front of it. It takes webhooks in, publishes them as
  events, and delivers events to the agent sessions that subscribed to them. Agents and Legion
  call its `/v1` API to subscribe, publish, claim roles, and send messages.

## How GitHub events arrive

GitHub sends webhooks to the listener's public route `/webhook/github`, for example
`https://envoy.internal.example/webhook/github`. Point one of these at it:

- **The GitHub App's webhook.** Set the App's webhook URL to that route. This is the usual
  setup: one webhook covers every repository the App is installed on, and Dispatch, which holds the
  App's key, asks GitHub to redeliver deliveries that failed.
- **A repository webhook**, for a repository the App is not on.

Either way the webhook's secret must equal the listener's `ENVOY_GITHUB_WEBHOOK_SECRET`. The
listener checks each delivery's signature, turns it into an event, and publishes it under the
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
`notifications.github.acme.site_io`. A redelivered webhook is recognised and stored once.

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

## Topics and subscriptions

| Topic | What it is |
| --- | --- |
| `notifications.agent.<session_id>` | One session's inbox. Direct messages, ask follow-ups, and claim changes arrive here. |
| `notifications.role.<role>` | A role, held by one live session at a time. You publish to it; the listener hands each message to the current holder. |
| `notifications.dispatch.issue.<KEY>.>` | Every event of one Dispatch issue. |
| `notifications.github.<owner>.<repo>.pr.<n>.>` | Every event of one pull request. |
| `notifications.legion.<project>.controller` | A Legion project's controller. |
| `notifications.envoy.exceptions.<topic>` | A delivery to `<topic>` that failed, with the reason. |

An agent subscribes with Envoy's `envoy_subscribe` tool, which stores the subscription in the
listener. A subscription to `<subject>.>` also covers `<subject>` itself, so subscribing to a pull
request's `.>` gets its lifecycle events as well as its comments and checks.

A role message is not stored for later. If no live session holds the role, a publish through the
API is refused with `404`. If the holder does not take a message, the listener publishes an
exception naming the reason: `no_holder`, `delivery_failed`, or `receipt_timeout`.

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

## Running the listener

The listener is `envoy-listener` in the `ghcr.io/sjawhar/legion/envoy` image.

| Setting | What it is |
| --- | --- |
| `ENVOY_MACHINE_ID` | Required. Names this listener; its durable consumer is `listener-<machine id>`. |
| `NATS_URLS` | Required. Comma-separated NATS addresses, such as `nats://<nats-host>:4222`. |
| `NATS_NKEY_SEED_FILE` | The NATS user to connect as, as a file holding its seed. |
| `ENVOY_ALLOW_REMOTE_NATS` | Set to `1` when NATS runs on another machine; otherwise the listener refuses it. |
| `ENVOY_LISTEN_HOST`, `PORT` | Where to listen. Defaults: `127.0.0.1` and `9020`. |
| `ENVOY_API_TOKEN` | The bearer token `/v1` callers must send. |
| `ENVOY_OIDC_ISSUER`, `ENVOY_OIDC_AUDIENCE` | Also accept Kubernetes service-account tokens from that issuer for that audience. Set both or neither. |
| `ENVOY_WEBHOOKS` | Which webhook routes to mount, for example `github,slack`. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | Required with `github`: the webhook secret. |
| `ENVOY_REVIEWER_APP_ID` | Required with `github`: the id of the GitHub App that posts reviews. |
| `ENVOY_GITHUB_MENTION_TRIGGER` | The mention that publishes a `.mention` event. Defaults to `@legion`. |

A listener on anything but loopback refuses to start without `ENVOY_API_TOKEN` or the OIDC pair.
Keep the API private: only `/webhook/...` needs to be reachable from GitHub.

### Health and readiness

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

Give the listener about 30 seconds to stop after `SIGTERM`. It stops taking requests, lets
deliveries in progress finish, then exits with a non-zero code so a restart policy brings it back.
