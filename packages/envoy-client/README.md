# Envoy client

Shared HTTP transport, tool contract, and inbound delivery renderer for Envoy hosts. pi-envoy and
claude-envoy consume these modules rather than maintaining host-specific envelope parsers.

## Inbound delivery

`renderInbound(raw, sessionID, subject?)` accepts an additive Envoy envelope and returns a TOON
block with recognized routing and delivery fields. The renderer includes `to` only for a direct
message to the reader, formats sender identity and reply metadata, renders structured payloads
once as `message`, and marks unknown sources or fields with `unrecognised`.

Malformed input is safe: a non-JSON frame becomes a TOON block naming its topic and an
`unrecognised` parse failure. The renderer never emits raw frame bytes. Dispatch envelopes
(`source: "dispatch"`) render through the same TOON path as every other source, nesting the
parsed event under `dispatch: { issue_key, type, actor, payload }` with `payload` typed per event
type; a payload whose shape disagrees with its event type's schema is preserved as its raw parsed
value under `dispatch`, never dropped or reduced to prose. A dispatch envelope with no payload at
all falls back to the same `summary` field every other source gets and marks `unrecognised:
payload`. The renderer skips Dispatch events marked `notify: false` on an issue topic before a host
can steer or record them in its inbox, as well as events authored by the reader's own session.

## HTTP transport

`createEnvoyClient` sends and receives the listener's `/v1` JSON contracts. Send and publish
accept optional `inReplyTo`, `supersedes`, `urgency`, `expectsReply`, and `expiresAt` metadata.
Successful direct sends return both the envelope and listener-selected recipient; publishes return
the envelope and an optional role holder. Network failures and 5xx responses retry once after
250 ms. Listener error responses preserve their `error` and optional `expected` fields. The
listener's bearer is the `token` the client is given (`envoySettings`); without one it sends no
`Authorization` header.

`subscribe` exposes listener warnings, including a topic that has no event in the configured
stream. `getRole` returns a role holder with its last-seen timestamp, and `listSessions` accepts
optional directory and title filters.

## envoy.json

Every client and command reads one envoy.json holding a `dispatch` section and an `envoy` section:

```json
{
  "dispatch": {
    "enabled": true,
    "serverUrl": "https://dispatch.example",
    "token": "!secrets get DISPATCH_TOKEN"
  },
  "envoy": {
    "enabled": true,
    "url": "http://127.0.0.1:9020",
    "natsUrls": ["nats://nats.internal.example:4222"],
    "token": "listener-token",
    "natsSeed": "!secrets get NATS_NKEY_SEED"
  }
}
```

