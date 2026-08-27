import {
  formatIssueKey,
  type IssueKey,
  LEGION_ROLES,
  type LegionRole,
  parseIssueKey,
} from "@legion/contracts";

export class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
  }
}

export function asRecord(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new HttpError(400, "Expected a JSON object");
  }
  return value as Record<string, unknown>;
}

/**
 * Structural view of a `@legion/contracts` zod schema. The daemon pins zod 3 while
 * the contracts package is on zod 4, so schemas cross the boundary by shape; the
 * type parameter keeps the parsed type visible to response construction.
 */
export interface ContractSchema<T = unknown> {
  parse(value: unknown): T;
}

export function validateContractRequest(
  schema: ContractSchema,
  body: Record<string, unknown>
): void {
  try {
    schema.parse(body);
  } catch {
    throw new HttpError(400, "Invalid Legion daemon API request");
  }
}

export function validateContractResponse<T>(
  schema: ContractSchema<T>,
  response: NoInfer<T>
): NoInfer<T> {
  schema.parse(response);
  return response;
}

export function requiredString(body: Record<string, unknown>, field: string): string {
  const value = body[field];
  if (typeof value !== "string" || value.length === 0) {
    throw new HttpError(400, `Expected non-empty string ${field}`);
  }
  return value;
}

export function optionalStrings(body: Record<string, unknown>, field: string): string[] {
  const value = body[field];
  if (value === undefined) {
    return [];
  }
  if (
    !Array.isArray(value) ||
    value.some((entry) => typeof entry !== "string" || entry.length === 0)
  ) {
    throw new HttpError(400, `Expected string array ${field}`);
  }
  return value;
}

export function requiredNumber(body: Record<string, unknown>, field: string): number {
  const value = body[field];
  if (typeof value !== "number" || !Number.isFinite(value) || !Number.isInteger(value)) {
    throw new HttpError(400, `Expected integer ${field}`);
  }
  return value;
}

export function issueKey(body: Record<string, unknown>, field: string): IssueKey {
  const value = requiredString(body, field);
  const parsed = parseIssueKey(value);
  if (!parsed) {
    throw new HttpError(400, `Expected issue key ${field}`);
  }
  return formatIssueKey(parsed.owner, parsed.repo, parsed.number);
}

export function legionRole(value: string): LegionRole {
  const role = LEGION_ROLES.find((candidate) => candidate === value);
  if (!role) {
    throw new HttpError(400, `Unknown Legion role ${value}`);
  }
  return role;
}
