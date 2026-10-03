---
title: The clients
description: How Oh My Pi, Claude Code and OpenCode sessions connect to Envoy, the settings they read, and the shared client library.
sidebar:
  order: 4
---

A client connects one agent session to Envoy. It registers the session with the listener, keeps that
registration fresh, brings what arrives into the agent's conversation, and gives the agent the
`envoy_*` [tools](/legion/envoy/reference/tools/). The three hosts are built from the same shared
library, `@legion/envoy-client`, so they register the same tools from the same specs and render an
event the same way.

| | Oh My Pi | Claude Code | OpenCode |
| --- | --- | --- | --- |
| Package | `@sjawhar/pi-legion-envoy` on npm | `claude-envoy` in the `legion-plugins` marketplace (this repository) | `@sjawhar/opencode-legion-envoy` on npm |
| Receives events | Its own NATS subscriptions | Its own NATS subscriptions | The listener pushes to its server's port |
| How an event reaches the agent | Steers the turn in progress, or starts one | A Claude Code channel notification, read on the model's next turn | Posted to the session as a prompt |
| Envoy tools | All ten | All ten | All but `envoy_inbox` and `envoy_role_get` |
| Delivery modes it accepts | `aside`, `steer`, and `btw` when it can run a side turn | `aside` | None advertised |

All three also carry Dispatch's `dispatch_*` tools when Dispatch is configured;
[Dispatch for agents](/legion/dispatch/for-agents/) covers those.

## Settings every client reads

| Variable | What it is |
| --- | --- |
| `ENVOY_URL` | The listener. Defaults to `http://127.0.0.1:9020`. |
| `ENVOY_NATS_URL` | Comma-separated NATS URLs the session subscribes through. There is no default: without it, Oh My Pi and Claude Code receive nothing and say so. |
| `ENVOY_TOKEN_FILE`, `ENVOY_TOKEN` | The listener's bearer token, as a file holding it (which wins) or as the value. Neither set sends no token, which a listener on loopback with no token configured accepts. |
| `NATS_NKEY_SEED_FILE`, `NATS_NKEY_SEED` | The NATS user the session connects as, as a file holding its seed (which wins) or as the seed. Neither set connects without a credential. |
| `ENVOY_HEARTBEAT_MS` | How often the session re-registers, in milliseconds. Defaults to `120000`, two minutes. |
| `ENVOY_MACHINE_ID` | The machine name the session reports. Defaults to the host name. |

A file setting that is set must be readable and hold something; otherwise the client stops with an
error naming the variable and the path, rather than carrying on without the credential.

## Oh My Pi

Install the extension and start a session with NATS named:

```sh
omp plugin install @sjawhar/pi-legion-envoy
ENVOY_NATS_URL=nats://127.0.0.1:4222 omp
```

The package carries two extensions. `envoy` is the messaging this section describes. `legion` is
Legion's worker lifecycle, which stays inactive unless Legion's daemon started the session.

When a session starts, the extension registers it as self-subscribed, subscribes to its inbox, and
re-registers every heartbeat. An event that arrives during a turn steers that turn; one that
arrives while the session is idle starts a turn. Each event is rendered as one block naming who
sent it, when, its id, and its message.

`/whoami` copies the session's id, the address another session sends to, to the clipboard.
`envoy_inbox` lists the session's 50 most recent deliveries, for catching up after an interrupt.
`envoy_list` shows each subscription and whether it is live in the session, recorded in the
listener's registry, or both.

A session that is resumed, forked or branched takes back the role its predecessor held with a soft
claim, so it never takes a role another live session holds. Every heartbeat also checks the claim
and takes it back softly if the listener lost it.

## Claude Code

`claude-envoy` is a Claude Code channel plugin: an MCP server that subscribes to NATS for the
session and passes each event to Claude Code as a channel notification, together with the Envoy
tools. It is installed from this repository's `legion-plugins` marketplace, and a channel plugin
runs only when an organization's managed settings enable channels and allow it.
`packages/claude-envoy/README.md` in the repository says how to enable the channel, which variables
the session inherits, and how to run its manual smoke test.

A channel notification has no acknowledgement from the model, so the plugin accepts only `aside`
messages from Dispatch. `envoy_role_set` saves the role in the plugin's data directory, and
`claude --resume <session id>` takes it back with a soft claim. `/clear` gives the session a new
id; the plugin follows it within a quarter of a second, moving its subscriptions, registration and
role to the new id.

## OpenCode

`@sjawhar/opencode-legion-envoy` registers each OpenCode session with the port of the OpenCode
server it runs in. It does not subscribe to NATS itself: the listener on the same machine reads
the stream and posts each event the session's interest names to that server's
`/session/<id>/prompt_async`. A session that has not been busy in this server is not registered, and
a deleted session stops being refreshed, so it lapses from the registry within five minutes.

## The shared library

`@legion/envoy-client` lives in this repository's workspace and is not published on its own. It
holds:

- `createEnvoyClient`, the HTTP client for the listener's [`/v1` API](/legion/envoy/reference/api/),
  which retries once after 250 milliseconds on a network failure or a `5xx` answer;
- `renderInbound`, which turns an envelope into the block a host shows its agent and never shows the
  raw bytes;
- the delivery dedupe the Oh My Pi and Claude Code clients use to drop a repeated event;
- `envoyToolSpecs`, the one definition of every Envoy tool's name, description and arguments.

## Writing a client of your own

Any program can be a session. It needs to:

1. Register with `POST /v1/interests/subscribe`, giving a `session_id`, `self_subscribed: true` and
   the topics it wants, and repeat that at least every five minutes while it runs.
2. Subscribe to `notifications.agent.<session_id>` and to each of its topics on NATS.
3. Acknowledge each role message. A message on the inbox whose envelope still names a
   `notifications.role.<role>` topic was forwarded to it as the role's holder: answer its reply
   subject with an empty message within two seconds, or the sender sees a `receipt_timeout`. Answer
   nothing else. A message sent straight to the inbox is a stream publish, and its reply subject is
   the stream's acknowledgement, which an empty answer would break.
4. Drop a repeat of an `event_id` it has already handled.
5. Remove its registration with `DELETE /v1/sessions/<session_id>` when it stops.
