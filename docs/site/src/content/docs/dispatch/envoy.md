---
title: "Envoy: how events reach Dispatch, Legion and the agents"
description: The Envoy listener and NATS under it, how GitHub webhooks and Dispatch events reach subscribers, topics, the NATS grant a Legion daemon needs, running the listener, and running Envoy on one machine.
sidebar:
  label: Envoy
  order: 9
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
notifications.github.<owner>.<repo>.job.<run_id>.completed    a finished Actions job on the default branch, of a run with no pull request
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
- A person's reply to a comment reaches the session that started its thread, and a person's new
  comment on a document reaches every session that wrote the document, each on its own inbox
  topic.

Anything else on an issue reaches an agent only if the agent subscribes to it.
[Dispatch for agents](/legion/dispatch/for-agents/#what-reaches-you) covers this from the agent's
side.

## Topics and subscriptions

| Topic | What it is |
| --- | --- |
| `notifications.agent.<session_id>` | One session's inbox. Direct messages, ask follow-ups, replies to its comments, comments on documents it wrote, and claim changes arrive here. |
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
| `ENVOY_NATS_SOCKS5_PROXY` | A SOCKS5 proxy, as `host:port` (for example `127.0.0.1:1055`), that every NATS connection goes through, each server's host name handed to the proxy unresolved. Needs `ENVOY_ALLOW_REMOTE_NATS=1`, since the proxy decides which server a connection reaches. |
| `ENVOY_LISTEN_HOST`, `PORT` | Where to listen. Defaults: `127.0.0.1` and `9020`. |
| `ENVOY_API_TOKEN` | The bearer token `/v1` callers must send. |
| `ENVOY_OIDC_ISSUER`, `ENVOY_OIDC_AUDIENCE` | Also accept Kubernetes service-account tokens from that issuer for that audience. Set both or neither. |
| `ENVOY_ALLOWED_HOSTS` | Comma-separated host names or addresses, with no port, that a listener with no bearer serves `/v1` to besides loopback. Set it to each name clients reach such a listener by, such as a forward to it. |
| `ENVOY_WEBHOOKS` | Which webhook routes to mount, for example `github,slack`. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | Required with `github`: the webhook secret. |
| `ENVOY_REVIEWER_APP_ID` | Required with `github`: the id of the GitHub App that posts reviews. |
| `ENVOY_GITHUB_MENTION_TRIGGER` | The mention that publishes a `.mention` event. Defaults to `@legion`. |

A listener on anything but loopback refuses to start without `ENVOY_API_TOKEN` or the OIDC pair.
A listener with neither serves `/v1` only to a request whose `Host` is `localhost`, a loopback
address or a name in `ENVOY_ALLOWED_HOSTS`, and answers any other `Host` with a JSON `421`, so a
page that rebinds its own name to the listener's address cannot use the API.
Keep the API private: only `/webhook/...` needs to be reachable from GitHub.

### With Pulumi

The `envoy-infra` Pulumi plugin runs the listener on a Docker host from a Pulumi program in any
language, as the component `envoy-infra:host:Listener`, and a NATS server for it on Google Cloud,
as `envoy-infra:gcp:NatsHub`. Its
[README](https://github.com/sjawhar/legion/blob/main/packages/envoy/infra/README.md) says how to
install it, what each component deploys and which settings it sets.

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

## Running Envoy on one machine

`packages/envoy/deploy/compose/single-node.compose.yml` runs a NATS server with JetStream of its
own and the listener in front of it, both on `127.0.0.1` only. The agent sessions on that machine
register, subscribe, claim roles and send each other messages through it. Nothing on another
machine reaches it, and no GitHub or Slack webhook arrives, so no pull request or check events
reach the sessions. NATS takes no credential, so every user on the machine can read and send
messages through it: run it on a machine whose users you trust.

You need Linux with Docker Engine and the Compose plugin (`docker compose version` answers), and a
checkout of this repository; the images are public. The two services listen on `4222` (NATS),
`8222` (NATS monitoring) and `9020` (the listener). When something else on the machine uses one,
move it in `.env` (`NATS_PORT`, `NATS_MONITOR_PORT`, `ENVOY_LISTENER_PORT`) and use that port
wherever a command or the `envoy.json` below names the default.

1. Write the machine's settings:

   ```sh
   cd packages/envoy/deploy/compose
   cp -n single-node.env.example .env
   ```

   Set `ENVOY_MACHINE_ID` in `.env` to what `hostname -s` prints, with no `.` in it; the file says
   what its other settings do. The other compose files here read the same `.env`, so `cp -n` leaves
   one that already exists alone (it may print `not replacing`): add to it the template's settings
   it lacks.

2. Start it:

   ```sh
   docker compose -f single-node.compose.yml up -d --wait
   curl http://127.0.0.1:9020/healthz
   ```

   `--wait` returns once both containers pass their health checks. The answer's `status` is then
   `healthy`, or `starting` for a few seconds ([Health and readiness](#health-and-readiness)). Both
   restart with Docker unless you stop them. `up` refuses an empty `ENVOY_MACHINE_ID` and an
   `ENVOY_IMAGE_TAG` that was never published; when a container stays unhealthy instead (a port
   already in use, say), `docker compose -f single-node.compose.yml logs --tail 50` says why.

3. Point the sessions at it in `~/.config/legion/envoy.json` (`mkdir -p ~/.config/legion` first):

   ```json
   {
     "dispatch": { "enabled": false },
     "envoy": {
       "enabled": true,
       "url": "http://127.0.0.1:9020",
       "natsUrls": ["nats://127.0.0.1:4222"]
     }
   }
   ```

   `"dispatch": { "enabled": false }` is for a machine with no Dispatch server; one that has a
   server keeps its `dispatch` section. When `.env` sets `ENVOY_API_TOKEN`, put the same value in
   `envoy.token`. A session under its own Oh My Pi profile, or with `ENVOY_CONFIG_FILE` set, reads
   another file; [Connecting an agent](/legion/dispatch/for-agents/#connecting-an-agent) says which.

4. Start an Oh My Pi session with `@sjawhar/pi-envoy` 10.0.0 or later
   (`omp plugin install @sjawhar/pi-envoy`), then `omp`. A session reads `envoy.json` when it
   starts, so restart any session that was already running; `/reload-plugins` does not reload it.
   Check that it registered:

   ```sh
   curl http://127.0.0.1:9020/v1/sessions
   ```

   With `ENVOY_API_TOKEN` set, add `-H "Authorization: Bearer <token>"`. The answer lists each live
   session with its `session_id`. Start a second session, and the two can message each other with
   `envoy_send`.

NATS keeps the event stream and Envoy's key-value buckets in the `envoy-single-node_nats-data`
volume. A restart keeps the last 72 hours of events, and the subscriptions and role claims of every
session still running. A running session renews its registration every two minutes; once it stops,
Envoy drops its subscriptions and role claims within minutes.

| Task | Command, from `packages/envoy/deploy/compose` |
| --- | --- |
| Upgrade to the newest published image | `docker compose -f single-node.compose.yml pull && docker compose -f single-node.compose.yml up -d --wait` |
| Read the logs | `docker compose -f single-node.compose.yml logs -f` |
| Stop it, keeping its data | `docker compose -f single-node.compose.yml down` |
| Stop it and delete its data | `docker compose -f single-node.compose.yml down -v` |
