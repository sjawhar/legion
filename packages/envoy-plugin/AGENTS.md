# Envoy Plugin Package

OpenCode plugin package for Legion's Envoy subsystem.

## Overview

This plugin exposes the Envoy tools and maintains the live session registry metadata Envoy needs for hot delivery.

It is the user-facing bridge between OpenCode sessions and Envoy transport.

## Where to look

| Task                | Location               | Notes                                                              |
| ------------------- | ---------------------- | ------------------------------------------------------------------ |
| Tool definitions    | `src/server.ts`        | `dispatch`, `envoy_subscribe`, `envoy_unsubscribe`, `envoy_list`, `envoy_send`, `envoy_publish`, `envoy_role_set`, `envoy_whoami`, `envoy_sessions` |
| Packaging metadata  | `package.json`         | npm identity, `exports` map, scripts. Published entries point at `dist/`: `prepack` bundles `src/server.ts` (externals: `@opencode-ai/*`) so the tarball has no dependency on the unpublished `@legion/*` workspace packages, which live in `devDependencies` |
| TUI: `/whoami` + sidebar | `src/tui.tsx`     | slash command + session-id/port sidebar; loaded via the `./tui` export. Ships as `.tsx` source (solid JSX cannot be bundled by `bun build`) — Bun transpiles it natively at load, so `@opentui/core` + `@opentui/solid` MUST be `peerDependencies` (not `devDependencies`) so the `@jsxImportSource @opentui/solid` runtime resolves in the consumer's install tree |
| Host rollout helper | `scripts/sync-host.sh` | sync packed release tarball to remote host                         |
| Dispatch tool + auto-subscribe | `src/server.ts` (`dispatch` in the `tool` map), `@legion/envoy-client/dispatch-*` | Native tool present when `resolveDispatchConfig` yields a service URL; an invalid `envoy.json` refuses plugin load. The schema is built with OpenCode's `tool.schema` and mirrors the contract's `dispatchToolShape` (the test suite compares them as JSON Schema). `tool.execute.after` auto-subscribes dispatch callers to the thread topic |
| Bundled legion skills | `src/server.ts` `config` hook | OpenCode never scans plugin package dirs for skills; the hook pushes the package's `skills/` onto `config.skills.paths` (staged from repo-root `skills/` at prepack, removed postpack). Repo checkouts resolve `<repo>/skills` instead. |

## Critical conventions

- Tool descriptions must be self-describing enough that agents can infer correct topic formats.
- Slack examples must use real `team_id` values, not workspace slugs.
- This package owns the session-registry/port-backfill behavior; do not split that back into a second plugin casually.
- Keep the plugin source-of-truth here even if a dotfiles wrapper is used for rollout convenience.

## Topic reminders

- Agent: `notifications.agent.<session_id>`
- GitHub: `notifications.github.<owner>.<repo>.<kind>`
- Slack: `notifications.slack.<team_id>.<channel_id>.<message|mention>`

If you are unsure what a session is subscribed to, use `envoy_list()`.
