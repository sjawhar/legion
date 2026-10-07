# Pi Shared

Private workspace package (`@legion/pi-shared`, never published) holding what the two Oh My Pi
plugins — `@sjawhar/pi-envoy` (`extensions/envoy.ts`) and `@sjawhar/pi-legion`
(`extensions/legion.ts`) — both compile in: the versioned in-process interface between them, the
host types, and the modules both entries use. Each plugin's `bun build` inlines it into its own
bundle, as it inlines `@legion/contracts` and `@legion/envoy-client`; only `@oh-my-pi/*` stays
external. Nothing here imports either plugin's source, and neither plugin's shipped code imports
the other's: that rule covers every non-test `.ts` under each plugin's `extensions/` and `src/`
(`test/cross-imports.ts`, below) and never a test, so the two cross-entry tests
`packages/pi-legion/extensions/legion-role-claim.test.ts` and
`packages/pi-legion/extensions/legion-phase-stall-omp.test.ts` may load
`packages/pi-envoy/extensions/envoy.ts` by relative path.

## The interface (`src/interface.ts`)

Each plugin bundles its own copy of this module, so the one shared object lives on `globalThis`
under `ENVOY_PLUGIN_INTERFACE_KEY` and both copies reach it by `Symbol.for`. It is created by
whoever touches it first (`envoyPluginInterface()`, get-or-create at that copy's version), so the
plugins' factory order never matters; the Envoy entry *publishes* into it at factory time
(`publishEnvoyPluginInterface(import.meta.url)`), and the Legion entry *reads* it at `session_start`
and in `/legion-claim-controller` (`readEnvoyPluginInterface()`: `absent` until an Envoy entry
published, `mismatch` when the publisher speaks another version, else `present`) and refuses to run
otherwise. A second Envoy entry at the same version joins; one at another version warns once and
publishes nothing, so the first publisher wins. The object carries:

- `roleClaim` — the role-claim bridge (`src/role-claim-bridge.ts`): Envoy's bound instances, the
  sessions Legion drives, and the one `regained` hook slot.
- `injectedUserTurns` — the user turns Envoy sent into each session
  (`src/injected-user-turns.ts`), which both entries match user messages against.
- `bootstrappedSession` — the transcript of the session this process bootstrapped as its Legion
  identity (`src/subagent-session.ts`).

Every consumer resolves the object at each use through `envoyPluginInterface()`; nothing caches it
at module scope, and `resetEnvoyPluginInterfaceForTests()` is the one test seam.

**Symbols a Go probe reads from `globalThis`** (the daemon's boot gate evaluates them by their
exact `Symbol.for` strings, so renaming one is a daemon change too):

| constant | `Symbol.for(...)` string | value |
| --- | --- | --- |
| `ENVOY_PLUGIN_INTERFACE_KEY` | `legion.pi-shared.envoy-plugin-interface` | the interface object; `version` and `publishers` are what a probe reads |
| `LEGION_PLUGIN_LOADED_KEY` | `legion.pi-legion.loaded` | `{ from: import.meta.url, envoyInterface: ENVOY_PLUGIN_INTERFACE_VERSION }`, set by the Legion entry's factory |
| `LEGACY_LEGION_LOADED_KEY` | `legion.pi-envoy.legion-loaded` | the pre-split `@sjawhar/pi-legion-envoy`'s marker; set means that package is loaded, and the Legion entry refuses to run beside it |

**Bump rule.** Any change to the shape of `EnvoyPluginInterface`, or to what either side reads from
it, bumps `ENVOY_PLUGIN_INTERFACE_VERSION`; both plugins release from the same commit, and the
daemon's gate refuses a pair whose versions differ.

**No per-instance state.** The interface holds process-wide state only — the arrays, the map, the
record holder — never a function bound to one extension instance. Oh My Pi re-binds every
extension factory for each in-process `task` subagent, so a per-instance callback stored here
would be the last instance's, not the pane's
(`docs/solutions/envoy/heartbeat-role-reassertion-and-regain-hooks.md`). `roleClaim.regained`
stays the one listener slot it is, written only by the paths that establish a Legion identity.

## Test harness and the shared test rules (`test/`)

`test/omp-harness.ts` runs the real Oh My Pi binary (`LEGION_TEST_OMP`) for each plugin's
`*-omp.test.ts`; `test/omp-natives.ts` shares one natives cache per binary with the Go tests;
`test/host-registry.ts` is the `AgentRegistry` stand-in every plugin suite's mock spreads in.
`spawnRpc` takes **absolute** extension paths — this module lives in another package than its
callers, so `import.meta.dir` here is not the caller's — and callers pass
`path.join(import.meta.dir, "envoy.ts")`.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| The interface | `src/interface.ts`, `src/interface.test.ts` | The key, the version, the types, `envoyPluginInterface`, `publishEnvoyPluginInterface`, `readEnvoyPluginInterface`, the test seam; the test holds the get-or-create order independence, the second publisher at the same and at another version, the three readings, and the exact `Symbol.for` strings the Go probe reads |
| Modules both entries use | `src/role-claim-bridge.ts`, `src/injected-user-turns.ts`, `src/subagent-session.ts`, `src/tool-result.ts`, `src/pi-types.ts`, `src/omp-host.d.ts` | Each resolves the interface at each use; `omp-host.d.ts` is the ambient `@oh-my-pi/pi-coding-agent` declaration each plugin's `tsconfig` includes |
| Skills guard rules | `test/skills-guard.ts` | What `packages/pi-envoy/src/skills-guard.test.ts` and `packages/pi-legion/src/skills-guard.test.ts` run over each plugin's staged partition: `stageSkills(<package name>)` stages it through `scripts/pi-plugin-prepack.sh --stage-skills`, exactly as the prepack does; `oversizedFiles` (a file at or over Oh My Pi's 51,200-byte spill threshold), `longSkillBodies` (a `SKILL.md` of 500 lines or more), `misnamedSkills` (a frontmatter `name` that is not the directory's), `brokenSkillLinks` (a `skill://<name>/<path>[#anchor]` link naming a missing file or heading, resolved against the repository's `skills/`, where both partitions live), and `brokenRelativeLinks` (a `](../…)` or `](./…)` link outside a code fence that leaves the staged partition or names no file: a cross-partition link is written `skill://<name>/<path>`) |
| Skills partition | `test/skills-partition.test.ts` | Stages both partitions and holds them to the repository's `skills/`: together they are every skill directory, none is in both, each staged skill is a copy of the repository's, and a package the prepack does not know (`@sjawhar/pi-shared`) has none |
| No cross import | `test/cross-imports.ts` | `shippedSources(<package root>)` is every `.ts` under its `extensions/` and `src/` that is not a `*.test.ts`; `crossImports(<package root>, <sibling root>)` reports each relative import of those files that resolves into the sibling's directory. `packages/pi-envoy/src/no-cross-import.test.ts` and `packages/pi-legion/src/no-cross-import.test.ts` each run it against the other plugin |
| Real-binary harness | `test/omp-harness.ts`, `test/omp-natives.ts`, `test/omp-natives.test.ts`, `test/host-registry.ts` | Above |

## Checks

`bunx tsc --noEmit`, `bun test`, `bunx biome check src/ test/`, each from this directory.
