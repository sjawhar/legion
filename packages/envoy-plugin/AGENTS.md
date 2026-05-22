# Envoy Plugin Package

OpenCode plugin package for Legion's Envoy subsystem.

## Overview

This plugin exposes the Envoy tools and maintains the live session registry metadata Envoy needs for hot delivery.

It is the user-facing bridge between OpenCode sessions and Envoy transport.

## Where to look

| Task                | Location               | Notes                                                              |
| ------------------- | ---------------------- | ------------------------------------------------------------------ |
| Tool definitions    | `src/server.ts`        | `envoy_subscribe`, `envoy_unsubscribe`, `envoy_list`, `envoy_send`, `envoy_publish`, `envoy_role_set`, `envoy_whoami`, `envoy_sessions` |
| Packaging metadata  | `package.json`         | npm identity, build scripts                                        |
| Distribution output | `dist/server.js`       | built plugin consumed by OpenCode                                  |
| Host rollout helper | `scripts/sync-host.sh` | sync dist + shim to remote host                                    |

## Critical conventions

- Tool descriptions must be self-describing enough that agents can infer correct topic formats.
- Slack examples must use real `team_id` values, not workspace slugs.
- This package owns the session-registry/port-backfill behavior now; do not split that back into a second plugin casually.
- Keep the plugin source-of-truth here even if a dotfiles shim is still used for rollout convenience.

## Topic reminders

- Agent: `notifications.agent.<session_id>`
- GitHub: `notifications.github.<owner>.<repo>.<kind>`
- Slack: `notifications.slack.<team_id>.<channel_id>.<message|mention>`

If you are unsure what a session is subscribed to, use `envoy_list()`.
