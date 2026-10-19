# Changelog

## [Unreleased]

### Fixed

- The `dispatch` command waits for its stdout and stderr writes before it exits. Bun's
  `process.exit` dropped whatever a pipe had not yet taken, so every omp session, which reads the
  command through one, lost the end of any output over the pipe's buffer (8 KB to 16 KB) with exit
  code 0.
- A `dispatch` command that takes `--issue`, given a bare issue key in its place
  (`dispatch read ACME-12`), names the fix in its refusal: `use --issue <KEY>, as in: dispatch
  read --issue ACME-12`. The command still refuses a positional value.
- `dispatch comment`, `suggest`, `doc-edit` and `doc-read` refuse an ask, comment or message link
  as `--ref`, before any request. They take `--ref` as the issue or document they act on, so such
  a link used to post or read on its owner and drop the item: a comment meant for an ask's thread
  landed loose on the issue. The refusal names what acts on the item: `reply_to_ask` or `reply_to`
  (`--reply-to-ask`, `--reply-to`) under `dispatch comment`, otherwise `dispatch read`. `dispatch read` and
  `dispatch ask` still take such a link.

### Changed

- The `envoy_unsubscribe` and `envoy_role_set` descriptions say what an Oh My Pi session keeps:
  unsubscribe keeps the subscriptions another plugin holds for the session's role, and a move to a
  different role is refused while any are held. A host without such subscriptions behaves as
  before.
- `dispatch read`'s issue summary names the issue's page (`Link:`), on `DISPATCH_PUBLIC_URL` when
  it is set, so an agent asked about an issue repeats a link rather than building one.
- `slack_post` refuses a reply linking an address inside the cluster before it asks the broker
  anything, naming it: a Service name (`.svc`, a custom cluster domain after `.svc.`,
  `.cluster.local`, a single label in a URL), a private, loopback or link-local IP address (an
  IPv4-mapped one included), or, while `DISPATCH_PUBLIC_URL` names another host,
  `DISPATCH_URL`'s host. An http(s) URL, Slack's `<url|text>` form included, is read as a browser
  reads it; text with no scheme is judged as written, where an address is only a four-octet dotted
  quad or a bracketed IPv6 literal and a single label only `DISPATCH_URL`'s before a port, so a
  decimal, a version, `dispatch://ACME-12` and the word "Dispatch" are prose.
- `dispatch issue`'s `Created` line names the assignee Dispatch answered (`; assigned to <email>`
  or `; unassigned`): Dispatch picks one when the call names none, so an agent reports it rather
  than assuming.
- `dispatch issue`'s `Created <KEY>: <title>` line names the new issue's dashboard link
  (`→ <Dispatch URL>/issues/<KEY>`), so an agent can hand a person the issue it filed.
- `subscribe` sends `host` (`machineID()`: `ENVOY_MACHINE_ID`, falling back to the kernel
  hostname) on every call, including the registration heartbeat's re-subscribe: where the
  session process actually runs, which the listener now stores separately from the registering
  listener's own `machine_id`. `SessionInfo` and `RoleInfo` gain an optional `host`.
- Every text that says when to request approval again states the carried approval (LEGION-573):
  a human's own answer to a decision block carries their approval to the version it writes when
  that answer is the whole change since the version they approved, so nothing is requested for
  it; a change outside the decision blocks makes the approval stale, and the request that follows
  carries a `--summary` naming what changed since the earlier approval. A stale approval's line
  in a document read, `dispatch request-approval`'s refusal while a decision block is open (its
  answered-block line says to wait for the version, no longer to request again) and its two
  result texts say so.