`envoyConfigPath(harness, env)` (`envoy-config.ts`) picks the file. A plugin reads its own
harness's directory and no other harness's: Oh My Pi's `$PI_CODING_AGENT_DIR/envoy.json` when
`OMP_PROFILE` names a profile other than `default`, Claude Code's `$CLAUDE_CONFIG_DIR/envoy.json`,
OpenCode's `$OPENCODE_CONFIG_DIR/envoy.json`. Without that directory it reads the file
`ENVOY_CONFIG_FILE` names, else `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`. A script
outside any plugin (harness `command`: `scripts/dispatch/ask-census.ts`, the e2e operator scripts,
`claude-envoy`'s `envoy-send`) reads `ENVOY_CONFIG_FILE` first, then the Oh My Pi, Claude Code and
OpenCode directories in that order (Claude Code's only under `CLAUDE_CODE_CHILD_SESSION=1`, which
Claude Code alone sets, in every Bash it runs), then the same neutral file. A relative
`ENVOY_CONFIG_FILE`, harness directory or `XDG_CONFIG_HOME` is refused, naming it and its value,
when the rule would use it. Each plugin hands the path it chose to the commands its session runs in
`ENVOY_CONFIG_FILE` (Oh My Pi sets it in `process.env` at load, Claude Code's `SessionStart` hook
appends it to `$CLAUDE_ENV_FILE`, OpenCode's `shell.env` hook sets it), beside `DISPATCH_HOST`; the
innermost session's value wins.

The `dispatch` command reads the file its session's plugin chose: with `DISPATCH_HOST` set, the
file `ENVOY_CONFIG_FILE` names, else its host's own rule (`envoyConfigPath(host)`). A Bash that
lacks what the hook wrote, as a Claude Code subagent's can (anthropics/claude-code#46696), has
`DISPATCH_HOST` unset and `CLAUDE_CODE_CHILD_SESSION=1`, which Claude Code sets in every Bash it
runs; `dispatch` then takes host `claude` and Claude Code's rule, so `CLAUDE_CONFIG_DIR`, the
session's own directory, comes ahead of any `ENVOY_CONFIG_FILE` an outer session exported.

`readEnvoyConfig(path, env, { cwd })` reads that file and a checkout's `<cwd>/.envoy/envoy.json`
as written, running no command. Each section is strict: an unknown key is refused naming it, and
so is a top-level `natsUrls`, which belongs in `envoy.natsUrls`. JSON that does not parse is
refused at its line and column, never with the parser's message, which quotes the text it stopped
at. A missing chosen file contributes nothing and sets `missing`: each section is then on only when
its variables name it, and a section still off says `no envoy.json at <path>: Dispatch is off`
(`Envoy is off`, or `Dispatch and Envoy are off` when neither is on), with
`(a link to <target>, which does not exist)` after the path when it is a dangling link. A chosen
file that cannot be read or is not valid is `failed`, naming the file and the key, and both
sections are off with that error.

Nothing reads the OpenCode-era `$HOME/.config/opencode/envoy.json`. While it is there and the
chosen file is missing, the notice ends with what to do about it, decided by the chosen file's
path: for `$HOME/.config/legion/envoy.json`, whichever rule chose it,
`; <old file> is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`;
for a profile's own file, `; <old file> is not read for this profile; copy or link it to <path>, or
write that file`; for the neutral file under a relocated `XDG_CONFIG_HOME`, as a Legion pane's is,
`; <old file> is not read under XDG_CONFIG_HOME=<dir>`, without asking anyone to move it.

No notice tells anyone to `mv` across a mount. When the old file's directory and the nearest
existing ancestor of the new file's directory are on different filesystems (`stat`'s device
differs, as in an agent box whose `~/.config/opencode` is a bind mount of its host's), the move
reads `<old file> is not read, and is on another filesystem than <new file> (a mount): create <new
file>, and move <old file> on the machine that owns it`, with no command, for the user file and the
checkout's `.opencode/envoy.json` alike; `readEnvoyConfig`'s `deviceOf` option answers a
directory's device, `stat`'s by default. The Go Dispatch server's refusal and warning follow the
same rule.

A checkout's file is the repository's, and its failure is its own: one that cannot be read or is
not valid refuses Dispatch with its error only while `DISPATCH_URL` is unset (a session's start
notice then says `<error>; not read while DISPATCH_URL is set`), Envoy never reads it, and a `.envoy`
that is a plain file is no checkout file. It may configure Dispatch alone: a top-level `natsUrls`
there is refused saying a checkout's envoy.json holds only a `dispatch` section, and an `envoy`
section, whatever it holds, is not read, which the start notice says
(`<checkout>: envoy is not read from a checkout's envoy.json; put it in <chosen file>`). So a cloned
repository neither chooses where a session's deliveries come from or which servers the user's NATS
seed signs in to, nor turns off the Envoy the session's environment configured. The Go Dispatch
server, which reads the same files, refuses to start on a checkout's `envoy` section. A checkout's
OpenCode-era `.opencode/envoy.json` is never read: while the checkout has no `.envoy/envoy.json`,
`warnings` holds `<old file> is not read: mkdir -p <cwd>/.envoy && mv <old file> <cwd>/.envoy/envoy.json`,
which the start notice and the `dispatch` command say.

Each section is resolved on its own: `dispatchSettings(env, config)` reads the chosen file's
`dispatch` section and, while `DISPATCH_URL` is unset, the checkout's, and `envoySettings(env,
config)` reads the chosen file's `envoy` section alone. Both follow one rule for `enabled`:
`enabled: false` turns the section off whatever else is set; otherwise the section's fields apply,
whether or not it sets `enabled`, and it is on when its address resolves, from the file or a
variable:

|                                        | Dispatch                                    | Envoy                                               |
| -------------------------------------- | ------------------------------------------- | --------------------------------------------------- |
| off whatever else is set               | `dispatch.enabled: false`                   | `envoy.enabled: false`                              |
| its address, from the file             | `dispatch.serverUrl`                        | `envoy.url`, `envoy.natsUrls`                       |
| its address, from a variable           | `DISPATCH_URL`                              | `ENVOY_URL`, `ENVOY_NATS_URL`                       |
| `enabled: true` with no address names  | `http://localhost:8766`                     | the listener `http://127.0.0.1:9020`, no NATS server |
| off, saying so, when nothing names one | `<file> names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off` | `<file> names no Envoy listener or NATS server, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off` |

So a file `{"envoy": {"url": …, "token": …}}` with no `enabled` is on with its bearer, and so is
`{"dispatch": {"serverUrl": …, "token": …}}`; a section holding only a token is off until a variable
names its server. `DISPATCH_URL` replaces a checkout's file whole: while it is set, the checkout's
server, token and `dispatch.enabled: false` are neither used nor refused. Otherwise a checkout
contributes only its `dispatch.enabled: false`, or a `dispatch.serverUrl` with the `dispatch.token`
written beside it, which is an address: its `dispatch.enabled: true` alone turns nothing on, since it
would name the default server and draw a variable's token to it. A checkout's `dispatch.serverUrl`
without its own token is refused, and so is its `dispatch.token` without its own
`dispatch.serverUrl`, unless `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE` replaces it. A checkout's
server with its own token points the session at that server as that token's holder, whichever
server it names, the user's own included; what a checkout cannot do is take the user's token, or a
variable's, anywhere.

A `token` or `natsSeed` value starting with `!` is a command (`fileValue`): it runs under
`/bin/sh -c` in the directory of the envoy.json that names it, never the session's working
directory, so `./bin/token` or a directory-configured shim (mise, direnv) resolves against that
file and not whatever repository the session started in. It runs in its own process group for at
most 10 s, and its trimmed stdout is the value. Its
exit status decides: a command that exited 0 has printed its value even when a process it left
running holds stdout open; a timeout kills the whole group. It runs only when its section's
resolver is about to use the value (the section on, no variable overriding the field). Its value
is kept for the process, shared across module instances (one map on `globalThis` under
`Symbol.for("legion.envoy-config.commands.v1")`, since Oh My Pi loads each extension instance, a
`task` subagent's included, as a fresh module, and pi-envoy and pi-legion, released separately,
each bundle this module; a change to an entry's shape takes a new key), and keyed by the command,
its file and its working directory. A command that ran out of time is kept as failed for the life
of the process: a hanging
command never runs again in that process, since each run would block it for the whole 10 s. A
failure that ended on its own (a non-zero exit, empty output, more than one line) is kept for 60 s
and then the command runs again, so a cause that clears (a keyring unlocked after the session
started) needs no restart. The `dispatch` command is one process per call, and each Claude Code
hook one per event, so a `!` token runs once for each. A non-zero
exit, a timeout or empty output is an error naming the field and the file, never the output. A
value with whitespace or a control character inside it, written, printed by a command, or given in
`DISPATCH_TOKEN`, `ENVOY_TOKEN` or a `_FILE` variable's file, is refused without being echoed, since
it cannot go in a header as one bearer. A checkout's file is the repository's, so a command in it
is refused.

`envoySettings` is the Envoy a session uses: `{ enabled: true, url, token, inbound, heartbeatMs }`,
or a `SectionOff`, `{ enabled: false, notice, error }` with exactly
one of the two set. A `notice` says why nothing is wrong: `<file> sets envoy.enabled to false:
Envoy is off`, `<file> names no Envoy listener or NATS server, and neither ENVOY_URL nor
ENVOY_NATS_URL is set: Envoy is off`, or the missing-file text. An `error` names what could not be
read or used. `ENVOY_URL`, `ENVOY_NATS_URL` (comma-separated), `ENVOY_TOKEN_FILE` then
`ENVOY_TOKEN`, and `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED` each override their own field of the
section; with no `envoy` section, `ENVOY_URL` or `ENVOY_NATS_URL` alone names an Envoy, which is
how a Legion pane gets the one its daemon names. An empty `ENVOY_*` variable counts as unset; an
empty `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED` is refused, naming it. `inbound` is how the session
takes inbound messages: `{ natsUrls, natsSeed }`, never with an empty `natsUrls`, or `{ off }`, `off`
saying why: `Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)` when nothing names one, a
seed's refusal (below), or `this session opens no NATS connection` for a caller that opens none
(`{ nats: false }`, OpenCode's plugin). The seed is resolved only when there are NATS servers to
use it on, so that caller resolves none. A set `_FILE` pointer that
cannot be read is refused, never a fallback. The file's `envoy.token` goes with `ENVOY_URL` only
when it names the origin of the listener written in the file (`http://127.0.0.1:9020` when it names
none), and is otherwise refused with `ENVOY_URL names <origin>, but <file>'s envoy.token is for
<origin>; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`; an empty `envoy.token` is refused, as an
empty `dispatch.token` is. A refusal of the listener's bearer turns Envoy off with that `error`.
The file's `envoy.natsSeed` goes only with the servers written in the file, since a server that is
not the real one can relay the real server's nonce and replay the signature: `ENVOY_NATS_URL` gets
it only when it names those servers, compared as a set of normalised URLs (scheme and host
lower-cased, a trailing `/` dropped, the default port 4222 written when none is, so
`nats://a:4222` and `nats://a:4222/` pair), and is otherwise refused with `ENVOY_NATS_URL names
other servers than <file>'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`. The seed is for NATS
alone, so its refusal (that pairing, a `natsSeed` command that fails, or a seed variable that cannot
be read) leaves Envoy on with its listener and bearer, and turns off inbound messages: `inbound.off`
names the refusal. A credential from the variables
goes with whatever address resolves. `natsAuthOptions` takes the resolved seed and names its source
when it is not a user seed; `natsSeedFromEnvironment` resolves one from the two variables alone.

Every caller resolves through `session-settings.ts`. `sessionConfigFile(harness, env)` is the file
`envoyConfigPath` chooses, with the rule's refusal of a relative path as `error` instead of a
throw, so a plugin that cannot work out its file still loads, with both sections off;
`handedConfigFile(env)` is the file `ENVOY_CONFIG_FILE` names, the one a plugin hands its
session's commands, or null when that is unset or empty. `sessionSettings(file, env, { cwd, nats
})` reads it once and returns both sections (`dispatchSettings`, `envoySettings`) and the one
notice the session shows at its start: the file's error (`<error>: Dispatch and Envoy are off`),
the missing file's notice, the missing-file text for a section still off when a variable turned
the other on, or a section's own error (`Dispatch is off: <error>`, `Envoy is off: <error>`), then
what a checkout's files hold that the session does not read (its `envoy` section, the file's own
error while `DISPATCH_URL` replaces it, or its `.opencode/envoy.json`), then, for a session that
opens a NATS connection (`nats` not `false`), that an Envoy that is on takes no inbound messages:
`Envoy's inbound messages are off: <inbound.off>; the Envoy tools still work`. A file that turns a
section off, or holds no section for it, says nothing.
`sessionDispatchSettings(file, env, { cwd })` reads
Dispatch alone, now: `{ dispatch, warnings, missing }`, Dispatch's resolution beside the files the
read found and did not read and the missing-file record, for a command, which says the `warnings`
itself, and for a plugin whose Dispatch calls follow a file that changes. `activeDispatchSettings`
turns its `dispatch` into the configuration a caller uses, null when nothing configures Dispatch,
or a `dispatch config: <reason>` throw. `sessionEnvoySettings(file, env, { cwd, nats })` reads
Envoy alone, for a command such as `envoy-send`.

## Dispatch

`dispatch-http.ts` sends the native Dispatch JSON API with the configured bearer
token. `dispatch-execute.ts` validates a call against its shared spec, derives
the caller's session origin and Legion issue reference, executes the operation,
and returns its text and typed details. No result carries a subscription topic:
`dispatch ask` and an ask reply through `dispatch comment` return
`details.follows = { ask }` (the session follows that ask server-side), and every
write's text names the `envoy_subscribe notifications.dispatch.issue.<KEY>.>`
line an agent passes to subscribe to the whole issue itself.
`dispatch-subscribe.ts` turns `details.follows` into the one-time host notice.
`dispatch-first.ts` reads the `dispatch-first` skill from a plugin's staged `skills/` and wraps it in
`DISPATCH_FIRST_MARKER`, the text pi-envoy and claude-envoy inject into a session with Dispatch.

A non-2xx answer is thrown as `DispatchServiceError`. Dispatch's own refusal, JSON with a string
`error`, is the error's message under its `code` (`HTTP_<status>` when the server set none), with
`fromDispatch` true. Any other body — a gateway's HTML page, an empty body, JSON of another shape —
did not come from Dispatch, so it is a `DispatchGatewayError` (`fromDispatch` false) whose message
names the method, the URL with its query (never the bearer, which is a header), the status and
reason phrase, a one-line plain-text excerpt of the body, and what asking again can do. The excerpt
drops scripts, styles and tags, reads at most the body's first 64 KiB in linear time, and, like the
reason phrase, has the value after `Authorization:` or `Bearer` redacted, and every run of 8 or more
of the client's bearer's characters (a shorter bearer only whole), so a copy cut short, split by
markup or overlapping another leaves no such piece. A body none of whose read part is text is named
by its size in bytes; only a body with nothing in it is "an empty body". A status that cannot clear
answers the same until the Dispatch URL, or whatever
answers in its place, is fixed. A 5xx, 408 or 429 may clear: a GET may succeed on a retry, and so
may a write answered 408 or 429, the gateway's own timeout or rate limit, which it sends before it
forwards anything (`mayHaveReachedDispatch` false); a write answered 5xx may or may not have reached
Dispatch, so the advice is to check whether it took effect before retrying. The error's `answer`,
`advice`, `transient` and `mayHaveReachedDispatch` let a caller that knows more about its own
request, such as `dispatch issue-update`'s close path, give its own advice instead. The executor
reads a status as Dispatch's meaning (a 404 as no linked issue, no such document, or a server
without the route) only when `fromDispatch` is true.

