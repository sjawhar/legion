import { ROLE_TOPIC_PREFIX } from "./subject";

/** A Legion issue key: a Dispatch key (`^[A-Z][A-Z0-9]*-[0-9]+$`, e.g. `LEGION-7`) going forward,
 * or a legacy `owner/repo#number` GitHub issue key on state a pre-Dispatch daemon has not yet
 * migrated. Not a template-literal union: both shapes are plain strings, distinguished at parse
 * time (see `roleToken`/`parseRoleToken`). */
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
const ISSUE_PART = /^[a-z0-9._-]+$/;
/** A Dispatch issue key: matches `IssueKey`'s Dispatch shape (uppercase project, then a numeric
 * suffix). Distinguishes a Dispatch key from a legacy `owner/repo#number` key in `roleToken` and
 * `parseRoleToken`, which encode each shape differently. */
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

function encodeIssuePart(part: string): string {
  const normalized = part.toLowerCase();
  if (!ISSUE_PART.test(normalized)) {
    throw new Error(`Invalid Legion issue token part: ${part}`);
  }

  return normalized.replaceAll("_", "_u").replaceAll(".", "_d").replaceAll("-", "_h");
}

function decodeIssuePart(part: string): string | undefined {
  let decoded = "";
  for (let index = 0; index < part.length; index += 1) {
    const character = part[index];
    if (character !== "_") {
      decoded += character;
      continue;
    }

    const escapeCode = part[index + 1];
    if (escapeCode === "u") decoded += "_";
    else if (escapeCode === "d") decoded += ".";
    else if (escapeCode === "h") decoded += "-";
    else return undefined;
    index += 1;
  }
  return decoded;
}

export function formatIssueKey(owner: string, repo: string, number: number): IssueKey {
  return `${owner}/${repo}#${number}`;
}

export function parseIssueKey(
  s: string
): { owner: string; repo: string; number: number } | undefined {
  const match = /^([^/#]+)\/([^/#]+)#(\d+)$/.exec(s);
  if (!match) return undefined;

  const owner = match[1];
  const repo = match[2];
  const numberPart = match[3];
  if (!owner || !repo || !numberPart) return undefined;

  const number = Number(numberPart);
  if (!Number.isSafeInteger(number)) return undefined;

  return { owner, repo, number };
}

export function sanitizeToken(part: string): string {
  const token = part
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");

  return token || "x";
}

export function roleToken(project: string, issue: IssueKey, role: LegionRole): string {
  assertLegionProjectToken(project);

  const dispatchMatch = DISPATCH_KEY_PATTERN.exec(issue);
  if (dispatchMatch) {
    const [, dispatchProject, number] = dispatchMatch;
    return `legion-${project}-${dispatchProject?.toLowerCase()}-${number}-${role}`;
  }

  const parsedIssue = parseIssueKey(issue);
  if (!parsedIssue) throw new Error(`Invalid IssueKey: ${issue}`);

  return `legion-${project}-${encodeIssuePart(parsedIssue.owner)}__${encodeIssuePart(parsedIssue.repo)}-${parsedIssue.number}-${role}`;
}

export function controllerToken(project: string): string {
  assertLegionProjectToken(project);
  return `legion-${project}-controller`;
}

export function roleTopic(token: string): string {
  return `${ROLE_TOPIC_PREFIX}${token}`;
}

/**
 * Role tokens preserve the complete lowercased issue identity. A Dispatch key encodes as
 * `<project>-<number>-<role>` (its project component is already `[A-Z][A-Z0-9]*`, so lowercasing
 * it needs no further escaping); a legacy `owner/repo#number` key keeps the `__`-separated
 * encoding below. The two are unambiguous: only the legacy encoding ever contains `__`.
 */
export function parseRoleToken(
  project: string,
  token: string
): ParsedRoleToken | { controller: true } | undefined {
  if (!isLegionProjectToken(project)) return undefined;

  const prefix = `legion-${project}-`;
  if (!ENVOY_ROLE_TOKEN.test(token) || !token.startsWith(prefix)) return undefined;

  const rest = token.slice(prefix.length);
  if (rest === "controller") return { controller: true };

  if (!rest.includes("__")) {
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

  const match =
    /^([a-z0-9_]+)__([a-z0-9_]+)-(\d+)-(architect|planner|implementer|tester|reviewer|merger)$/.exec(
      rest
    );
  if (!match) return undefined;

  const ownerPart = match[1];
  const repoPart = match[2];
  const numberPart = match[3];
  const role = match[4];
  if (!ownerPart || !repoPart || !numberPart || !role || !isLegionRole(role)) return undefined;

  const owner = decodeIssuePart(ownerPart);
  const repo = decodeIssuePart(repoPart);
  const number = Number(numberPart);
  if (!owner || !repo || !Number.isSafeInteger(number)) return undefined;

  return {
    project,
    issue: formatIssueKey(owner, repo, number),
    role,
  };
}
