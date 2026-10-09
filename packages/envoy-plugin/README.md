# @sjawhar/opencode-legion-envoy

OpenCode plugin for Legion's Envoy subsystem.

This package exposes the Envoy tools:

- `envoy_subscribe`
- `envoy_unsubscribe`
- `envoy_list`
- `envoy_send`
- `envoy_publish`
- `envoy_role_set`
- `envoy_whoami`
- `envoy_sessions`

Agents reach Dispatch through the `dispatch` command in their shell: `dispatch --help` lists the
commands, `dispatch <command> --help` each one's flags, and the `dispatch` skill teaches them. The
plugin's `shell.env` hook puts its `bin/` (the `dispatch` shim, which runs the bundled
`dist/dispatch.js`) first on every shell command's `PATH`, sets `DISPATCH_HOST=opencode`, and sets
`DISPATCH_SESSION_ID` and `DISPATCH_SESSION_TITLE` to the session running the command and the
title the plugin tracks for it (each empty when OpenCode names no session or the session has no
title yet, so a value inherited from a parent process never stands in). Dispatch is reachable when
`dispatch.enabled` resolves a server URL and bearer token from envoy.json
(`~/.config/opencode/envoy.json`, merged with `<repo>/.opencode/envoy.json`) or the `DISPATCH_URL`
and `DISPATCH_TOKEN` environment variables; `dispatch.enabled: true` without `dispatch.serverUrl`
targets `http://localhost:8766`. With Dispatch configured, every session also carries the
`dispatch-first` skill as an instruction file.

It also maintains the live session registry metadata needed for Envoy to discover OpenCode sessions and their API ports.

Slack topic examples must use the real Slack `team_id`, for example:

- `notifications.slack.T01234567.C0A0DHVU8HE.mention`

Do not use workspace slugs like `acme` in the topic path.

## Sync to another machine

```bash
# From the repo root:
./packages/envoy-plugin/scripts/sync-host.sh sami@example-host-laptop

# Or via the combined envoy sync:
./scripts/sync-envoy-host.sh sami@example-host-laptop
```

The sync script downloads the latest envoy-plugin release tarball from GitHub,
extracts it to `~/legion/default/packages/envoy-plugin/` on the remote host, and
updates the remote's `opencode.json` to use a `file://` reference instead of the
npm package. Requires `gh` CLI on the machine running the script.