Successful write responses may include `advice`. The executor preserves that object as
`details.advice` and appends short pointers after the subscription/follow suffix: a primary spec
with no decision blocks, three or more session writes without a human response (with stronger
wording from six), a write while the issue is still in triage, and up to two still-open asks owned
by the session. Ask replies do not trigger the write-cadence pointer and suppress an open-ask
pointer when replying to that ask; a status-setting issue update does not trigger the triage
pointer. Triage advice is shown once per issue for the lifetime of the executor process; absent
`advice` leaves the prior result text and details unchanged.

`dispatchSettings(env, config)` (`dispatch-config.ts`), which `sessionDispatchSettings` runs over
the envoy.json a caller reads, enables Dispatch only when both its URL and bearer token resolve,
under the `enabled` rule above. `DISPATCH_URL` overrides `dispatch.serverUrl`, and
`dispatch.enabled: true` with neither names `http://localhost:8766`, the Go server's listen
address; an empty `DISPATCH_*` variable counts as unset. The token resolves from
`DISPATCH_TOKEN_FILE` (a path; the trimmed file contents are the token — how the Legion daemon hands
the bearer to a pane without putting it on argv), then `DISPATCH_TOKEN`, then the chosen file's
`dispatch.token`; while `DISPATCH_URL` is unset, a checkout that names its own `dispatch.serverUrl`
gives that server and its own token instead. A set `DISPATCH_TOKEN_FILE` that is unreadable or blank
disables Dispatch and names the path in `error`; it never falls back. The chosen file's token goes
with `DISPATCH_URL` only when it names the origin of the server written beside it: any other server
needs `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`, and is refused with
`DISPATCH_URL names <origin>, but <file>'s dispatch.token is for <origin>; set DISPATCH_TOKEN or
DISPATCH_TOKEN_FILE for it`. Invalid configuration, malformed URLs, and empty tokens leave Dispatch
disabled and name the failing source in `error`; nothing configuring it, or a file turning it off,
leaves it disabled with a `notice` (`<file> sets dispatch.enabled to false: Dispatch is off`,
`<file> names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`, or the
missing-file text).

