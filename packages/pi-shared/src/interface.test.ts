import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { logger } from "@oh-my-pi/pi-utils";
import { matchInjectedUserTurn, noteInjectedUserTurn } from "./injected-user-turns";
import {
  ENVOY_PLUGIN_INTERFACE_KEY,
  ENVOY_PLUGIN_INTERFACE_VERSION,
  type EnvoyPluginInterface,
  envoyPluginInterface,
  LEGACY_LEGION_LOADED_KEY,
  LEGION_PLUGIN_LOADED_KEY,
  publishEnvoyPluginInterface,
  readEnvoyPluginInterface,
  resetEnvoyPluginInterfaceForTests,
} from "./interface";
import {
  claimEnvoyRole,
  type LegionRoleClaimInstance,
  legionRoleClaimBridge,
} from "./role-claim-bridge";

const store = globalThis as typeof globalThis & { [key: symbol]: unknown };

/** An Envoy instance as envoy.ts binds one, recording the claims that reach it. */
function instance(sessionID: string, claims: string[]): LegionRoleClaimInstance {
  return {
    claim: async (_sessionID, role) => {
      claims.push(role);
    },
    subscribe: async () => undefined,
    sessionID: () => sessionID,
  };
}

/** The object a build at `version` of this module would have created, published from `from`. */
function foreignInterface(version: number, from: string): EnvoyPluginInterface {
  return {
    version,
    publishers: [from],
    roleClaim: { instances: [], managedSessions: new Set(), regained: undefined },
    injectedUserTurns: new Map(),
    bootstrappedSession: { file: undefined },
  };
}

const warnings: logger.LogEvent[] = [];
let stopSink = (): void => undefined;

beforeEach(() => {
  resetEnvoyPluginInterfaceForTests();
  warnings.splice(0);
  stopSink = logger.registerLogSink((event) => {
    if (event.level === "warn") warnings.push(event);
  });
});

afterEach(() => {
  stopSink();
  resetEnvoyPluginInterfaceForTests();
});

describe("the Envoy plugin interface", () => {
  test("is created by whoever touches it first, and a later publisher joins that same object with the consumer's state intact", () => {
    // A Legion consumer runs before any Envoy entry published: it records a sent turn on the
    // object it created.
    noteInjectedUserTurn("ses_1", "ship it", "m-1");
    const consumerView = envoyPluginInterface();
    expect(readEnvoyPluginInterface()).toEqual({ kind: "absent" });

    const published = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");

    expect(published).toBe(consumerView);
    expect(published?.publishers).toEqual(["file:///plugins/pi-envoy/dist/envoy.js"]);
    expect(store[ENVOY_PLUGIN_INTERFACE_KEY]).toBe(consumerView);
    expect(
      matchInjectedUserTurn("ses_1", {
        role: "user",
        timestamp: 7,
        content: [{ type: "text", text: "ship it" }],
      })
    ).toBe("m-1");
  });

  test("a second publisher at the same version joins, and the instance it pushes is reachable through the first publisher's object", async () => {
    const first = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    const second = publishEnvoyPluginInterface("file:///checkout/packages/pi-envoy/envoy.ts");
    if (first === undefined || second === undefined) throw new Error("a publish was refused");
    const claims: string[] = [];
    second.roleClaim.instances.push(instance("ses_pane", claims));

    await claimEnvoyRole("ses_pane", "legion-omp-controller");

    expect(second).toBe(first);
    expect(first.publishers).toEqual([
      "file:///plugins/pi-envoy/dist/envoy.js",
      "file:///checkout/packages/pi-envoy/envoy.ts",
    ]);
    expect(first.roleClaim.instances).toHaveLength(1);
    expect(claims).toEqual(["legion-omp-controller"]);
    expect(legionRoleClaimBridge().managedSessions.has("ses_pane")).toBe(true);
    expect(warnings).toEqual([]);
  });

  test("a second publisher at another version warns once naming both versions and both entries, and does not join", () => {
    const other = ENVOY_PLUGIN_INTERFACE_VERSION + 1;
    const foreign = foreignInterface(other, "file:///plugins/pi-envoy-next/dist/envoy.js");
    store[ENVOY_PLUGIN_INTERFACE_KEY] = foreign;

    const refused = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    const refusedAgain = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");

    expect(refused).toBeUndefined();
    expect(refusedAgain).toBeUndefined();
    expect(foreign.publishers).toEqual(["file:///plugins/pi-envoy-next/dist/envoy.js"]);
    expect(warnings).toHaveLength(1);
    const [warning] = warnings;
    expect(warning?.message).toContain(`version ${other}`);
    expect(warning?.message).toContain(`version ${ENVOY_PLUGIN_INTERFACE_VERSION}`);
    expect(warning?.message).toContain("file:///plugins/pi-envoy-next/dist/envoy.js");
    expect(warning?.message).toContain("file:///plugins/pi-envoy/dist/envoy.js");
    // Consumers keep reading the one object, whatever its version.
    expect(envoyPluginInterface()).toBe(foreign);
  });

  test("reads absent until an Envoy entry published, mismatch for another version with the first publisher, and present otherwise", () => {
    expect(readEnvoyPluginInterface()).toEqual({ kind: "absent" });
    envoyPluginInterface();
    expect(readEnvoyPluginInterface()).toEqual({ kind: "absent" });

    const other = ENVOY_PLUGIN_INTERFACE_VERSION + 1;
    store[ENVOY_PLUGIN_INTERFACE_KEY] = foreignInterface(
      other,
      "file:///plugins/pi-envoy-next/dist/envoy.js"
    );
    expect(readEnvoyPluginInterface()).toEqual({
      kind: "mismatch",
      found: other,
      expected: ENVOY_PLUGIN_INTERFACE_VERSION,
      from: "file:///plugins/pi-envoy-next/dist/envoy.js",
    });

    resetEnvoyPluginInterfaceForTests();
    const published = publishEnvoyPluginInterface("file:///plugins/pi-envoy/dist/envoy.js");
    if (published === undefined) throw new Error("the publish was refused");
    expect(readEnvoyPluginInterface()).toEqual({ kind: "present", iface: published });
  });

  test("names the symbols the daemon's probe reads from globalThis, the old package's marker among them", () => {
    expect(Symbol.keyFor(ENVOY_PLUGIN_INTERFACE_KEY)).toBe(
      "legion.pi-shared.envoy-plugin-interface"
    );
    expect(Symbol.keyFor(LEGION_PLUGIN_LOADED_KEY)).toBe("legion.pi-legion.loaded");
    expect(Symbol.keyFor(LEGACY_LEGION_LOADED_KEY)).toBe("legion.pi-envoy.legion-loaded");
  });
});
