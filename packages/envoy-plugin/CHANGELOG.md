# Changelog

## [Unreleased]

### Added

- With Dispatch configured, every OpenCode session carries the `dispatch-first` skill: the config
  hook adds `skills/dispatch-first/SKILL.md` to `instructions`, which OpenCode puts in the main
  loop's system prompt on every request and leaves out of title and compaction requests
  (LEGION-386). A package without the file fails to load naming it.

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.