- **Breaking:** Dispatch and Envoy are read from one envoy.json, chosen per harness (LEGION-707):
  `resolveDispatchConfig`, `activeDispatchConfig` and `DispatchConfigOptions` are gone. A caller
  reads Dispatch with `sessionDispatchSettings(file, env, { cwd })` over the file
  `sessionConfigFile(harness, env)` names (`envoyConfigPath`) and the checkout's
  `.envoy/envoy.json` beside it, and `activeDispatchSettings` over its result. A missing
  file contributes nothing, so the variables still configure each section. A checkout's file
  configures Dispatch alone: an `envoy` section there is not read, and the start notice says
  `<checkout>: envoy is not read from a checkout's envoy.json; put it in <chosen file>`; a checkout's
  file that cannot be read or is not valid refuses Dispatch only while `DISPATCH_URL` is unset, and
  never touches Envoy. `DISPATCH_URL` with the chosen file's token at another origin than the server
  written beside it is refused naming `DISPATCH_TOKEN` and `DISPATCH_TOKEN_FILE`. The chosen file's
  `dispatch.enabled: false` is off whatever the variables say. `DISPATCH_URL` replaces a checkout's
  file whole: while it is set, the checkout's server, token and `dispatch.enabled: false` are neither
  used nor refused, so a checkout's token never goes to the server `DISPATCH_URL` names. Otherwise a
  checkout contributes only its `dispatch.enabled: false`, or its own server with its own token: its
  `dispatch.enabled: true` alone turns nothing on. A checkout's server without its own token is
  refused, and so is its token without its own `dispatch.serverUrl` unless `DISPATCH_TOKEN` or
  `DISPATCH_TOKEN_FILE` replaces it; either refusal turns off Dispatch alone. An empty `DISPATCH_*`
  variable counts as unset.
- **Breaking:** one rule for `enabled` in both sections. `enabled: false` turns a section off
  whatever else is set; otherwise the section's fields apply whether or not it sets `enabled`, and
  it is on when its address resolves, from the file or a variable (Dispatch: `dispatch.serverUrl` or
  `DISPATCH_URL`; Envoy: `envoy.url` or `ENVOY_URL`, `envoy.natsUrls` or `ENVOY_NATS_URL`), or at
  the default address `enabled: true` names. An `envoy` section `{url, token}` with no `enabled` is
  now on with its bearer, where it lent nothing before, and a `dispatch` section `{serverUrl, token}`
  with no `enabled` is on too. A section that names no address is off with
  `<file> names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off` or
  `<file> names no Envoy listener or NATS server, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off`.
- **Breaking:** the OpenCode-era files are not read. While `~/.config/opencode/envoy.json` is there
  and the chosen file is missing, both sections stay off unless a variable names one, and the
  missing-file notice says the old file is not read and what to do: for
  `~/.config/legion/envoy.json`, `mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`;
  for a profile's own file, copy or link the old file to it, or write it; under a relocated
  `XDG_CONFIG_HOME`, only that it is not read there. A checkout's `.opencode/envoy.json` is not
  read: while the checkout has no `.envoy/envoy.json`, the start notice and the `dispatch` command
  name the rename. Neither move is a `mv` across a mount: where the old file's directory and the
  nearest existing ancestor of the new file's directory are on different filesystems (as in an
  agent box that bind-mounts `~/.config/opencode`), the notice reads `<old file> is not read, and is
  on another filesystem than <new file> (a mount): create <new file>, and move <old file> on the
  machine that owns it` (`readEnvoyConfig`'s `deviceOf` option answers a directory's device). A
  top-level `natsUrls` is refused, naming `envoy.natsUrls`.
- `createEnvoyClient` takes the listener's bearer as `token` and reads no environment;
  `natsAuthOptions` takes a resolved `NatsSeed` (`envoySettings`, or `natsSeedFromEnvironment`).
- The Envoy resolver's module is `envoy-settings` (`@legion/envoy-client/envoy-settings`), which
  was `defaults`: `envoySettings`, `natsSeedFromEnvironment`, `DEFAULT_ENVOY_URL` and the settings
  types.
- Every command resolves Envoy through `sessionEnvoySettings`, so a location its rule cannot work
  out (a relative `ENVOY_CONFIG_FILE`, harness directory or `XDG_CONFIG_HOME`) arrives as that
  caller's Envoy-off refusal: `envoy-send` says `Envoy is off: <refusal>`,
  `scripts/e2e/lib/operator-nats-seed.ts` says `the NATS seed for <label> …: <refusal>`, and the
  skill scenarios' `daemon-pane.ts` says `envoy: <refusal>`; those two refuse an Envoy that is on
  with inbound messages off (`inbound.off`) the same way.
