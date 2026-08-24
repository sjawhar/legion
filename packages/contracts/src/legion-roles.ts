import { ROLE_TOPIC_PREFIX } from "./subject";

export type IssueKey = `${string}/${string}#${number}`;

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

function isLegionRole(role: string): role is LegionRole {
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
 * Role tokens preserve the complete lowercased owner/repository identity. The
 * separator is the only `__` sequence; `_u`, `_d`, and `_h` encode underscore,
 * dot, and hyphen within either component.
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
