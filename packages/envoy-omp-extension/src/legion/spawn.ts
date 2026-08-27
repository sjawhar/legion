import path from "node:path";
import { LEGION_ROLES, type LegionRole, parseRoleToken } from "@legion/contracts";
import type { SessionContext } from "./pi-types";
import type { WorkerSpawn } from "./worker-budget";

export const legionSpawnBlockPattern = /<legion-spawn\s+([^>]*?)\/>/g;

export function parseWorkerSpawn(prompt: string, project: string): WorkerSpawn | undefined {
  const blocks = [...prompt.matchAll(legionSpawnBlockPattern)];
  if (blocks.length !== 1) return undefined;
  const block = blocks[0];
  if (!block) return undefined;

  const attributes = new Map<string, string>();
  const attributePattern = /([A-Za-z]+)="([^"]*)"/g;
  for (const attribute of block[1].matchAll(attributePattern)) {
    const key = attribute[1];
    const value = attribute[2];
    if (!key || value === undefined || attributes.has(key)) return undefined;
    attributes.set(key, value);
  }
  if (block[1].replace(attributePattern, "").trim() !== "") return undefined;

  const issue = attributes.get("issue");
  const role = attributes.get("role");
  const token = attributes.get("token");
  const tree = attributes.get("tree");
  const spawnToken = attributes.get("spawnToken");
  const workspace = attributes.get("workspace");
  if (!spawnToken) {
    throw new Error("Legion spawn block is missing daemon-issued recovery token");
  }
  if (
    !issue ||
    !role ||
    !token ||
    !tree ||
    !workspace ||
    !LEGION_ROLES.includes(role as LegionRole)
  )
    return undefined;

  const parsedToken = parseRoleToken(project, token);
  if (
    !parsedToken ||
    "controller" in parsedToken ||
    parsedToken.issue !== issue ||
    parsedToken.role !== role
  )
    return undefined;
  return { tree, issue, role, token, spawnToken, workspace };
}
export function workerAgentId(context: SessionContext): string {
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile?.endsWith(".jsonl")) {
    throw new Error("Legion worker session must have a persisted transcript");
  }
  const agentId = path.basename(sessionFile, ".jsonl");
  if (!agentId) throw new Error("Legion worker transcript has no agent id");
  return agentId;
}