- The `dispatch` command reads the envoy.json its session's plugin chose: with `DISPATCH_HOST` set,
  the file `ENVOY_CONFIG_FILE` names (each plugin sets the two together), else its host plugin's
  rule (`omp`, `claude` or `opencode`); with `DISPATCH_HOST` unset and
  `CLAUDE_CODE_CHILD_SESSION=1`, as in a Claude Code subagent's Bash that lacks what the
  `SessionStart` hook writes, host `claude` and Claude Code's rule. With Dispatch off it prints the
  resolver's own notice (`dispatch: no envoy.json at <path>: Dispatch is off`,
  `dispatch: <file> sets dispatch.enabled to false: Dispatch is off`, or
  `dispatch: <file> names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`),
  exiting 2.
- The `dispatch` command keeps its output whole when the session's own state fails (LEGION-588).
  Under Claude Code the whole output, every line the CLI adds counted, stays under 25,000
  characters: past it the result text and picture lines are written whole to a file the output
  names, the text is cut first, then picture lines are dropped from the end with a line saying how
  many, and the follow notice and the CLI's own lines are kept. A `state.json` that is not JSON
  exits 2 naming the file before any request; a picture, ledger or memory write that fails after
  Dispatch took the call prints the result and one line saying so. `readResultsSince` names a
  ledger line that is not JSON, with its file and line number, to the callback it now takes, and
  reads past it. An ask option whose label holds `: ` is written `--options-json`, so every option
  list round-trips through `commandLine`. A single-value flag given twice is refused as given twice
  whether or not its first value parsed.
- `hasErrnoCode` is exported from `errors`, where claude-envoy now imports it.
- A refusal names a Dispatch tool by its `dispatch` command and flags always; `ToolInputError`
  and `formatZodIssues` no longer take a `syntax` option.

### Removed

- `requiresSubscriptionCapability` on every Envoy tool spec: nothing read it.
- `envoyDefaultsFromEnvironment`, replaced by `envoySettings`.

### Added

- `dispatch issue --repo <owner/name>` and `dispatch issue-update --repo` (`''` clears) set the
  repository an issue targets, and `dispatch read` prints a `Repository:` line: `owner/name (own)`,
  `owner/name (inherited or project default)`, or `none`. Against a Dispatch server older than the
  repository fields, the read prints `Repository: unknown (…)` and returns its warning in the new
  `DispatchToolResult.warnings`, which `runDispatchCli` writes to the new, required
  `DispatchCliIo.stderr`; a `--repo` write whose answer has no `repo` fails, saying it was not saved.
- `DISPATCH_PUBLIC_URL`, Dispatch's dashboard as people open it (sjawhar/legion#1998): every link
  the `dispatch` command prints is resolved against it when it is set, so an agent that reaches
  Dispatch at an in-cluster address prints links people can open, and a pasted dashboard link on
  either address resolves. One that is not an http or https URL turns Dispatch off, naming it.
- `slack_post` joins `envoyToolSpecs` after the ten `envoy_*` tools (`channel`, `thread_ts`,
  `text` at most `SLACK_POST_TEXT_MAX`), and `postSlackReply` (`@legion/envoy-client/slack-post`)
  posts it with Slack's `chat.postMessage` (sjawhar/legion#1998). The bot token is the secrets
  broker's agent secret `LEGION_SLACK_BOT_TOKEN`, released through the broker's exec form,
  `agent-secrets LEGION_SLACK_BOT_TOKEN --reason … --wait 0s -- bun -e <poster>`, so it is in the
  environment of the process that posts and never the session's. A request waiting for approval,
  a denial or any other broker refusal posts nothing and names the state; a post is sent once and
  never retried, since a post whose answer was lost may already be in the thread.
  `SLACK_POST_API_URL` (https, or http on a loopback host) moves the Web API for a stand-in.
- `envoy-config`: `envoyConfigPath` (a plugin's own harness directory, else `ENVOY_CONFIG_FILE`,
  else `${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`; a script, harness `command`, tries
  `ENVOY_CONFIG_FILE` and then each harness's directory first, Claude Code's only under
  `CLAUDE_CODE_CHILD_SESSION=1`, so a `CLAUDE_CONFIG_DIR` a plain terminal exports is not read; a
  relative `ENVOY_CONFIG_FILE`, harness directory or `XDG_CONFIG_HOME` is refused when the rule
  uses it), `readEnvoyConfig`, which validates both sections strictly, names where JSON stops
  parsing by line and column alone,
  and runs no command, and `fileValue`, which runs a `token` or `natsSeed` starting with `!` as a
  command (under `/bin/sh -c` in the directory of the envoy.json that names it, never the session's
  working directory, in its own process group for at most 10 s, its exit status deciding,
  its value kept for the process and shared across module instances, keyed by the command, its file
  and its working directory, and so is a run out of time, so a hanging command never runs again in
  that process, while a failure that ended on its own (a non-zero exit, empty output, more than one
  line) is kept for 60 s and then run again; never from a checkout's file, a value with
  whitespace inside refused, its output never in an error) when that section's resolver uses it.