### The `dispatch` command

Agents reach Dispatch through one command, `dispatch <command> [flags]`, which each host plugin
bundles from `bin/dispatch.ts` into its `dist/dispatch.js` and puts on its agents' shell `PATH`
through a `bin/dispatch` shim. `dispatch-command.ts` derives the commands and flags from the
specs: one command per spec, named by the spec's name without its `dispatch_` prefix and with
hyphens for underscores (`issue-update`), one flag per field
(`reply_to_ask` → `--reply-to-ask`), `--<field>-file <path>` for every string (`-` reads stdin),
`--no-<field>` for a boolean, `--clear-<field>` for a nullable field or a list, a repeated
singular flag for a list (`--label a --label b`, `--option "Label: what it costs"`, split at the
first `: `; a label holding `: ` goes in `--options-json`, which the generated command line uses
for such a list), and `--<field>-json` for any other object. `dispatch --help` lists the
commands, `dispatch <command> --help` its flags and an example, and `--dry-run` prints the
arguments a command would send.

`runDispatchCli` (`dispatch-cli.ts`) runs one call per process. The host plugin sets
`DISPATCH_HOST` (`omp`, `claude` or `opencode`) and the session's id (`CLAUDE_CODE_SESSION_ID`
under Claude Code, `DISPATCH_SESSION_ID` elsewhere); with either missing the command exits 2
naming it, and an empty variable counts as unset. `DISPATCH_SESSION_TITLE`, or the session
directory's `title` file, names the session on what it writes. It resolves Dispatch from the
envoy.json its session's plugin chose (above): with Dispatch off it prints `dispatch: <notice>` for
nothing configuring it or a file turning it off, and `dispatch: dispatch config: <error>` for what
could not be read or used, each exiting 2. `DISPATCH_HOST` unset with
`CLAUDE_CODE_CHILD_SESSION=1` is host `claude`. stdout carries what
the model reads: the result, one `- picture: <path>` line per image (written under the session
directory), the follow notice the first time a call follows an ask, and every refusal. It exits 0
on success, 1 when Dispatch or the arguments refused the call, and 2 on a usage error, a corrupted
`state.json` included. Under Claude Code the whole output stays under 25,000 characters: the
result text is cut to fit, the full text written to a file the output names, and the picture lines
and follow notice kept. When Dispatch took the call but the session's state (a picture, the
ledger, the memory) could not be written, the result still prints, with one line saying so.

