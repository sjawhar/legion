# Pi Legion Extension

`@sjawhar/pi-legion` is the Oh My Pi plugin a Legion pane loads beside `@sjawhar/pi-envoy`
(`packages/pi-envoy`). It carries one extension entry, `extensions/legion.ts`: the Legion
lifecycle, which boots a root architect or a phase worker from the daemon's pane environment
(`LEGION_TREE`/`LEGION_ROLE`), registers the controller (`LEGION_CONTROLLER`), registers the
`legion` tool, whose operations mint their own daemon grants in-process, and holds the phase a
worker is in open until its `handoff_complete`. A session without that environment gets nothing from
it. The Envoy messaging and Dispatch tools every session uses are `@sjawhar/pi-envoy`'s, and this
plugin reaches them through the in-process interface that package publishes
(`packages/pi-shared/AGENTS.md`): in a Legion session the entry refuses to run, naming the remedy,
when no `@sjawhar/pi-envoy` is loaded, when the loaded one speaks another interface version, or
when the pre-split package is still installed beside it (`AGENTS.md`, "What this plugin needs from
pi-envoy").

## Installing both into the daemon's profile

The Legion daemon's boot gate (`packages/daemon/internal/daemon/bootgate.go`) refuses to start
unless the Oh My Pi a pane will run loads this plugin at the daemon's contract
(`legion.daemonApiVersion` in this package's `package.json`, equal to `DaemonAPIVersion` in
`packages/daemon/internal/api/version.go`) and loads `@sjawhar/pi-envoy` with it, so both go into
the Oh My Pi profile the daemon's panes use:

```sh
omp plugin install @sjawhar/pi-envoy && omp plugin install @sjawhar/pi-legion
```

Both plugins release from one commit, with the same version, and the gate refuses a pair whose
interface versions differ, so upgrade them together. A daemon on the Kubernetes runtime needs no
install on the daemon host for its pods: the worker image carries both, packed from the same
checkout as the `legion` binary, at `/opt/legion/pi-envoy` and `/opt/legion/pi-legion`, and
installs each into its `legion` profile (`packages/daemon/docker/worker.Dockerfile`,
`docs/kubernetes.md`). The operator's own machine, where `legion controller start` runs the
controller, installs both the same way.

## Published package

Released installs come from npm as `@sjawhar/pi-legion`. The tarball ships `dist/legion.js`, which
bundles every dependency except the Oh My Pi host packages (`@legion/pi-shared`, `@legion/contracts`
and `@legion/envoy-client` are inlined), `dist/THIRD_PARTY_NOTICES`, the eight Legion skills staged at
`dist/skills` (`legion-architect`, `legion-controller`, `legion-oracle`, `legion-retro`,
`legion-worker`, `ce-simplify-code`, `thermonuclear-code-quality`, `thermonuclear-deep-review`;
the manifest's `omp.skills: ["dist/skills"]` is what lets Oh My Pi discover them), and `agents/`,
the task agents Legion's prompts dispatch (`oracle`; the reviewer's pair
`thermonuclear-deep-review` and `thermonuclear-code-quality`; `deep-worker`; the planner's
`plan-gap-analyst`, which also reads a spec before a human does, and `plan-reviewer`), each
declaring its model as a role the operator maps (`docs/kubernetes.md`, "Model roles"). The Dispatch
and Envoy skills (`dispatch`, `dispatch-first`,
`dispatch-brainstorming`, `envoy`) ship in `@sjawhar/pi-envoy`; a link from a Legion skill into one
of them is written `skill://dispatch/...`, which resolves by name once both are installed.

`scripts/pi-plugin-prepack.sh` at the repository root is both plugins' `prepack`: it builds the one
entry, writes the notices and stages this package's partition of `skills/`. It refuses to pack with
the committed manifest, whose `omp.extensions` names `extensions/legion.ts`; the release workflow
(`.github/workflows/release.yaml`, job `pi_legion`), the worker image and the e2e pack step rewrite
it to `["dist/legion.js"]` before `bun pm pack` and restore it afterwards. The manifest's `legion`
key carries the daemon API contract the plugin was built against; `src/daemon-api-version.test.ts`
pins it to the daemon's golden fixture.

## Development install

The repository root `package.json` loads `../pi-envoy/extensions/envoy.ts` for dev sessions inside
this checkout and not this entry: `legion.ts` is inert outside a Legion pane, and a Legion pane runs
the installed packages. To run this entry from the checkout, link it into an Oh My Pi profile beside
the Envoy entry:

```sh
ln -sfn "$PWD/packages/pi-envoy/extensions/envoy.ts" ~/.omp/agent/extensions/envoy.ts
ln -sfn "$PWD/packages/pi-legion/extensions/legion.ts" ~/.omp/agent/extensions/legion.ts
```

The two cross-entry tests (`extensions/legion-role-claim.test.ts`,
`extensions/legion-phase-stall-omp.test.ts`) load both entries in one process; the second needs
the pinned Oh My Pi (`LEGION_TEST_OMP`). `CHANGELOG.md` records this package's releases; the history
before the split is in `packages/pi-envoy/CHANGELOG.md`.
