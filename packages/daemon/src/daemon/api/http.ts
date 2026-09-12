import { type IssueKey, LEGION_ROLES, type LegionRole } from "@legion/contracts";
import { ISSUE_KEY_PATTERN } from "../legion-state";

export class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
  }
}

/** Raised by `publishToEnvoy` (`../index.ts`) for a non-2xx Envoy response. `status` carries
 * Envoy's own HTTP status verbatim so callers can distinguish "no live session holds this role"
 * (404) from a genuine delivery failure without parsing the error message. */
export class EnvoyPublishError extends Error {
  constructor(
    readonly topic: string,
    readonly status: number
  ) {
    super(`Envoy publish to ${topic} failed with status ${status}`);
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
  safeParse(value: unknown):
    | { success: true; data: T }
    | {
        success: false;
        error: { issues: ReadonlyArray<{ path: ReadonlyArray<PropertyKey>; message: string }> };
      };
}

/** Rejects a body the route's contract does not accept with a 400 that names every offending
 * field (`version: Invalid input: expected number, received undefined`) or, for an unknown key on
 * a strict object, the key itself under `<body>` — so a caller sending a retired field learns
 * which one, instead of a bare "invalid request". */
export function validateContractRequest(
  schema: ContractSchema,
  body: Record<string, unknown>
): void {
  const result = schema.safeParse(body);
  if (result.success) return;
  const detail = result.error.issues
    .map((issue) => `${issue.path.map(String).join(".") || "<body>"}: ${issue.message}`)
    .join("; ");
  throw new HttpError(400, `Invalid Legion daemon API request: ${detail}`);
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
  if (!ISSUE_KEY_PATTERN.test(value)) {
    throw new HttpError(400, `Expected issue key ${field}`);
  }
  return value;
}

export function legionRole(value: string): LegionRole {
  const role = LEGION_ROLES.find((candidate) => candidate === value);
  if (!role) {
    throw new HttpError(400, `Unknown Legion role ${value}`);
  }
  return role;
}