- `envoySettings(env, config, { nats })`: the Envoy a session uses, `{ enabled: true, url, token,
  inbound, heartbeatMs }` or a `SectionOff`, `{ enabled: false, notice, error }` with exactly one of
  the two set, each variable overriding its own field of the chosen file's section; `ENVOY_URL`
  with the file's token at another origin than the file's listener is refused, as is an empty
  `envoy.token`, with the same messages as `dispatch.token`'s, and the file's `natsSeed` goes only
  with the file's `natsUrls`, compared as normalised URLs (scheme and host lower-cased, a trailing
  `/` dropped, the default port 4222 written), so `ENVOY_NATS_URL` naming other servers needs
  `NATS_NKEY_SEED_FILE`. `inbound` is `{ natsUrls, natsSeed }` or `{ off }`, `off` saying why the
  session takes no inbound messages: `Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)`;
  a refused seed (that pairing, a failing `natsSeed` command, or a seed variable that cannot be
  read), which leaves Envoy on with its listener and bearer; or `this session opens no NATS
  connection` for `nats: false`, a caller that opens none, which resolves no seed.
  `sessionSettings`'s start notice says `Envoy's inbound messages are off: <off>; the Envoy tools
  still work`, except under `nats: false`, where it says nothing of inbound messages.
- `dispatchSettings(env, config)`, Dispatch from an envoy.json already read, and
  `activeDispatchSettings(resolution)`, the one adapter that turns a resolution into the
  configuration a caller uses, null when nothing configures Dispatch, or a `dispatch config:
  <reason>` throw.
- `session-settings`: `sessionConfigFile(harness, env)`, the file a harness rule chooses, and
  `handedConfigFile(env)`, the file `ENVOY_CONFIG_FILE` names, each with a relative path's refusal
  as an `error` instead of a throw; `sessionSettings(file, env, { cwd, nats })`, both sections and
  the one start notice a plugin shows; `sessionDispatchSettings(file, env, { cwd })`, Dispatch read
  now with the read's `warnings` and missing-file record, which every command and plugin resolves
  Dispatch through; and `sessionEnvoySettings(file, env, { cwd, nats })`, Envoy alone, for a
  command.
- `dispatch doc-read --blocks` lists the live document's blocks instead of its markdown, one line
  each in document order, indented under the block holding it: id, type, token, anchored comments
  and asks, and opening words sliced from the live markdown by the block's byte range. Under a
  table it lists each row by the index `delete_row` takes and each cell by its row, the column
  `delete_column` takes and the header drawn above it, as Dispatch places them; a Dispatch that
  does not report table rows yet is said to, with no index read from the markdown. It refuses a
  version and an uploaded file, naming the file, and a document that changed between its two
  reads of the text (LEGION-693).
- `dispatch project --key <KEY> --name <name>` creates a Dispatch project as the calling session
  (`DispatchClient.createProject`, `POST /api/v1/projects`) and prints it with its dashboard link.
  A taken key exits 1 with `PROJECT_EXISTS: …`, and a bad key or an empty name with
  `INVALID_PROJECT: …` (LEGION-684).
- `dispatch projects` lists every Dispatch project, one per line, its key then its name
  (`DispatchClient.listProjects`, `GET /api/v1/projects`), so an agent checks the keys that exist
  before `dispatch project` creates one (LEGION-684).
- A delivered `ask.answered` that changes an earlier answer renders `previous_answer`, an issue
  log names the answer it replaced, and `dispatch_read` of an ask lists its earlier answers
  (LEGION-622).
- Pictures in Dispatch (LEGION-541). `dispatch_message`, `dispatch_comment` and `dispatch_ask` take
  `images`, local paths of PNG, JPEG, GIF or WebP files (by their bytes) of at most 25 MiB: each is
  uploaded to the issue, the project document's project, or, for a reply to a direct message, the
  session's own conversation (`DispatchClient.agentArtifact`, `POST /api/v1/agents/{session}/artifacts`),
  and appended as one `![<file name>](dispatch://<owner>/artifact/<slug>@vN)` line after a blank
  line. The lines count toward the body and question caps; a call over the cap is refused with the
  count before any upload, and a bad file is refused naming its path. `DispatchToolResult.images`
  carries pictures the model is shown: `dispatch_doc_read` of an image (a
  `dispatch://agent/<session id>/artifact/<slug>@vN` reference included) returns it, and
  `dispatch_read` returns the pictures the messages, asks and comments it shows embed, newest
  first, at most 8 and 10 MiB per read, with a `Pictures:` section naming each and the rest; a
  picture over 3,750,000 bytes (5 MB of base64, the providers' bound) or of another type is described.
  A host session is shown each picture once: a later `dispatch_read` names one it was already shown
  as `shown earlier this session; dispatch_doc_read shows it again` instead of sending its bytes,
  counting toward neither per-read cap, because every request carries the session's history and
  Anthropic refuses one over 32 MB; `dispatch_doc_read` always shows the picture. A picture counts
  as shown once the model has it, a tool result once it is returned and a host's delivery once the
  host took it, so `readPictures` only reads the session's pictures and returns the ones it shows
  (`PicturesRead.shown`). `dispatch_doc_read` names its picture on the `- image 1: <address> (...)`
  line `dispatch_read` uses, and `shownPictureAddresses` reads those lines back from a transcript,
  so a host counts the pictures a session it moves onto already carries. A host forgets a session
  it stops serving (`forgetShownPictures`), and past 64 sessions the one used longest ago is
  forgotten. A `dispatch://agent/<session id>/...` reference reads its session id by
  `SESSION_ID_PATTERN`. `readPictures`, `shownPictures`, `shownPictureAddresses` and
  `forgetShownPictures` (`@legion/envoy-client/dispatch-picture-tools`),
  `@legion/envoy-client/dispatch-pictures` and `RenderInboundResult.pictures` serve the hosts'
  deliveries.

### Changed

- `dispatch_issue` and `dispatch_ask` append LEGION-550's write-time feedback to their result,
  from the new `WriteAdvice.suggestions`: up to three related items found by search (`Possibly
  related, found by search:`, each with its status or owner and a link), and, for an ask, any
  already-answered ask that may settle the same question (`A past decision may already answer
  this: …, answered by <who> on <when>`). A slow or down search never blocks or delays the write;
  `suggestions.missing` renders as `Related-item search was skipped: <reason>.` instead.
- `dispatch_search` takes `offset` and says how far a page reaches: its first line reads
  `20 results for "merge queue" (showing 1-20 of 1328, 78 ms)` when the query matches more than the
  page, a line says when some matches cannot be paged to (each kind lists only its best 100) and to
  narrow the query or name a project, and the last line names the next offset while more can be
  reached. `details` carries `total`, `reachable`, `offset` and `limit`. A Dispatch from before
  search paging answers no total and ignores `offset`, so a later page from it is refused rather
  than shown as its first page again (LEGION-386, LEGION-382). `SearchOptions` takes `offset`.
- Every text that says to request approval again says to do so only once the human has agreed to
  every point in the new version (LEGION-475): a stale approval's line in a document read;
  `dispatch_request_approval`'s refusal while a decision block is open, after an answer is folded
  in or a waived block's decision is written in; and its two result texts, for the request it
  opened or handed back and for a version already approved. The refusal says that answering a
  block would move the request to the new version and leave it waiting on the agent, and that the
  next request opens the request or hands an open one back to the human; the request's result
  says an edit before the answer moves the request and an edit after approval makes the approval
  stale, and that the next request hands the request back or opens a new one.
- `DispatchClient.requestApproval` returns `recorded`, false when Dispatch answered 200 because the
  open request already stood as asked, and `dispatch_request_approval` says which it was: "Approval
  requested" when the call opened the request or handed it back (rewording it first when the
  summary was new), and "already waits on the human, so this call changed nothing" otherwise,
  naming what would have left it waiting on the agent (LEGION-470). A different summary while the
  request waits on the human is Dispatch's `409 APPROVAL_WAITS_ON_HUMAN`, reported as the error it
  is. `DispatchClient.comment` and `artifactComment` return `CommentWriteResponse`, the comment
  plus `ask_waiting_on` on a reply to an open ask, which `dispatch_comment` reports as the ask's
  state after the reply. Delivery accepts the new `ask.handed_back` event, whose payload is the ask.
  A document's approval line in `dispatch_read` and `dispatch_doc_read` says whom an awaiting
  request waits on (`Approval: awaiting, waiting on agent (…)`), so an agent whose own revision
  moved its request, which sends it no event, reads that the request waits on it.
  `DispatchClient.requestApproval` answers `ApprovalRequestResponse` plus `recorded`.
- `DispatchClient.requestApproval` takes `{ actor, summary }`, and `dispatch_request_approval`
  sends `summary` and quotes the question the server returned in its result (LEGION-387).
- `dispatch_request_approval` is refused, with no request sent, while the version it would name
  (the document's latest) holds a decision block open (LEGION-387): answering the block would
  write a newer version than the one the request names. A block is judged by its state in that
  version, so an answer not yet folded into a version still counts, and so does a block with no
  ask yet; every line that opens the block counts, so one quoting its opener (in code, say) can add
  an open block but never hide one. The refusal names each block and its ask, and tells the agent
  to ask the human to answer or waive it. It reads `GET /artifacts/{id}/blocks` first, then, only
  when an `ask` block is present, the owner's asks and that version's markdown; an approved
  document skips the reads.
- `dispatch_doc_edit` is refused, with nothing sent, when a `delete` or `retype` by block id would
  take a decision block out of the document while its ask is open, even in a batch that inserts
  markdown carrying its id (LEGION-387). The edit would write its version at once and settlement
  would retract the ask without another, so the question would leave the human's Inbox
  unanswered and an approval request sent next would find no open block. The refusal names
  `replace`, `move` and, for the session that asked, `dispatch_edit_ask` instead. It covers `dispatch_doc_edit` only: a
  whole-document replace through `dispatch_artifact` is sent with no reads and can still remove an
  open block. An edit with no `delete` or `retype` by block id reads nothing more; one with reads
  `GET /artifacts/{id}/blocks`, and the owner's asks only when it reaches an `ask` block.

### Added

- `dispatch-execute` exports `asObject`, which reads an unknown value as a JSON object or null; the
  Oh My Pi extension's run-end nudge reads a tool result's details with it (LEGION-470).

- `dispatch_read` of an anchored comment or ask prints `Position:` after the quote, where the
  quote's block stands: `table[3] › row 5 (Red-teamer loop), column Due` for a table cell (the
  row's index, 0 the header, labelled by its cells before the anchored one, and the column's
  header), or the path of types and child indexes outside a table (`positionText`), and
  `Position: unavailable (<code>)` when the read carries `anchor_block_error` instead. An ask's
  summary also prints its `> quote`, which it did not before (LEGION-460).
- Added the shared nine-tool native Dispatch client, typed results, and per-issue event subscription details.
- `setRole` takes `soft` and `previousSessionID` and returns `{ claimed: true, interest }` or `{ claimed: false, holder }`, so a caller can recover a role without displacing a live holder.
- `resolveIssueDocumentId` resolves an issue's document reference (`spec`, or its id, slug or
  filename) to the document's id as the Dispatch tools resolve an issue's `artifact` argument.
