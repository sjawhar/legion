import { logger } from "@oh-my-pi/pi-utils";
import type { SessionUserTurns } from "./injected-user-turns";
import type { LegionRoleClaimBridge } from "./role-claim-bridge";

/**
 * The in-process interface between the Envoy plugin (`@sjawhar/pi-envoy`, which owns the
 * listener registration, the role heartbeat and the Dispatch tools) and the Legion plugin
 * (`@sjawhar/pi-legion`, which claims roles and reads what Envoy delivered). Each plugin bundles
 * its own copy of this module, so nothing here is shared by import: the one object lives on
 * `globalThis` under a `Symbol.for` key both copies compute, and `ENVOY_PLUGIN_INTERFACE_VERSION`
 * says which shape a copy expects. Any change to `EnvoyPluginInterface`'s shape, or to what either
 * side reads from it, bumps the version; both plugins release from the same commit.
 *
 * The object holds process-wide state only — arrays, a map, a record holder — and never a function
 * bound to one extension instance: Oh My Pi re-binds every extension factory for each in-process
 * `task` subagent, so a per-instance callback stored here would be the last instance's, not the
 * pane's (docs/solutions/envoy/heartbeat-role-reassertion-and-regain-hooks.md).
 */
export const ENVOY_PLUGIN_INTERFACE_VERSION = 1;
export const ENVOY_PLUGIN_INTERFACE_KEY = Symbol.for("legion.pi-shared.envoy-plugin-interface");
/** Set by the Legion entry's factory to `{ from: import.meta.url, envoyInterface: <version> }`;
 * the daemon's boot gate reads it from `globalThis` to prove the Legion plugin loaded. */
export const LEGION_PLUGIN_LOADED_KEY = Symbol.for("legion.pi-legion.loaded");
/** The pre-split package's Legion marker; its presence means @sjawhar/pi-legion-envoy is loaded. */
export const LEGACY_LEGION_LOADED_KEY = Symbol.for("legion.pi-envoy.legion-loaded");
/** The `details` of an `envoy-message` the Envoy extension writes into its own session (the follow
 * notice after a Dispatch write, the session-id-changed notice): the session's own doing, never an
 * event from outside, so Legion's phase-stall check does not re-arm a quiet stall on it. */
export const LOCAL_ENVOY_NOTICE = { localNotice: true } as const;

export interface EnvoyPluginInterface {
  readonly version: number;
  /** `import.meta.url` of each Envoy entry that published, in publish order; empty until one does. */
  readonly publishers: string[];
  /** The role-claim bridge: Envoy's bound instances, the sessions Legion drives, the regain hook. */
  readonly roleClaim: LegionRoleClaimBridge;
  /** The user turns Envoy sent into each session, which both entries match user messages against. */
  readonly injectedUserTurns: Map<string, SessionUserTurns>;
  /** The transcript of the session this process bootstrapped as its Legion identity. */
  readonly bootstrappedSession: { file: string | undefined };
}

type EnvoyPluginInterfaceReading =
  | { readonly kind: "present"; readonly iface: EnvoyPluginInterface }
  | { readonly kind: "absent" }
  | {
      readonly kind: "mismatch";
      readonly found: number;
      readonly expected: number;
      readonly from: string;
    };

interface GlobalEnvoyPluginInterfaceStore {
  [key: symbol]: EnvoyPluginInterface | undefined;
}

const store = globalThis as typeof globalThis & GlobalEnvoyPluginInterfaceStore;

// This module instance warns about a version clash once: every later publish from it would name
// the same two versions and the same two entries.
let mismatchWarned = false;

/**
 * The one interface object, created at this build's version by whoever touches it first. A
 * consumer that runs before the Envoy entry publishes gets the same object the publisher later
 * joins, so neither side depends on factory order. An object at another version is returned as it
 * is: a consumer never fails on a version, so a non-Legion session with a mismatched pair keeps
 * working; the Legion entry decides through `readEnvoyPluginInterface`.
 */
export function envoyPluginInterface(): EnvoyPluginInterface {
  const existing = store[ENVOY_PLUGIN_INTERFACE_KEY];
  if (existing !== undefined) return existing;
  const created: EnvoyPluginInterface = {
    version: ENVOY_PLUGIN_INTERFACE_VERSION,
    publishers: [],
    roleClaim: { instances: [], managedSessions: new Set(), regained: undefined },
    injectedUserTurns: new Map(),
    bootstrappedSession: { file: undefined },
  };
  store[ENVOY_PLUGIN_INTERFACE_KEY] = created;
  return created;
}

/**
 * Records the Envoy entry at `from` as a publisher of the interface and returns the object it
 * should push its instance onto. A second entry at the same version joins (a subagent's re-bound
 * instance, or a second copy of the plugin). An entry at another version than the object already
 * holds publishes nothing: the first publisher wins, this one warns once and gets `undefined`,
 * which keeps a dev checkout usable where the root manifest's source entry loads beside the
 * installed bundle.
 */
export function publishEnvoyPluginInterface(from: string): EnvoyPluginInterface | undefined {
  const iface = envoyPluginInterface();
  if (iface.version !== ENVOY_PLUGIN_INTERFACE_VERSION) {
    if (!mismatchWarned) {
      mismatchWarned = true;
      logger.warn(
        `Envoy plugin interface version ${iface.version} is already published by ${iface.publishers[0] ?? "no Envoy entry (a consumer created it)"}; the Envoy entry at ${from} speaks version ${ENVOY_PLUGIN_INTERFACE_VERSION} and publishes nothing`,
        {
          found: iface.version,
          expected: ENVOY_PLUGIN_INTERFACE_VERSION,
          publishers: iface.publishers,
          from,
        }
      );
    }
    return undefined;
  }
  iface.publishers.push(from);
  return iface;
}

/**
 * What a Legion entry finds: `absent` when no Envoy entry has published (the object may exist,
 * created by a consumer), `mismatch` when the publishers speak another version (with the first
 * publisher's URL), else `present`.
 */
export function readEnvoyPluginInterface(): EnvoyPluginInterfaceReading {
  const iface = store[ENVOY_PLUGIN_INTERFACE_KEY];
  const from = iface?.publishers[0];
  if (iface === undefined || from === undefined) return { kind: "absent" };
  if (iface.version !== ENVOY_PLUGIN_INTERFACE_VERSION) {
    return {
      kind: "mismatch",
      found: iface.version,
      expected: ENVOY_PLUGIN_INTERFACE_VERSION,
      from,
    };
  }
  return { kind: "present", iface };
}

/** Test seam: `bun test` runs every file in one process, and the object is process-wide. */
export function resetEnvoyPluginInterfaceForTests(): void {
  delete store[ENVOY_PLUGIN_INTERFACE_KEY];
  mismatchWarned = false;
}
