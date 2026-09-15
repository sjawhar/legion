import type { SessionContext } from "../pi-types";

/**
 * The process-wide role-claim bridge between the two pi-envoy extensions. `extensions/envoy.ts`
 * registers each bound instance's claim entry point and fires the regain hook from its
 * heartbeat; `extensions/legion.ts` and `controller-session.ts` claim through it and register
 * that hook. It lives under `src/` so the session module never imports an extension entry point.
 */
export type LegionRoleClaim = (sessionID: string, role: string, context?: SessionContext) => Promise<void>;

/**
 * Why the heartbeat decided the listener had lost sight of this session's role: `"reclaimed"` —
 * the listener no longer named this session and a soft claim landed; `"reregistered"` — the
 * claim itself survived, but this session had been unreachable (a registry outage), so the
 * listener may have answered "no holder" for it meanwhile.
 */
export type RoleRegainReason = "reclaimed" | "reregistered";

type LegionRoleRegained = (role: string, reason: RoleRegainReason) => Promise<void>;

/**
 * One bound `envoyExtension(pi)` instance: its claim entry point and a live read of the session
 * id it currently serves. OMP re-binds every extension factory for each in-process `task`
 * subagent, so a process holds one instance per live agent session, and a claim for the pane's
 * session must reach the pane's instance — the one whose heartbeat re-asserts the role — not
 * whichever instance bound last (a subagent's, whose own heartbeat would then see the pane's
 * id as drift and hand the role to the subagent's session). `sessionID` reads the pane's live
 * `SessionManager`, never this instance's own module state: OMP mutates the manager before it
 * dispatches a `session_switch`, so the key is already current when legion.ts's handler for
 * that event claims — whichever of the two extensions OMP dispatches first. The module
 * variable lags until this instance's own rebind runs, and keying on it would make the routing
 * depend on handler order.
 */
export type LegionRoleClaimInstance = {
  readonly claim: LegionRoleClaim;
  readonly sessionID: () => string;
};

export type LegionRoleClaimBridge = {
  /** Every live instance in bind order; an instance removes itself on `session_shutdown`. */
  readonly instances: LegionRoleClaimInstance[];
  /**
   * legion.ts's regain hook. One slot — unlike `instances` — because several legion.ts
   * instances share a process (OMP re-binds every extension factory for each in-process `task`
   * subagent) but only an instance that has established a Legion identity registers here, so
   * the slot always holds the identity-bearing instance's listener.
   */
  regained: LegionRoleRegained | undefined;
};

interface GlobalLegionRoleClaimBridgeStore {
  [key: symbol]: LegionRoleClaimBridge | undefined;
}

// A process-wide symbol bridges legion.ts's `claimEnvoyRole` import to the
// envoyExtension(pi) instances OMP actually ran, since each manifest entry
// loads as its own module instance with its own module-scope state.
const LEGION_ROLE_CLAIM_BRIDGE = Symbol.for("legion.pi-envoy.role-claim-bridge");

export function legionRoleClaimBridge(): LegionRoleClaimBridge {
  const store = globalThis as typeof globalThis & GlobalLegionRoleClaimBridgeStore;
  const bridge = store[LEGION_ROLE_CLAIM_BRIDGE];
  if (bridge) return bridge;

  const createdBridge: LegionRoleClaimBridge = {
    instances: [],
    regained: undefined,
  };
  store[LEGION_ROLE_CLAIM_BRIDGE] = createdBridge;
  return createdBridge;
}

/**
 * Test seam. The bridge is process-wide and an instance leaves it only through OMP's
 * `session_shutdown`; a suite that binds a fixture per test without shutting it down clears the
 * bridge between tests, or a stale instance still serving a reused session id would capture a
 * later test's claim.
 */
export function resetLegionRoleClaimBridgeForTests(): void {
  const bridge = legionRoleClaimBridge();
  bridge.instances.length = 0;
  bridge.regained = undefined;
}

/**
 * Claims `role` for `sessionID` through the envoy instance currently serving that session — the
 * instance whose live `SessionManager` reports `sessionID` — so its heartbeat is the one that
 * re-asserts the claim afterwards. The key is the manager's id, not the instance's own module
 * state, so a claim made from legion.ts's `session_switch` handler routes correctly whether OMP
 * dispatched envoy.ts's rebind for the same event before or after it. With several instances
 * on the same id (a fixture that binds one per test) the most recently bound wins. A target no
 * instance serves yet falls back to the most recently bound instance, which establishes the
 * session itself.
 */
export async function claimEnvoyRole(
  sessionID: string,
  role: string,
  context?: SessionContext
): Promise<void> {
  const bridge = legionRoleClaimBridge();
  const instance =
    bridge.instances.findLast((candidate) => candidate.sessionID() === sessionID) ??
    bridge.instances.at(-1);
  if (instance === undefined) throw new Error("Envoy has no bound instance for a role claim");
  await instance.claim(sessionID, role, context);
}

/**
 * Registers the hook the heartbeat fires after it re-establishes this session as `role`'s live
 * holder (see `reassertRole`). legion.ts re-runs the role's daemon ready call from it. Last
 * registration wins — the bridge holds one `regained` slot, unlike its per-instance claim list —
 * so legion.ts calls this only from the paths that establish a Legion identity, never at
 * extension setup, or a `task` subagent's identity-less instance would replace the holder's
 * listener.
 */
export function onEnvoyRoleRegained(
  listener: (role: string, reason: RoleRegainReason) => Promise<void>
): void {
  legionRoleClaimBridge().regained = listener;
}
