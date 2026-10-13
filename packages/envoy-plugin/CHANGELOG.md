# Changelog

## [Unreleased]

### Changed

- The plugin reads one envoy.json with a `dispatch` and an `envoy` section (LEGION-707):
  `$OPENCODE_CONFIG_DIR/envoy.json`, else the file `ENVOY_CONFIG_FILE` names, else
  `~/.config/legion/envoy.json`. During the warn-first migration, the old user file is read with a
  warning only when no harness directory is selected and that neutral file is at the default XDG
  location. A harness directory that is the default config directory follows the default-file rule.
  A profile with no file gets no Dispatch or Envoy; its notice names the existing old file and the
  chosen file to copy or link it to, or write. A relocated `XDG_CONFIG_HOME` says the old file is
  not read without asking to move it. A legacy checkout `.opencode/envoy.json` keeps only
  `dispatch`, ignoring top-level `natsUrls` and `envoy` with a warning; the current
  `.envoy/envoy.json` keeps refusing checkout settings. A top-level `natsUrls` in a user file is
  read as `envoy.natsUrls` with a warning.
- The `shell.env` hook sets `ENVOY_CONFIG_FILE` to that file beside `DISPATCH_HOST=opencode`, and the `dispatch` command reads the file `ENVOY_CONFIG_FILE` names whenever `DISPATCH_HOST` is set, so it reads the same one.
- With Envoy off the plugin registers no Envoy tool and calls no listener; Dispatch keeps its instruction file and session title.
- A `token` starting with `!` is a command whose output is the value. A current checkout's
  `.envoy/envoy.json` configures Dispatch alone; a legacy `.opencode/envoy.json` ignores its Envoy
  settings with a warning. A checkout's file that is not valid refuses Dispatch only while
  `DISPATCH_URL` is unset, and a checkout's `dispatch.token` needs its own `dispatch.serverUrl`
  beside it. The serve opens no NATS connection, so it never runs an `envoy.natsSeed` command.
- `DISPATCH_URL` and `ENVOY_URL` take a file's token only for that file's own server.
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
