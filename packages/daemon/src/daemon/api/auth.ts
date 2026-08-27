import { createHash, timingSafeEqual } from "node:crypto";
import type { IssueKey, LegionRole } from "@legion/contracts";
import type { LegionState } from "../legion-state";
import { HttpError, requiredString } from "./http";

export interface SessionCapability {
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  secretHash: Buffer;
}

export interface Grant {
  issue: IssueKey;
  role: LegionRole;
  expiresAt: number;
}

export interface BootToken {
  tree: IssueKey;
  generation: number;
  sessionId?: string;
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

  setGrant(grantId: string, grant: Grant): void {
    this.grants.set(grantId, grant);
  }

  registerBootToken(bootToken: string, tree: IssueKey, generation: number): void {
    this.bootTokens.set(bootToken, { tree, generation });
  }

  getBootToken(token: string): BootToken | undefined {
    return this.bootTokens.get(token);
  }
}

/**
 * Tracks whether the controller's startup redelivery has already run for the
 * current daemon process, so a controller reconnecting on the same boot never
 * replays it twice. Reset whenever a fresh controller capability is minted.
 */
export class ControllerGate {
  private ready = false;
  private readyAttempt: Promise<void> | undefined;
  private generation = 0;

  async ensureReady(onReady: () => Promise<void>): Promise<void> {
    if (this.ready) return;
    if (this.readyAttempt) {
      await this.readyAttempt;
      return;
    }

    const generation = this.generation;
    const attempt = (async () => {
      await onReady();
      if (this.generation === generation) {
        this.ready = true;
      }
    })();
    this.readyAttempt = attempt;
    try {
      await attempt;
    } finally {
      if (this.readyAttempt === attempt) {
        this.readyAttempt = undefined;
      }
    }
  }

  reset(): void {
    this.generation += 1;
    this.ready = false;
    this.readyAttempt = undefined;
  }
}
