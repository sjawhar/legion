---
title: "Two plugins in one process meet through a versioned object on globalThis, refused in the entry and again at the gate"
category: envoy
tags:
  - omp-extension
  - plugin-interface
  - globalThis
  - task-subagent
  - version-gate
  - boot-gate
  - pi-shared
date: 2026-10-07
status: active
module: packages/pi-shared
applies_when:
  - Two Oh My Pi plugins (or two entries of one) must share state or call each other in one process
  - A plugin's correctness depends on another plugin being installed at a matching version
  - A load-order assumption between extension factories is about to be made
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# Two plugins in one process meet through a versioned object on globalThis

- Put the shared state on `globalThis` under one `Symbol.for` key as a single object carrying its
  version number, created get-or-create by whichever side touches it first. Every consumer
  resolves it lazily at each use; nothing caches it at module scope, so publish order and read
  order between the two factories do not matter (Oh My Pi gives no order to cite).
- The object holds process-wide state only: arrays, maps, a holder. Never a function bound to one
  extension instance — the host re-binds every factory for each in-process `task` subagent, and
  the last binder would silently own the slot
  (docs/solutions/envoy/heartbeat-role-reassertion-and-regain-hooks.md).
- The publisher records itself; a second publisher at the same version joins, one at another
  version warns once and publishes nothing (first wins). The reader distinguishes `absent` (no
  publisher, even if a consumer created the object), `mismatch` (with the first publisher's
  origin) and `present`, and never throws on a version, so a session the dependent plugin does not
  serve keeps working with a mismatched pair.
- Any change to the object's shape or to what either side reads from it bumps the version; both
  plugins release from one commit.
- The dependent entry's refusal is a message, not a defence: a plugin loaded earlier can act
  before the entry refuses, and a person's session must never exit. Refuse the same conditions
  out of process, in every launch lane, before any session starts — from a probe that reads the
  same symbol strings off `globalThis` and prints them, never by importing the shared module into
  the gate, since the gate must judge whatever build is installed.

## Evidence

sjawhar/legion#1831, `packages/pi-shared/src/interface.ts`: `ENVOY_PLUGIN_INTERFACE_VERSION = 1`
under `Symbol.for("legion.pi-shared.envoy-plugin-interface")`, with `envoyPluginInterface()`
(get-or-create), `publishEnvoyPluginInterface(from)` (join or warn-once-and-decline) and
`readEnvoyPluginInterface()` (`absent` | `mismatch` | `present`); the role-claim bridge, the
injected-turn map and the bootstrapped-session holder replaced three ad-hoc symbols. The Legion
entry marks itself with `Symbol.for("legion.pi-legion.loaded") = { from, envoyInterface }` and, in
a Legion session only, logs one line and exits 1 when the pre-split package's marker is set or
the interface is absent or at another version (`packages/pi-legion/extensions/legion.ts`,
`envoyPluginRefusal`); a `not-legion` session returns before the check. The daemon's gate
(`packages/daemon/internal/daemon/bootgate.go`, `judgeLoaded`) refuses the same three cases in
the pane, pod and controller lanes from `probe.mjs`'s lines, which read the three symbol strings
directly, before judging prompt names — so an operator missing pi-envoy reads "install
@sjawhar/pi-envoy", never "finds no skill dispatch". The tester's negative controls at
7c21dd99: pi-envoy disabled → the Legion session exited before any turn with `needs
@sjawhar/pi-envoy at interface version 1, found none`; the real old package installed beside both
→ `found the pre-split @sjawhar/pi-legion-envoy loaded from …; uninstall …`; the gate refused each
in every lane.
