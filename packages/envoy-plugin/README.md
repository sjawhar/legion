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
- `dispatch_issue`
- `dispatch_ask`
- `dispatch_edit_ask`
- `dispatch_resolve_ask`
- `dispatch_comment`
- `dispatch_suggest`
- `dispatch_message`
- `dispatch_doc_edit`
- `dispatch_doc_read`
- `dispatch_artifact`
- `dispatch_read`
- `dispatch_search`

The twelve native `dispatch_*` tools create and read Dispatch issues, asks, comments,
documents, and artifacts, or search all of them. They are present when `dispatch.enabled`
resolves a server URL and bearer token from envoy.json (`~/.config/opencode/envoy.json`, merged
with `<repo>/.opencode/envoy.json`) or the `DISPATCH_URL` and `DISPATCH_TOKEN` environment
variables; `dispatch.enabled: true` without `dispatch.serverUrl` targets `http://localhost:8766`.
Each issue-scoped call fills the target issue from the session working directory, stamps it with
the OpenCode session id and title, and stores a successful mutation's `details.topic` as tool
metadata so the host subscribes to that exact Dispatch topic.

`dispatch_artifact` accepts exactly one upload source: a local `path`, or inline `content`.
An architect can post a specification directly with
`{ issue, name: "spec.md", content: "# Design" }`.

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
