import { isLegionRole, type LegionRole } from "@legion/contracts";

export type LegionSessionKind =
  | { kind: "controller" }
  | { kind: "root-architect"; tree: string }
  | { kind: "phase-worker"; role: LegionRole; tree: string; issue: string }
  | { kind: "not-legion" };

export function classifySession(env: NodeJS.ProcessEnv): LegionSessionKind {
  if (env.LEGION_CONTROLLER !== undefined && env.LEGION_TREE !== undefined) {
    throw new Error("Legion session has both controller and tree launch markers");
  }

  // The controller marker is LEGION_CONTROLLER alone. The daemon also sets
  // LEGION_ROLE=controller on that process, but "controller" is not a
  // LegionRole and this extension never reads LEGION_ROLE for it — one
  // signal, checked once, so the two markers can never disagree in practice.
  if (env.LEGION_CONTROLLER === "1") return { kind: "controller" };

  if (env.LEGION_ROLE === undefined) return { kind: "not-legion" };

  if (!isLegionRole(env.LEGION_ROLE)) {
    throw new Error(`LEGION_ROLE "${env.LEGION_ROLE}" is not a Legion role`);
  }
  const tree = requiredEnvironment(env, "LEGION_TREE");
  const issue = requiredEnvironment(env, "LEGION_ISSUE");
  // The daemon roots an issue tree at itself: the root architect's own issue
  // key equals the tree's. A sub-architect on a child issue is a phase
  // worker like any other role — bootstrapWorker already special-cases
  // role === "architect" for tool registration.
  if (env.LEGION_ROLE === "architect" && issue === tree) {
    return { kind: "root-architect", tree };
  }
  return { kind: "phase-worker", role: env.LEGION_ROLE, tree, issue };
}

export function requiredEnvironment(env: NodeJS.ProcessEnv, key: string): string {
  const value = env[key];
  if (!value) throw new Error(`${key} is required for Legion`);
  return value;
}

export function requiredControllerCapability(env: NodeJS.ProcessEnv): string {
  const secret = env.LEGION_CONTROLLER_SECRET;
  if (!secret) {
    throw new Error(
      "LEGION_CONTROLLER_SECRET is required to claim the controller. " +
        "Launch OMP with LEGION_CONTROLLER_SECRET in its environment before running " +
        "/legion-claim-controller."
    );
  }
  return secret;
}

export function generation(env: NodeJS.ProcessEnv): number {
  const value = Number(requiredEnvironment(env, "LEGION_GENERATION"));
  if (!Number.isSafeInteger(value)) throw new Error("LEGION_GENERATION must be an integer");
  return value;
}
