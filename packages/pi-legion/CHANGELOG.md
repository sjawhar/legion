# Changelog

`@sjawhar/pi-legion` was split from `@sjawhar/pi-legion-envoy` 7.x (LEGION-247). Every release
before the split, and the Envoy plugin's releases after it, are in `packages/pi-envoy/CHANGELOG.md`.

## [Unreleased]

### Added

- A controller the Go daemon launches itself (`controller: daemon` in `legion.yaml`, LEGION-592)
  runs as a controller session: a session with `LEGION_CONTROLLER=1` and `LEGION_BOOT_TOKEN_FILE`
  registers on `/legion/v1/claims/register` with that boot token in place of a controller
  capability, claims `legion-<project>-controller`, subscribes to the controller topic, and then
  calls `/legion/v1/claims/ready`, when the daemon sends its start message. A claim step that fails
  exits Oh My Pi, so the daemon relaunches it. The operator-launched controller is unchanged.

### Changed

- An architect's one shell command is held to one scan whatever its head: a `legion` command now
  refuses `$()`, backticks, redirects and comments as a `dispatch` command does, where it refused
  only `;`, `&`, `|` and a newline (found in LEGION-588's review). A here-document ends only where
  bash ends it: at a line that is exactly its delimiter, with leading tabs stripped under `<<-`
  alone and no line's trailing blanks dropped, and the scan splits words on space and tab only,
  never a no-break space, byte-order mark or ideographic space, which bash reads as part of a word.
  A test holds the scan to a real bash over 2,822 generated commands.
- `legion.daemonApiVersion` is 15 (LEGION-588). Contract 15 moves the daemon's role prompts from
  native Dispatch tools to the `dispatch` command bundled by `@sjawhar/pi-envoy`; pair this
  release with a Go `legion` built from the same commit, so an agent is never instructed to use a
  surface its installed plugin does not provide.
- Contract 14 (LEGION-592) adds the daemon-launched controller's pod, whose worker container
  carries `LEGION_CONTROLLER=1` beside `LEGION_BOOT_TOKEN_FILE`: it registers with the launch's
  boot token, where an earlier plugin reads it as the operator's controller and never registers.
- **Breaking:** the Legion plugin is its own package, `@sjawhar/pi-legion`, split from
  `@sjawhar/pi-legion-envoy` 7.x (LEGION-247). It carries `extensions/legion.ts` (published as
  `dist/legion.js`), the Legion lifecycle modules (`src/`, the former `src/legion/`), the task agents
  in `agents/`, the eight Legion skills (`legion-architect`, `legion-controller`, `legion-oracle`,
  `legion-retro`, `legion-worker`, `ce-simplify-code`, `thermonuclear-code-quality`,
  `thermonuclear-deep-review`) at `dist/skills`, and `legion.daemonApiVersion`, which the split
  itself did not move (no request, response or pane variable changed): the pre-split package's last
  release left it at 13 (LEGION-583: the `push` grant for `legion push` and the worker image's
  `LEGION_REMOVABLE_WORKSPACES` payload, described in `packages/pi-envoy/CHANGELOG.md`), and this
  release declares 15 (LEGION-588, above) after main's 14. The Envoy messaging and Dispatch command
  every session uses are `@sjawhar/pi-envoy`'s, installed beside this package; a Legion pane needs
  both.
  The Legion entry claims roles, matches injected user turns and reads the bootstrapped session
  through the in-process interface the Envoy entry publishes (`@legion/pi-shared/interface`, version
  1), and in a Legion session refuses to run, naming the remedy, when no `@sjawhar/pi-envoy` is
  loaded, when the loaded one speaks another interface version, or when `@sjawhar/pi-legion-envoy`
  is still installed beside it (`omp plugin uninstall @sjawhar/pi-legion-envoy`). The daemon's boot
  gate, `legion probe-image` and `legion controller start` refuse the same three by name. Links from
  a Legion skill into the `dispatch` skill are `skill://dispatch/...`, which resolve once both plugins
  are installed. Install both into the daemon's Oh My Pi profile:
  `omp plugin install @sjawhar/pi-envoy && omp plugin install @sjawhar/pi-legion`.
