# @sjawhar/opencode-legion-envoy

OpenCode plugin for Legion's Envoy subsystem.

This package exposes:

- `envoy_subscribe`
- `envoy_unsubscribe`
- `envoy_list`
- `envoy_send`
- `envoy_publish`
- `envoy_role_set`
- `envoy_whoami`
- `envoy_sessions`
- `dispatch`

`dispatch` raises or continues a durable question thread for the human (a GitHub
issue on the Dispatch dashboard) when `dispatch.enabled` is set in envoy.json
(`~/.config/opencode/envoy.json`, merged with `<repo>/.opencode/envoy.json`). The
session is auto-subscribed to the thread so the reply arrives back through Envoy.

It also maintains the live session registry metadata needed for Envoy to discover OpenCode sessions and their API ports.

Slack topic examples must use the real Slack `team_id`, for example:

- `notifications.slack.T01234567.C0A0DHVU8HE.mention`

Do not use workspace slugs like `acme` in the topic path.

## Sync to another machine

```bash
# From the repo root:
./packages/envoy-plugin/scripts/sync-host.sh sami@sami

# Or via the combined envoy sync:
./scripts/sync-envoy-host.sh sami@sami
```

The sync script downloads the latest envoy-plugin release tarball from GitHub,
extracts it to `~/legion/default/packages/envoy-plugin/` on the remote host, and
updates the remote's `opencode.json` to use a `file://` reference instead of the
npm package. Requires `gh` CLI on the machine running the script.
