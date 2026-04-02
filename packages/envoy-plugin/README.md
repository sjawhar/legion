# @sjawhar/opencode-legion-envoy

OpenCode plugin for Legion's Envoy subsystem.

This package exposes:

- `envoy_subscribe`
- `envoy_unsubscribe`
- `envoy_list`
- `envoy_send`

It also maintains the live session registry metadata needed for Envoy to discover OpenCode sessions and their API ports.

Slack topic examples must use the real Slack `team_id`, for example:

- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.mention`

Do not use workspace slugs like `trajectorylabs` in the topic path.

## Sync to another machine

```bash
cd ~/legion/default/packages/envoy-plugin
./scripts/sync-host.sh sami@sami
```

This syncs the built plugin dist and the dotfiles shim to the target host.
