# Changelog

## [Unreleased]

### Fixed

- The `dispatch` command waits for its stdout and stderr writes before it exits. Bun's
  `process.exit` dropped whatever a pipe had not yet taken, so a Bash tool lost the end of any
  output over the pipe's buffer (8 KB to 16 KB) with exit code 0.

### Changed

- The plugin reads one envoy.json with a `dispatch` and an `envoy` section (LEGION-707):
  `$OPENCODE_CONFIG_DIR/envoy.json`, else the file `ENVOY_CONFIG_FILE` names, else
  `~/.config/legion/envoy.json`. `~/.config/opencode/envoy.json` is not read: while it is there and
  the serve's file is missing, the logged notice says so and names the move
  (`mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`), or, for
  `$OPENCODE_CONFIG_DIR/envoy.json`, the file to copy or link it to; under a relocated
  `XDG_CONFIG_HOME` it says only that the old file is not read there. A top-level `natsUrls` is
  refused, naming `envoy.natsUrls`.
- No notice gives a `mv` across a mount: where the old file's directory is on another filesystem than the new file's nearest existing directory (an agent box's bind-mounted `~/.config/opencode`), it names the file to create and says to move the old one on the machine that owns it.
- The `shell.env` hook sets `ENVOY_CONFIG_FILE` to that file beside `DISPATCH_HOST=opencode`, and the `dispatch` command reads the file `ENVOY_CONFIG_FILE` names whenever `DISPATCH_HOST` is set, so it reads the same one.
- With Envoy off the plugin registers no Envoy tool and calls no listener; Dispatch keeps its instruction file and session title.
- A `token` starting with `!` is a command whose output is the value. A checkout's
  `.envoy/envoy.json` configures Dispatch alone; its `.opencode/envoy.json` is not read, and while
  `.envoy/envoy.json` is absent the logged notice and the `dispatch` command name the rename. A
  checkout's file that is not valid refuses Dispatch only while `DISPATCH_URL` is unset, and a
  checkout's `dispatch.token` needs its own `dispatch.serverUrl` beside it. The serve opens no NATS
  connection, so it never runs an `envoy.natsSeed` command.
- `DISPATCH_URL` and `ENVOY_URL` take a file's token only for that file's own server.
- One rule for `enabled` in both sections (envoy-client's README): `enabled: false` turns a section off; otherwise it is on when its address resolves, from the file or a variable, so an `envoy` section with `url` and `token` and no `enabled` is on with its bearer. A checkout's file is not read while `DISPATCH_URL` is set, so its token never goes to the server `DISPATCH_URL` names, and its `dispatch.enabled: true` alone turns nothing on.
- One logged notice replaces the `Dispatch disabled` and `Envoy disabled` warnings, and a relative `XDG_CONFIG_HOME`, `ENVOY_CONFIG_FILE` or `OPENCODE_CONFIG_DIR` no longer stops the plugin loading.
- Agents reach Dispatch through the `dispatch` command (LEGION-588). The `shell.env` hook puts
  the plugin's `bin/dispatch` first on every shell command's `PATH` and sets `DISPATCH_HOST`,
  `DISPATCH_SESSION_ID` and `DISPATCH_SESSION_TITLE`; the package ships `bin/` and builds the CLI
  into `dist/dispatch.js`, and `dist/THIRD_PARTY_NOTICES` covers both bundles.

### Removed

- The twenty-one native `dispatch_*` tools.

### Added

- The packaged skills gain `dispatch-brainstorming` (LEGION-475), which OpenCode finds on the
  skills path the config hook adds: in a session with Dispatch, a design conversation runs in the
  issue's spec and the plan is the issue's `plan.md` document. The `dispatch-first` instruction
  file sends each design conversation and plan to it.

### Changed

- `dispatch_request_approval`'s `summary` says only what the human is approving, with no commentary
  and no question, and an approval is requested only once the human has agreed to every point in
  the document (LEGION-475).
- `dispatch_request_approval` requires `summary`: the proposals in the document's latest version
  the human hasn't already agreed to, in one to three sentences (LEGION-387). The Inbox shows it
  after "Approve spec.md (version N)?", and the result text quotes the question the human sees.
  It needs a Dispatch server that accepts `summary`; an older one refuses the call.
- `dispatch_request_approval` is refused, with nothing sent, while the document holds an open
  decision block, and the refusal names each block.
- `dispatch_doc_edit` is refused, with nothing sent, when a `delete` or `retype` would take a
  decision block out of the document while its ask is open, even in a batch that inserts it
  again; the refusal names `replace`, `move` and, for the session that asked, `dispatch_edit_ask` instead. A whole-document
  replace through `dispatch_artifact` is not refused, so it can still remove an open block.
- The `dispatch_issue` and `dispatch_doc_edit` descriptions no longer list spec headings; they
  point at the dispatch skill's "Writing a spec".

### Added

- With Dispatch configured, every OpenCode session carries the `dispatch-first` skill: the config
  hook adds `skills/dispatch-first/SKILL.md` to `instructions`, which OpenCode puts in the main
  loop's system prompt on every request and leaves out of title and compaction requests
  (LEGION-386). A package without the file fails to load naming it.

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.

### Changed

- `dispatch_search` refuses a `query` over 1,000 characters (LEGION-386) and a `project` that is
  not a project key such as CORE before any request, naming the rule. Both ride in the search URL,
  which the load balancer in front of production Dispatch answers with a bare HTML `414` when it
  is too long, so this refusal is what stops a pasted passage from becoming that error; a
  lowercased key, which used to come back as no results, is now refused by name.