`dispatch-session-state.ts` keeps one directory per session under `DISPATCH_STATE_DIR` (default
`<XDG_STATE_HOME or ~/.local/state>/dispatch/sessions/<id>`): `state.json` carries what a
long-lived host remembered across calls (the triage lines and pictures already shown, the asks
whose follow notice was printed), and `results.jsonl` gets one line per call with the tool's
`details` or its refusal, which pi-envoy's run-end check reads; a line that is not JSON is named
(file and line number) and skipped. Directories idle for 14 days are removed, at most once a day.

`bun bin/stand-in-gateway.ts --status 502 --body html` serves one non-Dispatch answer (`html`,
`empty`, `text` or `json`, from `src/stand-in-gateway.ts`) on `--port` (ephemeral by default) and
prints its URL, for proving what a command shows when a gateway, not Dispatch, answers.

## Command specs

`@legion/contracts` `src/dispatch-tools.ts` is the single source for the twenty-three
Dispatch specs behind the `dispatch` command: each one's name, description, field schemas
and example. `dispatch-command.ts` derives the command and its flags from them, and no host
adds aliases or descriptions of its own.
`dispatch issue-update` reads the issue first and sends `PATCH /api/v1/issues/{key}` as the
session actor: `status` (a Legion lifecycle status), `title`, `labels` (replacing the set),
`priority` (`0` for P0 down to `3` for P3, or `null` to clear it), `route`,
and `external_links`: a non-empty list merges into the issue's existing links by URL rather than
replacing them, and an explicit empty list (`--clear-external-links`) clears every link instead;
`rank`, the board's own order, is not exposed. Closing an issue (`status: "done"`)
requires `reason`, which it posts as an issue message before the PATCH, since a closed issue
refuses messages; a failed post sends no PATCH, and a PATCH that fails after the post names the
posted message. The result is one line —
`KEY: status a -> b; priority -> P1; linked <url> (N links)`, with `priority cleared` on a clear
and `reason posted as message <id> (<ref>)` ahead of a close — and a server refusal keeps its
`code` (`INVALID_STATUS`, `ISSUE_CLOSED`, `EXTERNAL_LINK_TAKEN`) at the head of the thrown message.
`dispatch request-approval` refuses, before it sends anything, while the version the request would
name (the document's latest, `approval.latest_version`) holds a decision block open. It reads the
live document's `ask` blocks (`GET /api/v1/artifacts/{id}/blocks`), then that version's markdown
and the owner's asks (the issue's for an issue document, since the artifact route refuses those),
and judges each block by its `state` on every line of the version that opens it, so a line quoting
the opener can add an open block but never hide one: an answer or a resolution closes the ask at
once but reaches a version only when the document settles or the next edit is written. A block the
version does not hold yet, or one with no ask yet, counts as open. A request over an open block
would be retracted by the version its answer writes. The refusal names each block and its ask and
tells the agent to ask the human to answer or waive it. A document already approved at its latest
version skips the reads and gets the server's answer. `dispatch doc-edit` refuses, before it sends
anything, a `delete` or `retype` by block id that would take an `ask` block out of the document
(the block itself, or one inside a deleted block) while its ask is open: the edit would write its
version at once and settlement would retract the ask without writing another, so the question
would leave the human's Inbox unanswered and the approval refusal would find no block to judge.
