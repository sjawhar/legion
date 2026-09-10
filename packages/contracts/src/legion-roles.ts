import { ROLE_TOPIC_PREFIX } from "./subject";

/** A Legion issue key: a Dispatch key (`^[A-Z][A-Z0-9]*-[0-9]+$`, e.g. `LEGION-7`). Owner/repo
 * comes from `DaemonConfig`, never from this key — GitHub issues are never part of Legion's
 * lifecycle (see the T20 design). Not a template-literal union so it stays ergonomic as a plain
 * string through roles/state; every value at runtime matches `ISSUE_KEY_PATTERN`
 * (`legion-state.ts`). */
export type IssueKey = string;

export const LEGION_ROLES = [
  "architect",
  "planner",
  "implementer",
  "tester",
  "reviewer",
  "merger",
] as const;

export type LegionRole = (typeof LEGION_ROLES)[number];

export interface ParsedRoleToken {
  project: string;
  issue: IssueKey;
  role: LegionRole;
}

const ENVOY_ROLE_TOKEN = /^[a-z0-9][a-z0-9_-]*$/;
const PROJECT_TOKEN = /^[a-z0-9]+$/;
/** A Dispatch issue key: uppercase project, then a numeric suffix — `IssueKey`'s sole shape. */
const DISPATCH_KEY_PATTERN = /^([A-Z][A-Z0-9]*)-([0-9]+)$/;
const DISPATCH_ROLE_PART =
  /^([a-z0-9]+)-([0-9]+)-(architect|planner|implementer|tester|reviewer|merger)$/;

export function isLegionRole(role: string): role is LegionRole {
  return LEGION_ROLES.some((candidate) => candidate === role);
}

export function isLegionProjectToken(project: string): boolean {
  return PROJECT_TOKEN.test(project);
}

export function assertLegionProjectToken(project: string): void {
  if (!isLegionProjectToken(project)) {
    throw new Error(`Invalid Legion project token: ${project}`);
  }
}

export function roleToken(project: string, issue: IssueKey, role: LegionRole): string {
  assertLegionProjectToken(project);

  const dispatchMatch = DISPATCH_KEY_PATTERN.exec(issue);
  if (!dispatchMatch) throw new Error(`Invalid IssueKey: ${issue}`);
  const [, dispatchProject, number] = dispatchMatch;
  return `legion-${project}-${dispatchProject?.toLowerCase()}-${number}-${role}`;
}

export function sanitizeToken(part: string): string {
  const token = part
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");

  return token || "x";
}

export function controllerToken(project: string): string {
  assertLegionProjectToken(project);
  return `legion-${project}-controller`;
}

export function roleTopic(token: string): string {
  return `${ROLE_TOPIC_PREFIX}${token}`;
}

/** A Dispatch issue key encodes as `<project>-<number>-<role>`; its project component is already
 * `[A-Z][A-Z0-9]*`, so lowercasing it needs no further escaping. */
export function parseRoleToken(
  project: string,
  token: string
): ParsedRoleToken | { controller: true } | undefined {
  if (!isLegionProjectToken(project)) return undefined;

  const prefix = `legion-${project}-`;
  if (!ENVOY_ROLE_TOKEN.test(token) || !token.startsWith(prefix)) return undefined;

  const rest = token.slice(prefix.length);
  if (rest === "controller") return { controller: true };

  const dispatchMatch = DISPATCH_ROLE_PART.exec(rest);
  if (!dispatchMatch) return undefined;
  const [, dispatchProject, numberPart, role] = dispatchMatch;
  if (!dispatchProject || !numberPart || !role || !isLegionRole(role)) return undefined;
  return {
    project,
    issue: `${dispatchProject.toUpperCase()}-${numberPart}`,
    role,
  };
}
