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
`dist/dispatch.js`) first on every shell command's `PATH`, sets `DISPATCH_HOST=opencode`, hands on
the serve's envoy.json as `ENVOY_CONFIG_FILE`, and sets `DISPATCH_SESSION_ID` and
`DISPATCH_SESSION_TITLE` to the session running the command and the title the plugin tracks for it
(each empty when OpenCode names no session or the session has no title yet, so a value inherited
from a parent process never stands in).

The serve reads one envoy.json: `$OPENCODE_CONFIG_DIR/envoy.json` when `OPENCODE_CONFIG_DIR` is
set, else the file `ENVOY_CONFIG_FILE` names, else `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`,
with the `dispatch` section of a `<repo>/.envoy/envoy.json` merged over it: its server only with the
token beside it, and its token only beside its own server. An `envoy` section in that checkout's
file is not read, which the logged notice says, and a checkout's file that is not valid refuses
Dispatch alone, only while `DISPATCH_URL` is unset. The chosen file's `dispatch` section
(`enabled`, `serverUrl`, `token`) and `envoy` section (`enabled`, `url`, `natsUrls`, `token`,
`natsSeed`) are resolved each on its own: `enabled: false` turns one off, a missing file
contributes nothing (each section is then on only when its variables name it), and a `token`
starting with `!` is a command whose trimmed output is the value. The serve opens no NATS
connection, so it resolves no `natsUrls` and never runs `natsSeed`. `DISPATCH_URL`,
`DISPATCH_TOKEN_FILE`, `DISPATCH_TOKEN`, `ENVOY_URL`, `ENVOY_TOKEN_FILE` and `ENVOY_TOKEN` each
override their own field; a file's token goes with a variable's server only when it is the file's
own. The `dispatch` command reads the file the `shell.env` hook hands it in `ENVOY_CONFIG_FILE`.
`dispatch.enabled: true` without `dispatch.serverUrl` targets `http://localhost:8766`. With
Dispatch on, every session also carries the `dispatch-first` skill as an instruction file; with
Envoy off, the plugin registers no Envoy tool and calls no listener. Why a section is off is logged
once (`envoy: no envoy.json at <path>: Dispatch and Envoy are off`, the section's error, or what a
checkout's files hold that is not read).

`~/.config/opencode/envoy.json` and a checkout's `.opencode/envoy.json` are not read. While the old
user file is there and the serve's file is missing, Dispatch and Envoy stay off unless a variable
names one, and the log says the old file is not read and what to do: for
`~/.config/legion/envoy.json`,
`mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`; for
`$OPENCODE_CONFIG_DIR/envoy.json`, copy or link the old file to it, or write it; under a relocated
`XDG_CONFIG_HOME`, only that it is not read there. While a checkout has no `.envoy/envoy.json`, the
log and the `dispatch` command name the rename of its `.opencode/envoy.json`. A top-level
`natsUrls` is refused, naming `envoy.natsUrls`.

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