- `activeDispatchSettings` is the one "is Dispatch configured" check every host shares: the
  resolved URL and token, null when none is configured, and a `dispatch config: <reason>` throw on
  a broken configuration.
- `DispatchClient.acceptMessageDelivery(id, attempt, {actor})` calls
  `POST /api/v1/messages/{id}/deliveries/{attempt}/accept` and returns the accepted attempt with
  the message's stored `body` (`AcceptedMessageDelivery`); a refusal throws a
  `DispatchServiceError` naming Dispatch's check. A rendered Dispatch message delivery carries the
  frame's `broadcast_id` as `DispatchDelivery.broadcastId` (LEGION-394).

### Changed

- `dispatch_read` of an issue prints an `External links:` section: each URL a person or an agent
  linked on the issue (the pull request that delivers it among them), with its kind, as the issue
  page shows them. Before, an agent had no tool that showed a linked pull request.
- A session's claim, on `dispatch_read` and on each `dispatch_issues` row, says `· not running`
  when the live agent registry loaded and does not list the holder — the judgement the
  dashboard's claim chip makes, through `claimHolds` — and `· liveness unknown` when the registry
  could not be read. Before, the line named the holder either way, with nothing to say whether its
  session still ran.
- `dispatch_issues` asks Dispatch for its page, sending `limit` and `offset` on
  `GET /api/v1/issues` through `DispatchClient.listIssuePage`, which replaces `listIssues`
  (LEGION-406), and uses the page Dispatch serves, `{issues, total, limit, offset}`, as it is.
  Any other answer, a page missing one of its four fields included, is refused, naming the
  Dispatch and the request it was sent and what arrived. A JSON answer that is not a page comes
  back the same, so the refusal says a retry will not help; text in its place looks like a proxy or
  gateway page, so it says a retry may succeed. A bare array answered to the paged request, which a
  Dispatch older than #1612 sends, was paged by the client itself until every Dispatch the hosts
  reach, the production deploy included, ran #1612. It is now refused, naming a Dispatch older than
  that change or a regression of it.
