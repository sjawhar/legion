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

- `legion.daemonApiVersion` is 15 (LEGION-578). Contract 15 adds `capabilities` to
  `GET /legion/v1/state`: the deployment's capability report, one row per capability with its
  `status` (`present`, `unchecked`, `live`, `withheld`, `decided` or `open`), its `detail`, and on
  an open row the `configLine` to write into `legion.yaml`. Install this release with a Go `legion`
  built from the same commit; the daemon's image probe refuses a worker image whose plugin
  declares 14.
- `legion.daemonApiVersion` is 14 (LEGION-592). Contract 14 adds the daemon-launched controller's
  pod, whose worker container carries `LEGION_CONTROLLER=1` beside `LEGION_BOOT_TOKEN_FILE`: this
  release registers it with the launch's boot token, where an earlier one reads it as the
  operator's controller and never registers. Install this release with a Go `legion` built from
  the same commit; the daemon's image probe refuses a worker image whose plugin declares 13.
- **Breaking:** the Legion plugin is its own package, `@sjawhar/pi-legion`, split from
  `@sjawhar/pi-legion-envoy` 7.x (LEGION-247). It carries `extensions/legion.ts` (published as
  `dist/legion.js`), the Legion lifecycle modules (`src/`, the former `src/legion/`), the task agents
  in `agents/`, the eight Legion skills (`legion-architect`, `legion-controller`, `legion-oracle`,
  `legion-retro`, `legion-worker`, `ce-simplify-code`, `thermonuclear-code-quality`,
  `thermonuclear-deep-review`) at `dist/skills`, and `legion.daemonApiVersion`, which the split
  itself did not move (no request, response or pane variable changed): the pre-split package's last
  release left it at 13 (LEGION-583: the `push` grant for `legion push` and the worker image's
  `LEGION_REMOVABLE_WORKSPACES` payload, described in `packages/pi-envoy/CHANGELOG.md`), and this
  release declares 15 (LEGION-578 and LEGION-592, above). The Envoy messaging and Dispatch tools every session loads are
  `@sjawhar/pi-envoy`'s, installed beside this package; a Legion pane needs both.
  The Legion entry claims roles, matches injected user turns and reads the bootstrapped session
  through the in-process interface the Envoy entry publishes (`@legion/pi-shared/interface`, version
  1), and in a Legion session refuses to run, naming the remedy, when no `@sjawhar/pi-envoy` is
  loaded, when the loaded one speaks another interface version, or when `@sjawhar/pi-legion-envoy`
  is still installed beside it (`omp plugin uninstall @sjawhar/pi-legion-envoy`). The daemon's boot
  gate, `legion probe-image` and `legion controller start` refuse the same three by name. Links from
  a Legion skill into the `dispatch` skill are `skill://dispatch/...`, which resolve once both plugins
  are installed. Install both into the daemon's Oh My Pi profile:
  `omp plugin install @sjawhar/pi-envoy && omp plugin install @sjawhar/pi-legion`.
