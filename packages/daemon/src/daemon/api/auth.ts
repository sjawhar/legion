import { createHash, timingSafeEqual } from "node:crypto";
import { type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import type { LegionState, WorkerRoleClaim } from "../legion-state";
import { HttpError, requiredString } from "./http";

export interface SessionCapability {
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  secretHash: Buffer;
}

/** A short-lived credential handle (`LEGION_GRANT`). A phase worker's or root architect's grant
 * carries the issue and `LegionRole` its session capability was minted for; the controller's
 * grant carries only `role: "controller"` — it has no issue, is not a phase, and is the one grant
 * `/gh-token` and `/git-credential` honour `merge: true` for. */
export type Grant =
  | { issue: IssueKey; role: LegionRole; sessionId: string; expiresAt: number }
  | { role: "controller"; sessionId: string; expiresAt: number };

export interface BootToken {
  tree: IssueKey;
  generation: number;
  sessionId?: string;
}

export interface WorkerBootToken {
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  generation: number;
  /** The claim's existing sessionId at mint time, for a respawn of an already-known agent. */
  expectedSessionId?: string;
  /** The session that actually consumed this token; set once, blocks replay. */
  sessionId?: string;
}

/** What a boot token resolves to: the claim it was minted for, and the mint record when this
 * process still holds one. */
export interface ResolvedWorkerClaim {
  /** The role claim token (`state.roles` key) the boot token was minted for. */
  token: string;
  claim: WorkerRoleClaim;
  /** The in-memory mint record when this process still holds it; `undefined` once the lookup fell
   * back to the `bootTokenHash` `launchWorker` persisted onto the claim (a restart since the mint). */
  boot: WorkerBootToken | undefined;
}

export function secretHash(secret: string): Buffer {
  return createHash("sha256").update(secret).digest();
}
export function spawnCapabilityKey(spawnToken: string): string {
  return secretHash(spawnToken).toString("hex");
}

export function equalSecret(expected: Buffer, supplied: string): boolean {
  const actual = secretHash(supplied);
  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

export function equalSecretHash(expectedHash: string, supplied: string): boolean {
  if (!/^[a-f0-9]{64}$/.test(expectedHash)) {
    return false;
  }
  return timingSafeEqual(Buffer.from(expectedHash, "hex"), secretHash(supplied));
}

/**
 * Owns the in-memory session capability, boot token, and grant state for one
 * running Legion API instance. These are per-process secrets (never persisted
 * to LegionState) that authenticate requests from worker sessions.
 */
export class CapabilityService {
  private readonly capabilities = new Map<string, SessionCapability>();
  private readonly bootTokens = new Map<string, BootToken>();
  private readonly workerBootTokens = new Map<string, WorkerBootToken>();
  private readonly grants = new Map<string, Grant>();

  constructor(private readonly now: () => number) {}

  async requireController(state: LegionState, body: Record<string, unknown>): Promise<void> {
    const secret = body.secret;
    if (
      typeof secret !== "string" ||
      secret.length === 0 ||
      !state.controllerCapabilityHash ||
      !equalSecretHash(state.controllerCapabilityHash, secret)
    ) {
      throw new HttpError(403, "Invalid controller capability");
    }
  }

  requireSessionCapability(
    body: Record<string, unknown>,
    tree: IssueKey,
    issue: IssueKey
  ): SessionCapability {
    const sessionId = body.sessionId;
    const suppliedSecret = body.secret;
    if (
      typeof sessionId !== "string" ||
      sessionId.length === 0 ||
      typeof suppliedSecret !== "string" ||
      suppliedSecret.length === 0
    ) {
      throw new HttpError(403, "Invalid session secret");
    }
    const capability = this.capabilities.get(sessionId);
    if (
      !capability ||
      capability.tree !== tree ||
      capability.issue !== issue ||
      !equalSecret(capability.secretHash, suppliedSecret)
    ) {
      throw new HttpError(403, "Invalid session secret");
    }
    return capability;
  }

  requireArchitectCapability(body: Record<string, unknown>, tree: IssueKey): SessionCapability {
    const sessionId = body.sessionId;
    const issuedCapability =
      typeof sessionId === "string" ? this.capabilities.get(sessionId) : undefined;
    const capability = this.requireSessionCapability(body, tree, issuedCapability?.issue ?? tree);
    if (capability.role !== "architect") {
      throw new HttpError(403, "Only an architect may perform lifecycle writes");
    }
    return capability;
  }

  resolveGrant(body: Record<string, unknown>): Grant {
    const grantId = requiredString(body, "grantId");
    const grant = this.grants.get(grantId);
    if (!grant || grant.expiresAt <= this.now()) {
      throw new HttpError(403, "Invalid or expired grant");
    }
    return grant;
  }

  setCapability(sessionId: string, capability: SessionCapability): void {
    this.capabilities.set(sessionId, capability);
  }

  /** Invalidates a session's capability the moment its process is observed dead, so a stale
   * credential file cannot keep minting grants until a respawn overwrites it, and revokes every
   * grant already minted for that session — without this, a grant minted before revocation
   * would otherwise keep redeeming GitHub tokens for up to its own `expiresAt`, since
   * `resolveGrant` only ever checks expiry, never the capability that minted it. A no-op if the
   * session never had a capability (already revoked, or never minted). */
  deleteCapability(sessionId: string): void {
    this.capabilities.delete(sessionId);
    for (const [grantId, grant] of this.grants) {
      if (grant.sessionId === sessionId) this.grants.delete(grantId);
    }
  }

  setGrant(grantId: string, grant: Grant): void {
    this.grants.set(grantId, grant);
  }

  registerBootToken(bootToken: string, tree: IssueKey, generation: number): void {
    this.bootTokens.set(bootToken, { tree, generation });
  }

  getBootToken(token: string): BootToken | undefined {
    return this.bootTokens.get(token);
  }

  registerWorkerBootToken(bootToken: string, info: Omit<WorkerBootToken, "sessionId">): void {
    this.workerBootTokens.set(bootToken, { ...info });
  }

  /** The one boot-token → claim lookup both `/worker/started` and the worker stream listener
   * perform. The in-memory mint record names the claim directly; without it (this daemon
   * restarted since the mint) the token is matched against every worker claim's persisted
   * `bootTokenHash`, one constant-time compare per claim. Callers apply their own generation and
   * session checks to the result; `undefined` means no claim anywhere was minted this token. */
  resolveWorkerClaim(state: LegionState, bootToken: string): ResolvedWorkerClaim | undefined {
    const boot = this.workerBootTokens.get(bootToken);
    if (boot) {
      const token = roleToken(state.project, boot.issue, boot.role);
      const claim = state.roles[token];
      if (claim && "issue" in claim) return { token, claim, boot };
    }
    for (const [token, claim] of Object.entries(state.roles)) {
      if (
        "issue" in claim &&
        claim.bootTokenHash !== undefined &&
        equalSecretHash(claim.bootTokenHash, bootToken)
      ) {
        return { token, claim, boot: undefined };
      }
    }
    return undefined;
  }
}