- The `envoy_subscribe` description says a `pr.<n>.checks` settlement is published for every
  commit of the pull request whose checks settle, the head or not, and names its `sha`
  (LEGION-208).

### Fixed

- `dispatch issue-update --clear-external-links` actually clears every external link instead of
  re-sending the issue's existing set unchanged: `dispatch_issue_update`'s executor now replaces
  the set with nothing when the request names an explicit empty list, instead of merging it with
  zero new links. A non-empty list still merges by URL, so linking a pull request still keeps
  every earlier link.

- `dispatch_open_asks` links an issue ask by its item route, `/issues/<KEY>/asks/<id>`, which
  `dispatchRefFromUrl` reads back as the ask; the link it printed before named the issue alone
  (LEGION-622).
- `dispatch_doc_read` of an uploaded file or image returns the file's text at its latest or named
  version, after a line naming its type, version and size, where it failed with Dispatch's
  `artifact is not a document`: it asked `/text`, which answers a file 400 `NOT_DOCUMENT`. A file
  that is not UTF-8 text is described, with the `GET /api/v1/artifacts/{id}/versions/{n}` route that
  serves its bytes. `DispatchClient.fileVersion` reads one version's bytes and MIME type.
- `dispatch_doc_edit` given `lost_ops: null` says the live document is being reloaded or holds a
  tree too deep to read, where it said only that it was being reloaded: a document past the
  schema's depth bound reaches no verdict either, and a re-read answers it `DOC_SCHEMA`
  (LEGION-465).
