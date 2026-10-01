# Changelog

## [Unreleased]

### Changed

- `dispatch_request_approval` requires `summary`: the proposals in the document's latest version
  the human hasn't already agreed to, in one to three sentences (LEGION-387). The Inbox shows it
  after "Approve spec.md (version N)?", and the result text quotes the question the human sees.
  It needs a Dispatch server that accepts `summary`; an older one refuses the call.
- `dispatch_request_approval` is refused, with nothing sent, while the document holds an open
  decision block, and the refusal names each block.
- `dispatch_doc_edit` is refused, with nothing sent, when a `delete` or `retype` would take a
  decision block out of the document while its ask is open, even in a batch that inserts it
  again; the refusal says to reword it with `replace` or move it with `move`. A whole-document
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
