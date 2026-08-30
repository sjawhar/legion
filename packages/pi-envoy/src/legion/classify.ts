import { LEGION_ROLES, type LegionRole } from "@legion/contracts";
import type { SessionContext } from "../pi-types";

export type LegionSessionKind =
  | { kind: "root-architect"; tree: string }
  | { kind: "controller" }
  | { kind: "phase-worker"; role: LegionRole }
  | { kind: "sub-architect" }
  | { kind: "not-legion" };

export function classifySession(
  env: NodeJS.ProcessEnv,
  agentName: string | undefined,
  taskDepth: number
): LegionSessionKind {
  if (env.LEGION_CONTROLLER !== undefined && env.LEGION_TREE !== undefined) {
    throw new Error("Legion session has both controller and tree launch markers");
  }

  if (env.LEGION_CONTROLLER === "1") return { kind: "controller" };

  if (taskDepth === 0 && env.LEGION_TREE) {
    return { kind: "root-architect", tree: env.LEGION_TREE };
  }

  if (agentName === "legion-architect" && taskDepth >= 1) {
    return { kind: "sub-architect" };
  }

  const role = LEGION_ROLES.find((candidate) => agentName === `legion-${candidate}`);
  if (role && role !== "architect") return { kind: "phase-worker", role };

  return { kind: "not-legion" };
}

export function requiredEnvironment(env: NodeJS.ProcessEnv, key: string): string {
  const value = env[key];
  if (!value) throw new Error(`${key} is required for Legion`);
  return value;
}

export function positiveIntegerEnvironment(
  env: NodeJS.ProcessEnv,
  key: string,
  defaultValue: string
): number {
  const value = Number(env[key] ?? defaultValue);
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`${key} must be a positive integer`);
  }
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

export function isRootSession(env: NodeJS.ProcessEnv, context: SessionContext): boolean {
  if (context.taskDepth !== undefined) return context.taskDepth === 0;
  const rootWorkspace = env.LEGION_ROOT_WORKSPACE;
  return rootWorkspace ? context.cwd === rootWorkspace : true;
}
