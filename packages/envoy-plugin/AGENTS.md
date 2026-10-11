# Envoy Plugin Package

OpenCode plugin package for Legion's Envoy subsystem.

## Overview

This plugin exposes the Envoy tools, puts the `dispatch` command in every session's shell, and maintains the live session registry metadata Envoy needs for hot delivery.

It is the user-facing bridge between OpenCode sessions and Envoy transport.

## Where to look

| Task                | Location               | Notes                                                              |
| ------------------- | ---------------------- | ------------------------------------------------------------------ |
| Tool definitions    | `src/server.ts`        | `envoy_subscribe`, `envoy_unsubscribe`, `envoy_list`, `envoy_send`, `envoy_publish`, `envoy_role_set`, `envoy_whoami`, `envoy_sessions`; no `dispatch_*` tool |
| Packaging metadata  | `package.json`, `scripts/prepack.sh` | npm identity, `exports` map, scripts. Published entries point at `dist/`: the build bundles `src/server.ts` (externals: `@opencode-ai/*`) and the `dispatch` CLI (`../envoy-client/scripts/build-dispatch-cli.sh`, the one build of the CLI bundle, into `dist/dispatch.js`), so the tarball has no dependency on the unpublished `@legion/*` workspace packages, which live in `devDependencies`; `prepack.sh` gives each bundle its own metafile for `dist/THIRD_PARTY_NOTICES`. `files` ships `bin/` |
| TUI: `/whoami` + sidebar | `src/tui.tsx`     | slash command + session-id/port sidebar; loaded via the `./tui` export. Ships as `.tsx` source (solid JSX cannot be bundled by `bun build`) — Bun transpiles it natively at load, so `@opentui/core` + `@opentui/solid` MUST be `peerDependencies` (not `devDependencies`) so the `@jsxImportSource @opentui/solid` runtime resolves in the consumer's install tree |
| Host rollout helper | `scripts/sync-host.sh` | sync packed release tarball to remote host                         |
| The `dispatch` CLI | `bin/dispatch` (the shim, mode 755), `dist/dispatch.js`, the `shell.env` hook in `src/server.ts` | The hook puts `bin/` (found once at load by walking up from the module to the `package.json` named `@sjawhar/opencode-legion-envoy`) first on `PATH`, sets `DISPATCH_HOST=opencode`, and sets `DISPATCH_SESSION_ID` and `DISPATCH_SESSION_TITLE` to the hook's session and the title `trackedSessions` holds for it. Both are set to `""` when absent: OpenCode runs the shell with `{...process.env, ...output.env}`, so a key left out keeps the inherited value. A missing Dispatch configuration leaves Envoy loaded and logs one warning. |
| Bundled legion skills | `src/server.ts` `config` hook | OpenCode never scans plugin package dirs for skills; the hook pushes the package's `skills/` onto `config.skills.paths` (staged from repo-root `skills/` at prepack, removed postpack). Repo checkouts resolve `<repo>/skills` instead. |
| dispatch-first skill | `src/server.ts` `config` hook | With Dispatch configured, the hook pushes `skills/dispatch-first/SKILL.md` onto `config.instructions`; OpenCode reads instruction files into the main loop's system prompt on every request and leaves them out of title and compaction requests. A missing file throws at plugin load, naming it. |

## Critical conventions

- Tool descriptions must be self-describing enough that agents can infer correct topic formats.
- Slack examples must use real `team_id` values, not workspace slugs.
- This package owns the session-registry/port-backfill behavior; do not split that back into a second plugin casually.
- Keep the plugin source-of-truth here even if a dotfiles wrapper is used for rollout convenience.

## Topic reminders

- Agent: `notifications.agent.<session_id>`
- GitHub: `notifications.github.<owner>.<repo>.<kind>`, a dot in the owner or name written `_`
- Slack: `notifications.slack.<team_id>.<channel_id>.<message|mention>`

If you are unsure what a session is subscribed to, use `envoy_list()`.