- A non-2xx answer whose body is not Dispatch's `{code, error}` JSON — a gateway's or proxy's HTML
  page, an empty body, JSON of another shape — reached every `dispatch_*` tool as its raw body
  (LEGION-457). `DispatchClient` now throws it as a `DispatchGatewayError` (a
  `DispatchServiceError` whose `code` stays `HTTP_<status>` and whose `fromDispatch` is false)
  reading `<METHOD> <url with query> answered <status> [<reason>] with <a body that is not
  Dispatch's error JSON ("<excerpt>") | a body of <N> bytes and no readable text [in its first 64
  KiB] | an empty body>, which looks like a proxy or gateway page rather than Dispatch's own
  answer, so <advice>.` The advice fits the method and the status: a status that cannot clear gets
  "a retry gets the same answer until the Dispatch URL, or whatever answers in its place, is
  fixed"; a 5xx, 408 or 429 gets "a retry may succeed" on a GET; on a write, a 408 or 429 (the
  gateway's own timeout or rate limit, sent before it forwards anything) gets "the write did not
  reach Dispatch, and a retry may succeed", and a 5xx, which can come after Dispatch applied the
  write, gets "the write may or may not have reached Dispatch: check whether it took effect before
  retrying it". The error carries `answer` (the message without the advice), `advice`,
  `transient` and `mayHaveReachedDispatch`. `dispatch_issue_update`'s close path reads one rule for
  whether its reason's post or its close may have taken effect: only Dispatch's own 4xx or a
  gateway's answer that never reached Dispatch proves it did not, while a 5xx (Dispatch's own
  included, since it can fail after it committed), a timeout or a transport error says the reason,
  or the close, may or may not have landed, in place of the client's advice; after an error
  message that ends a sentence (`The operation timed out.`), that account starts one of its own. A
  gateway's 408 or 429 on the close says the issue did not close and how to retry without posting
  the reason twice, with nothing to fix. A status is read as Dispatch's meaning only from
  Dispatch's own answer: a gateway's 404 page is reported as such, not as no issue linked to an
  external reference (with the advice to create one), no such project document, no comments on a
  document, or a reference section the server does not serve, and a gateway's 500 on
  `external_links` gets no link-clash hint. The excerpt and the reason phrase are each one line of
  at most 120 characters with scripts, styles and tags dropped, and with the value after
  `Authorization:` or `Bearer` and every run of 8 or more of the client's own bearer's characters
  (trimmed; a shorter bearer only whole) redacted first, so a copy cut short, split by markup or
  overlapping another leaves no such piece. The excerpt reads at most the first 64 KiB of the body,
  redacting the bearer within that slice and taking whole a piece that runs past its end, and every
  pattern it runs is linear, so a body of any size or shape cannot hold the host's event loop. The
  URL never carries the bearer, which travels in a header. Dispatch's own JSON errors render as
  before: their `error` text under their `code`.

