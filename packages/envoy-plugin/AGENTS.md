# Envoy Plugin Package

OpenCode plugin package for Legion's Envoy subsystem.

## Overview

This plugin exposes the Envoy tools and maintains the live session registry metadata Envoy needs for hot delivery.

It is the user-facing bridge between OpenCode sessions and Envoy transport.

## Where to look

| Task                | Location               | Notes                                                              |
| ------------------- | ---------------------- | ------------------------------------------------------------------ |
| Tool definitions    | `src/server.ts`        | `dispatch_issue`, `dispatch_ask`, `dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`, `dispatch_doc_read`, `dispatch_artifact`, `dispatch_read`, `envoy_subscribe`, `envoy_unsubscribe`, `envoy_list`, `envoy_send`, `envoy_publish`, `envoy_role_set`, `envoy_whoami`, `envoy_sessions` |
| Packaging metadata  | `package.json`         | npm identity, `exports` map, scripts. Published entries point at `dist/`: `prepack` bundles `src/server.ts` (externals: `@opencode-ai/*`) so the tarball has no dependency on the unpublished `@legion/*` workspace packages, which live in `devDependencies` |
| TUI: `/whoami` + sidebar | `src/tui.tsx`     | slash command + session-id/port sidebar; loaded via the `./tui` export. Ships as `.tsx` source (solid JSX cannot be bundled by `bun build`) — Bun transpiles it natively at load, so `@opentui/core` + `@opentui/solid` MUST be `peerDependencies` (not `devDependencies`) so the `@jsxImportSource @opentui/solid` runtime resolves in the consumer's install tree |
| Host rollout helper | `scripts/sync-host.sh` | sync packed release tarball to remote host                         |
| Dispatch tools + auto-subscribe | `src/server.ts` (`dispatch_*` in the `tool` map), `@legion/contracts` (`dispatchToolSpecs`, `zodSchemaApi`), `@legion/envoy-client/dispatch-*` | Native tools are present when `resolveDispatchConfig` resolves URL and bearer token; an invalid `envoy.json` refuses plugin load. Build every host schema from `spec.arguments(zodSchemaApi(tool.schema))`; return `DispatchToolResult.details` as OpenCode tool metadata so `tool.execute.after` subscribes only to `details.topic`. |
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
