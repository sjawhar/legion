---
title: "Envoy: how events reach Dispatch, Legion and the agents"
description: The Envoy listener and NATS under it, how GitHub webhooks, Slack mentions and Dispatch events reach subscribers, replying in Slack, topics, the NATS grant a Legion daemon needs, running the listener, and running Envoy on one machine.
sidebar:
  label: Envoy
  order: 9
---

Envoy carries events between GitHub, Slack, Dispatch, Legion, and agent sessions. It has two parts:

- **NATS JetStream** holds the events. Events are messages on subjects under `notifications.`;
  the `ENVOY_NOTIFICATIONS` stream keeps those of GitHub, Slack, Dispatch, Legion and each
  session's inbox for 72 hours, and team topics (`notifications.team.<name>`) with them. Role
  messages travel over core NATS and are not kept for a later subscriber, but the listener's
  forward of a role message to its current holder is kept on that holder's inbox. Envoy also keeps
  its own state in JetStream key-value buckets:
  which session is subscribed to what (`envoy_interests`), which sessions are live
  (`envoy_sessions`), who holds each role (`envoy_roles`), and pull request check results
  (`envoy_ci_state`). The stream and every bucket are kept in at least as many copies as the
  deployment's `ENVOY_NATS_REPLICAS` says, when every Envoy program of the deployment carries the
  same value ([Keeping the stream](#keeping-the-stream)).
- **The listener** is an HTTP service in front of it. It takes webhooks in, publishes them as
  events, and delivers events to the agent sessions that subscribed to them. Agents and Legion
  call its `/v1` API to subscribe, publish, claim roles, and send messages. For each session it
  keeps a durable consumer on the stream and delivers the stored messages from it in order, each
  marked delivered only when the session has it, so a session that was down or disconnected gets
  them when it is back ([Messages a session missed](#messages-a-session-missed)).

## How GitHub events arrive

GitHub sends webhooks to the listener's public route `/webhook/github`, for example
`https://envoy.internal.example/webhook/github`. Point one of these at it:

- **The GitHub App's webhook.** Set the App's webhook URL to that route. This is the usual
  setup: one webhook covers every repository the App is installed on, and Dispatch, which holds the
  App's key, asks GitHub to redeliver deliveries that failed. `scripts/github-apps/envoy.json` is
  that App's manifest, its events and its read-only permissions, which
  [Running Legion](/legion/legion/running-legion/#the-github-apps) registers.
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

## How Slack mentions arrive, and how an agent replies

A Slack app sends its events to the listener's public route `/webhook/slack`, which the listener
mounts when `ENVOY_WEBHOOKS` lists `slack`. Set the app's Event Subscriptions request URL to it, for
example `https://envoy.internal.example/webhook/slack`, and subscribe it to the `app_mention` bot
event (its `app_mentions:read` scope). The listener answers Slack's `url_verification` challenge,
and checks every other delivery's signature against `ENVOY_SLACK_SIGNING_SECRET`, the app's
signing secret, answering `401` to one that does not match. The repository's
`deploy/slack/legion-app-manifest.yaml` creates such an app: the `app_mentions:read` and
`chat:write` bot scopes and the `app_mention` bot event.

A mention of the app is published under the Slack team's and channel's ids, and a mention inside a
thread a second time under the thread's root, its timestamp's dot written `_`:

```text
notifications.slack.<team>.<channel>.mention                  every mention in the channel
notifications.slack.<team>.<channel>.thread.<root ts>.mention  a mention inside that thread
```

Its payload carries `channel_id`, `user_id`, `text`, `ts`, `thread_ts` (inside a thread) and
`bot_id` (when a bot posted it). The controller the Legion daemon launches
(`controller: daemon`) subscribes to `notifications.slack.*.<channel>.mention` for each channel its
project's `slack_channels` names, for as long as it holds the controller role.

An agent replies in a thread with the `slack_post` tool, as the same Slack app. The listener holds
no Slack token: the tool asks the [Secrets Broker](/legion/broker/) for the agent secret
`LEGION_SLACK_BOT_TOKEN`, the app's bot token (`xoxb-…`), through the broker's own client
(`agent-secrets LEGION_SLACK_BOT_TOKEN --reason … --wait 0s -- <poster>`), which puts it in the
environment of the one process that calls Slack's `chat.postMessage`, never the session's. Store
the token in the broker's store with owner `legion-daemon` and tier `agent`, so the broker grants it
at once to the pods the Legion daemon enrolls under the accounts `BROKER_SERVICES` binds to
`legion-daemon`, its controller's included, and to no one else; the app needs the
`chat:write` bot scope and must be added to each channel it answers in. A request still waiting
for approval, or denied, posts nothing, and the tool says which; Slack's own refusal comes back as
Slack's error code, such as `not_in_channel`. A reply is at most 4,000 characters, and the tool
sends each one once, since a post whose answer was lost may already be in the thread. A session
posts into any channel the app has been added to: the tool does not know which project a channel
belongs to.

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
- A person's reply to a comment reaches every session that wrote a comment in its thread, and
  every session that wrote the document the thread is on. A person's new comment on a document
  reaches every session that wrote the document. Each arrives on the session's own inbox topic.
- The session holding an issue's claim gets, on its own inbox topic, a person's answers,
  hand-backs and resolutions of asks, comments (new, edited, resolved or reopened) and messages on
  that issue. A message a person sends to one session reaches only that session. Dispatch reads
  the claim when it publishes the event, so a retry after the claim changed hands reaches the new
  holder.
- On an issue with no route and no session holding its claim, a person's comment, and its edit,
  reach every session that wrote the issue's spec (its primary document), on its own inbox topic.

A session reached more than one of these ways gets one copy. A new comment that mentions a session
arrives there once, as the mention, when the mention reached it; when the mention's send fails,
the session still gets the comment on its own inbox topic, as the route, a follower, an author or
the claimant. While the mention is still being sent, for up to a minute, Dispatch holds that copy
back and checks again a few seconds later. One copy is not guaranteed: when the mention's receipt
times out, or its send is stranded for over a minute, Dispatch records it as failed though it may
have landed, so the session can get the comment twice, once as the mention and once on its inbox
topic. A session treats the second copy as the same comment. A comment's edit reaches every one
of these sessions, mentioned or not; a resolution or reopening reaches the route, the claimant, the
ask's followers and the thread root's author, mentioned or not. Neither is held back with the new
comment: while the new comment waits on its mention, an edit to it can arrive first. A session that
gets an edit, resolution or reopening of a comment it has not seen yet should expect the comment
to follow. Anything else on an issue, including every event an agent makes outside an ask the
session follows, reaches an agent only if the agent subscribes to it.
[Dispatch for agents](/legion/dispatch/for-agents/#what-reaches-you) covers this from the agent's
side. Because the stream keeps them, an agent whose connection was down when one of these was
published still gets it when it reconnects
([Messages a session missed](#messages-a-session-missed)).

## Topics and subscriptions

| Topic | What it is |
| --- | --- |
| `notifications.agent.<session_id>` | One session's inbox. Direct messages, ask follow-ups, replies in threads it wrote in, comments on documents it wrote, a person's answers, comments and messages on an issue it has claimed, comments on an issue with no route and no session claim whose spec it wrote, and claim changes arrive here. |
| `notifications.role.<role>` | A role, held by one live session at a time. You publish to it; the listener hands each message to the current holder and answers once the holder confirmed it. |
| `notifications.dispatch.issue.<KEY>.>` | Every event of one Dispatch issue. |
| `notifications.github.<owner>.<repo>.pr.<n>.>` | Every event of one pull request. |
| `notifications.legion.<project>.controller` | A Legion project's controller. |
| `notifications.slack.*.<channel>.mention` | Every mention of the Slack app in one channel, whatever the team. |
| `notifications.envoy.exceptions.<topic>` | A delivery to `<topic>` that failed, with the reason. |
| any other `notifications.<name>` topic | A team topic you name, such as `notifications.team.deploy-squad.all`. It reaches every session subscribed to it when it is published, and is not kept for a session that subscribes later. The stream keeps no such topic, so it is the one family a session still takes live, on its own NATS connection, its own publish included. A publish through the listener also hands it to each other such session registered on the topic, on any machine, and waits for each to confirm it: if one does not within four seconds, the publish fails naming each session that did not, and every session it does not name received it. A session that stopped without unregistering stays registered for up to five minutes and is named in that time. A registered session whose connection was down gets the message from the stream when it reconnects ([Messages a session missed](#messages-a-session-missed)). A session the listener delivers to over a serve port, such as an OpenCode agent, is not waited for: it receives the topic through its machine's listener when that listener subscribes to every topic (`ENVOY_SUBSCRIBE_ALL_TOPICS=1`, which a host's own listener sets; the shared hub's listener does not), except a publish of its own, which the listener does not echo back. A topic outside `notifications.` is refused. |

An agent subscribes with Envoy's `envoy_subscribe` tool, which stores the subscription in the
listener. A subscription to `<subject>.>` also covers `<subject>` itself, so subscribing to a pull
request's `.>` gets its lifecycle events as well as its comments and checks.

A role message is not stored for a later holder. A publish through the API waits for the stream to
store the forward to the live holder and answers `200` naming the holder. If no live session holds
the role, it is refused with `404`; if the holder does not take the message's delivery mode, with
`403`; and if the stream did not store the forward within four seconds, with `504` and the reason
`delivery_failed`. A role message published straight onto NATS, not through the API, is forwarded
once, and if that forward is not stored the listener publishes an exception naming the reason:
`no_holder` or `delivery_failed`. Either way the forward to the live holder is
stored on the holder's inbox, and it counts as delivered once the stream has stored it, so a
holder that restarts or loses its connection gets the forward once when it is back. No session
answers a receipt for one.

## Messages a session missed

Every message a session gets comes from a durable consumer the listener keeps for it on the
stream: one per Oh My Pi session (`@sjawhar/pi-envoy`), per Claude Code session (`claude-envoy`)
and per session on a serving port, filtered on the session's inbox and on each topic it follows
that the stream keeps. The listener that owns the session sends it those messages in the order
they were published, on `notifications.envoy.deliver.<session_id>`, a subject the stream does not
keep, and marks each one delivered only when the session answers that it has it. No session takes
its messages from a NATS subscription of its own, so a message published while the session was not
running, was restarting or had lost its connection, or while its listener restarted, reaches the
session once when it is back, up to 72 hours later.

- **What comes this way:** the session's inbox, `notifications.agent.<session_id>` (direct
  messages, Dispatch deliveries, and the forwards of role messages to a role it holds), and the
  topics it follows that the stream keeps, such as an issue's
  `notifications.dispatch.issue.<KEY>.>`.
- **What does not:** anything published on a topic before the session followed it. Every
  `notifications.` topic a session may follow is one the stream keeps: the listener refuses a
  publish to, or a subscription to, any other by name, since no session would ever receive it.
- **When a message counts as delivered:** an Oh My Pi session answers once the message is in its
  session file. A Claude Code session under Legion's bridge answers once Claude Code took the
  message as a user message. A Claude Code session in channel mode answers once Claude Code
  accepted the channel notification, the last signal Claude Code gives there, so a notification it
  drops after that (a channel it did not load) is not sent again. A session on a serving port, such
  as an OpenCode agent, gets each message by the listener's push, marked delivered on the port's
  `204` and pushed again until then.
- **Nothing runs late:** a BTW delivered before, or published before the session's connection
  began, starts no side turn: an Oh My Pi session shows it as one inert notice, and a Claude Code
  session passes it over.

Both plugins ping NATS every 10 seconds and drop a connection that leaves two pings unanswered, so
a connection that stops answering without closing, as when the network path under it freezes, is
left within 30 seconds and redialed. On the reconnect the session registers again, and its
listener sends it what it had not answered yet, from the oldest, so what was published meanwhile
reaches it once.

The listener manages these consumers only when its NATS user may (the `envoy-infra` grant for a
listener, `ListenerPermissions`, includes it). A registration it cannot give a consumer is refused
by name — `503` with reason `no_grant` or `consumer_error`, `409` with `not_requested` or
`invalid_session_id` — and nothing is registered, so a session nothing delivers to never appears
live. There is no second path: no session receives from a plain NATS subscription.
A session's own NATS user may subscribe to every session's delivery subject, so a client holding
one can read another session's delivery frames and answer one as delivered; it may read no other
notification topic.

- **What comes back after a break:** everything the consumer holds, in the order it was published:
  messages on the session's inbox topic, `notifications.agent.<session_id>`, and on the topics it
  follows, such as an issue's `notifications.dispatch.issue.<KEY>.>` or a team's
  `notifications.team.<name>`, up to 72 hours old. When the stream was recreated, or the
  consumer was created below the stream's oldest message, the session is told once that messages
  may be missing.
- **Nothing runs late:** a person's Send or Aside, a Steer or a BTW that comes back this way is
  shown as a message only, since a person's input run late could act on an old instruction. The
  session does not take it as the person's turn and does not answer a BTW. A BTW that comes back
  starts no turn at all: an Oh My Pi session shows it as one inert notice once it is idle (a turn
  running when it comes back ends first), and a Claude Code session, which is never sent a BTW,
  passes it over.
- **Once:** the session records what it was shown, in its transcript (Oh My Pi) or in a state
  file per session (Claude Code), so a restart or resume does not show a message again (up to
  20,000 messages shown in 72 hours). An Oh My Pi session counts a message shown only once its
  transcript holds it: a message handed over while a turn runs waits in memory until the turn
  takes it, and one still waiting when the session stopped is shown after the restart. A crash
  between showing a message and recording it can show that message twice; it is never lost. A
  message the session fails to show three times is replaced by one notice naming it and saying
  how to read it.

## The session list

`GET /v1/sessions` lists the live sessions, and `GET /v1/registry/<session_id>` answers for one.
Besides each session's id, title, directory, topics and roles, a row carries the plugin the
session registered through, which every registration sends:

| Field | Oh My Pi | Claude Code | OpenCode |
| --- | --- | --- | --- |
| `plugin` | `@sjawhar/pi-envoy` | `@sjawhar/claude-envoy` | `@sjawhar/opencode-legion-envoy` |
| `plugin_version` | the package version (a source checkout reports the version of the last release in its history) | the first 12 characters of the marketplace commit Claude Code installed it from, or `source` when it runs from outside Claude Code's plugin cache (a checkout, `--plugin-dir`) | the package version |

A session whose plugin predates the two fields shows neither. The `envoy_sessions` tool of the
Oh My Pi and Claude Code plugins shows them too.

Whether a session is delivered to through its own consumer, read from its row: `delivery` is
`consumer` when its listener holds that consumer and relays it. It is the empty string for a
session on a serving port its listener could give none, which that listener pushes to instead,
and the key is missing altogether on a listener older than the field.

## The NATS grant a Legion daemon needs

Legion's daemon reads Dispatch and GitHub events from the stream through three durable consumers
per project, `legion-go-<project>-dispatch` (filtering `notifications.dispatch.issue.>` and the
project's repository-mapping subject), `legion-go-<project>-dispatch-server` (filtering
Dispatch's own start, `notifications.dispatch.server.started`) and `legion-go-<project>-github`
(filtering each of the project's repositories). It sends its own notices to roles and to the
controller through the listener's HTTP API, not over NATS.

So the NATS user the daemon connects as needs these permissions:

When `envoy-infra:gcp:NatsHub` renders this user, set its `daemon` field to `true`; that selects
`core.DaemonPermissions()` instead of a host's or an explicit user's grant.

| Publish | Subscribe |
| --- | --- |
| `$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS` | `notifications.envoy.exceptions.notifications.role.>` |
| `$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS` | `_INBOX.>` |
| `$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>` | |
| `$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>` | |
| `$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>` | |
| `$JS.ACK.ENVOY_NOTIFICATIONS.>` | |

The last row is the subject a consumer acknowledges a message on. Replies to the API requests come
back on `_INBOX.>`. The stored-message get (`STREAM.MSG.GET`) is how `legion state` reads each
consumer's oldest waiting message, whose time is the consumer's lag; it only reads, one message at
a time. A daemon whose user lacks it still runs. NATS does not answer a request it refuses, so the
daemon's state shows each consumer with messages waiting as `unavailable: read the oldest waiting
message: context deadline exceeded`, its waiting count still shown and the other consumers
unchanged, and the daemon's log names the refused subject.

The consumer subjects end in `>`, so the grant reaches every consumer of `ENVOY_NOTIFICATIONS`,
not just the daemon's three: a holder of the daemon's seed can read, recreate with another filter,
and drain any consumer of the stream, every host's listener consumer and another project's daemon
consumers included. It can also create a push consumer whose delivery subject is any subject at
all, so it can send the stream's messages to subjects it may not publish to itself: role lanes,
other clients' inboxes, `agentstream.*.control`. Treat the daemon's seed as a credential of the
whole stream and of every subject a push consumer can deliver to. Its `_INBOX.>` subscription
also receives every reply any client on the server gets, key-value reads and watch updates
included, such as the listener's `envoy_roles` and `envoy_sessions` watchers.

When `envoy-infra:gcp:NatsHub` renders this user, set its `daemon` field to `true`; that selects
`core.DaemonPermissions()` instead of a host's or an explicit user's grant.

The `$JS.API` subjects are chosen by the NATS client library, not by Legion's code: the same three
consumers written against another client library ask for different ones. So a grant is proven by
running the daemon's consumers under exactly that grant, against a real server, and watching for a
refusal: create the three consumers, restart onto them, have Dispatch and GitHub events
acknowledged, and read their lag as `legion state` does. The daemon logs every refusal at error as
`NATS refused the daemon a permission: its NATS user lacks that grant`, with the operation and the
subject. `packages/daemon/internal/daemon/nats_permissions_test.go` in the repository runs that
proof against the server images the hub runs, and `docs/kubernetes.md` says how the daemon's
credential is configured.

## Running the listener

The listener is `envoy-listener` in the `ghcr.io/sjawhar/legion/envoy` image. Pin it by digest and
verify it as [Running Dispatch](/legion/dispatch/running-dispatch/) shows.

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
| `ENVOY_HOST_BRIDGE` | Where the listener reaches a session on its own machine that registered a serve port: one registered through it, or one whose recorded host is the listener's own `ENVOY_MACHINE_ID` (letter case ignored), as a session that stayed on that machine and re-registered through another listener records. Unset, it uses the host the session registered with, then `host.docker.internal`. A session that re-registered through another listener and records another host is reached at that host, else that listener's machine id, never through the bridge. A listener delivers only to the sessions it owns: a session's owner is the listener its stored subscription names (the one it first subscribed through), else the one it last registered through; any other session is its owner's to deliver. |
| `ENVOY_SUBSCRIBE_ALL_TOPICS` | Set to `1` to have the listener also deliver every topic the stream does not store, such as a team topic, to the sessions on its machine that registered a serve port. Only a NATS user allowed to subscribe to `notifications.>` (a host's own user) may set it; under any other the listener exits at start with an error naming the variable. |
| `ENVOY_NATS_REPLICAS` | The deployment's replica count, a whole number from 1 to 5; default `1`. The listener creates the `ENVOY_NOTIFICATIONS` stream and its buckets with this many copies and raises one it finds with fewer, at start and every 30 seconds after ([Keeping the stream](#keeping-the-stream)). More than 1 needs a clustered NATS: a single server refuses it, and the listener then exits at start with an error naming the variable. Any other value refuses startup. Give every Envoy program of a deployment the same value: a clustered deployment sets it in every listener and in Dispatch's server. |
| `ENVOY_STREAM_MAX_BYTES` | The `ENVOY_NOTIFICATIONS` stream's size limit, a positive whole number of bytes. Once the stream holds that much it refuses every new message and keeps every one it holds, which leaves only when it is 72 hours old, and the sender gets an error it can retry: the listener answers a GitHub webhook `503`, which Dispatch's redelivery sweep has GitHub send again (five times, over about half an hour); Dispatch's outbox retries its event; an agent's send or publish fails with `stream ENVOY_NOTIFICATIONS is full`. Size the limit above what the stream takes in 72 hours at its busiest, with room to spare. Unset, the listener creates the stream with no limit and keeps a limit the stream already has; set, it raises a smaller limit, or none, to its own at start and every 30 seconds, and never lowers one ([Keeping the stream](#keeping-the-stream)). A limit larger than the NATS server can reserve makes the listener exit at start with an error naming the variable. Give every Envoy program of a deployment the same value: a clustered deployment sets it in every listener and in Dispatch's server. |
| `ENVOY_WEBHOOKS` | Which webhook routes to mount, for example `github,slack`. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | Required with `github`: the webhook secret. |
| `ENVOY_REVIEWER_APP_ID` | Required with `github`: the id of the GitHub App that posts reviews. |
| `ENVOY_GITHUB_MENTION_TRIGGER` | The mention that publishes a `.mention` event. Defaults to `@legion`. |
| `ENVOY_SLACK_SIGNING_SECRET` | Required with `slack`: the Slack app's signing secret. |

A listener on anything but loopback refuses to start without `ENVOY_API_TOKEN` or the OIDC pair.
A listener with neither serves `/v1` only to a request whose `Host` is `localhost`, a loopback
address or a name in `ENVOY_ALLOWED_HOSTS`, and answers any other `Host` with a JSON `421`, so a
page that rebinds its own name to the listener's address cannot use the API.
Keep the API private: only `/webhook/...` needs to be reachable from GitHub and Slack.

Raising a bucket to `ENVOY_NATS_REPLICAS` copies is an update of the bucket's stream, so the
listener's NATS user needs `$JS.API.STREAM.UPDATE.KV_<bucket>` for each of its buckets beside the
info and create it already holds; the `envoy-infra` grant for a listener includes it. Under a grant
without it, a start that finds a bucket with fewer copies waits 10 seconds and exits with an error
naming the bucket and the variable:
`raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: context deadline exceeded`.

### Keeping the stream

Every Envoy program of a deployment carries `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES`, so
a clustered deployment sets both in every listener and in Dispatch's server. A start never writes
fewer copies or a smaller limit than it found, so an operator's raise of either stands, and
programs with different values do not fight: the most copies and the largest limit stand. But
JetStream's stream update has no compare-and-swap: a start with other settings, or none, or
another subject list, that reads the stream before another start writes it and writes after puts
the old subjects, copies, limit and metadata back.

So every program that owns the stream, each listener and Dispatch's server, reads it again every
30 seconds and puts back copies below its `ENVOY_NATS_REPLICAS`, a limit below its
`ENVOY_STREAM_MAX_BYTES` or none (when set), a missing `envoy_stream_id`, and the 72-hour age and
duplicate window and the discard policy (the same in every release), and adds back each subject
its own build carries that the stream lacks, unless it overlaps a subject the stream has or is a
role lane. It logs a WARN line `envoy nats keeper restored the stream` with, for example,
`restored="replicas 1 -> 3, max_bytes -1 -> 2000000, envoy_stream_id added as <id>, subjects added notifications.legion.>"`.
It never lowers the copies or the limit, so raising the limit of a full stream with
`nats stream edit ENVOY_NOTIFICATIONS --max-bytes=<n>` takes effect at once and stays. It only
adds subjects and never removes or replaces one, so two releases with different subject
lists do not switch the stream back and forth, and a subject a concurrent write dropped comes
back within 30 seconds from the keeper of the release that carries it. A stream that still
carries a role-lane subject is left to a start, which moves the role lanes off. It also raises
the buckets its program opens back to `ENVOY_NATS_REPLICAS` (`envoy nats keeper raised a bucket`,
with `replicas="1 -> 3"`). A pass that fails logs `envoy nats keeper could not restore the stream`
or `envoy nats keeper could not raise a bucket` and is tried again 30 seconds later; it never
stops the program.

During a rollout with mixed settings the stream can therefore sit below the setting for up to 30
seconds after the last start. To lower the copies, or to lower or remove the limit, change the
setting in every program first, then run `nats stream edit <stream> --replicas=<n>` or
`nats stream edit ENVOY_NOTIFICATIONS --max-bytes=<n>` (`-1` for none); otherwise the next pass
puts it back.

On a clustered NATS (nats-server v2.10.29 among them), the server keeps the id of a message the
full stream refused on the stream's leader for 72 hours, a purge notwithstanding, and
acknowledges every retry under that id as a duplicate it does not hold. Envoy then publishes the
same message once more without its id, so the retry is stored as soon as the stream has room,
with no operator step. While the stream is still full, that publish is refused too, and the
retry fails with `stream ENVOY_NOTIFICATIONS acknowledged a duplicate of a message it does not
hold, and its publish without its id failed: stream ENVOY_NOTIFICATIONS is full: nats: maximum
bytes exceeded`, which is retried as a full stream is. The stream drops no repeat of a message
stored without its id, so a later publish under the same id stores another copy, which the
stream's consumers see.

### With Pulumi

The `envoy-infra` Pulumi plugin runs the listener on a Docker host from a Pulumi program in any
language, as the component `envoy-infra:host:Listener`, and a NATS server for it on Google Cloud,
as `envoy-infra:gcp:NatsHub` on one VM or as `envoy-infra:gcp:ClusteredNatsHub`, three NATS
servers on a GKE cluster whose optional tailnet hosts also run the listener. It also deploys
Dispatch on Google Cloud as `envoy-infra:gcp:Dispatch`, which creates an Autopilot cluster, private
Cloud SQL, and the tailnet-served Dispatch workloads. A hub user with `dispatchMachineId` renders
the NATS user `envoy-dispatch` shares with its listener. Its
[README](https://github.com/sjawhar/legion/blob/main/packages/envoy/infra/README.md) says how to
install it, what each component deploys and which settings it sets.

### Health and readiness

`GET /healthz` needs no credential:

| Answer | Meaning |
| --- | --- |
| `200` `starting` | The listener is still binding its durable consumer. |
| `200` `healthy` | Everything answers. The body includes the consumer's lag, and `live_topics`: `subscribed` when the listener subscribes to every topic (`ENVOY_SUBSCRIBE_ALL_TOPICS=1`), `not_applicable` when it does not. |
| `200` `degraded` | A passing key-value failure the listener is retrying. Also `KV watch rebuild unavailable` when the listener has not rebuilt its key-value watches for a minute after a NATS reconnect: its caches serve what they last held, and the answer stays a `200` because the container health check and the readiness probe both read `/healthz`, so refusing would take out the one listener still serving those caches and replace it with one that rebuilds into the same failure with empty caches. |
| `503` `unhealthy` | NATS is unreachable, the subscription or a cache watcher stopped, the durable consumer is gone, or the durable consumer is `not bound to this listener's subscription`, which a durable deleted and created again under the listener leaves. `role lane subscription inactive` when the role lane's own connection is down or holds no valid subscription, since no role message is forwarded then. With `ENVOY_SUBSCRIBE_ALL_TOPICS=1`, also `live subscription inactive` when the subscription to every topic is gone or the server refuses it, as one restarted without that grant does. |

After a NATS reconnect the listener rebuilds the key-value watches its caches read through. A
rebuild that fails is tried again within seconds, so one that fails and then succeeds inside that
minute leaves the listener `healthy` and logs a warning rather than an error; only a rebuild that
has not succeeded for a minute is logged as an error and makes `/healthz` answer `200` `degraded`,
naming the rebuild and how long it has gone without succeeding. Page on that error line, not on
the health check: the health check and the readiness probe act on `/healthz` themselves, so a
listener taken out for a stalled rebuild is replaced by one that rebuilds into the same failure
with caches that warm up empty and drop the messages the running one still delivers.

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
session still running; once NATS is back, each session shows what was sent to it, or published on
a topic it follows that the stream keeps, while NATS was down
([Messages a session missed](#messages-a-session-missed)). A running session renews its
registration every 90 seconds; once it stops, Envoy drops its subscriptions and role claims within
minutes. An Oh My Pi or Claude Code session remembers the topics it follows, so when it is resumed
it follows them again and recovers what the stream kept for them.

| Task | Command, from `packages/envoy/deploy/compose` |
| --- | --- |
| Upgrade to the newest published image | `docker compose -f single-node.compose.yml pull && docker compose -f single-node.compose.yml up -d --wait` |
| Read the logs | `docker compose -f single-node.compose.yml logs -f` |
| Stop it, keeping its data | `docker compose -f single-node.compose.yml down` |
| Stop it and delete its data | `docker compose -f single-node.compose.yml down -v` |
