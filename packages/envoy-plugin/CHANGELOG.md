# Changelog

## [Unreleased]

### Changed

- `dispatch_request_approval` requires `summary`: the proposals in the document's latest version
  the human hasn't already agreed to, in one to three sentences (LEGION-387). The Inbox shows it
  after "Approve spec.md (version N)?", and the result text quotes the question the human sees.
  It needs a Dispatch server that accepts `summary`; an older one refuses the call.
- The `dispatch_issue` and `dispatch_doc_edit` descriptions no longer list spec headings; they
  point at the dispatch skill's "Writing a spec".

### Added

- With Dispatch configured, every OpenCode session carries the `dispatch-first` skill: the config
  hook adds `skills/dispatch-first/SKILL.md` to `instructions`, which OpenCode puts in the main
  loop's system prompt on every request and leaves out of title and compaction requests
  (LEGION-386). A package without the file fails to load naming it.

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.