- `createDeliveryDedupe` replaces `rememberBounded` as the one dedupe both core-NATS hosts keep: it
  recognises a re-send by its `dedupe_key`, and only for a key that names its event
  (`dedupeKeyNamesItsEvent` in `@legion/contracts`: every Dispatch key, a webhook key of its
  delivery id, a key the listener or this package's transport minted once for its message), which
  it remembers for `DELIVERY_DUPLICATE_WINDOW_MS`. Any other key is never a repeat: the latest-1,000
  memory it replaces dropped a later event that shared a key with an earlier one (the MCP bridge's
  content hash, the Go daemon's outbox row id). It also recognises a second copy of one publish,
  arriving on an overlapping subscription, by the `event_id` the copies share, whatever their key,
  holding the last 5,000 to 10,000 event ids apart from the keys. A host `claim`s a frame before
  anything it awaits, which records it and says whether it is a repeat in one step: `undefined`
  for a repeat, otherwise a `DeliveryClaim` whose `release` undoes exactly what that claim
  recorded, once, for a frame its agent was not handed (a delivery that threw, a frame it answered
  with an error), so its re-send still arrives. `DedupeIdentitySchema` is the identity a host
  parses a frame into for the claim. The record holds at most `DELIVERY_DEDUPE_KEY_LIMIT`
  (250,000) keys and forgets the oldest past it, so a producer that floods a followed topic with
  fresh keys cannot grow a host's memory without bound.
- `getArchitectureSource` reads both answers a server gives for a project with no architecture
  source as `null`: a current server's `200 null` and an older server's `404 SOURCE_NOT_FOUND`.
  A client meets both while a rollout mixes versions. Before, only `200 null` read as none, so
  against an older server every `dispatch_issue` create in a project without a source ended with
  "Could not check whether project … has an architecture model" and the advice to link components.
  Any other failure, a 404 with another code included, is still reported as a source the client
  could not check.

- A bare document reference (an `artifact` argument that is not a `dispatch://` reference's own
  document) that is one document's slug and another's filename on the same issue or project
  (Dispatch suffixes a slug two documents would share, so `spec-v2` can be both) is refused as
  naming two documents, with each one's id, instead of silently taking the slug's document. On a
  project the slug route's answer is now checked against the project's unlinked documents, so an id
  also outranks another project document's slug there, as it does on an issue. The document part of
  a `dispatch://<issue>/artifact/<slug>` or `dispatch://<PROJECT>/artifact/<slug>` reference, and
  of a dashboard document URL, is a slug and resolves by slug, never refused for a clash.
- Non-creation tools resolve external issue references to linked native issues without creating
  them, and `dispatch_read` follows ask and comment references to their targeted results.
- Suggestions without a rationale omit `body` from their request.
