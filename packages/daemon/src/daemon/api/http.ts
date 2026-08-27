import {
  formatIssueKey,
  type IssueKey,
  LEGION_ROLES,
  LegionDaemonApi,
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

export type ContractSchema = { parse(value: unknown): unknown };

export const REQUEST_SCHEMAS: Record<string, ContractSchema> = {
  "/legion/v1/process/started": LegionDaemonApi.ProcessStarted.request,
  "/legion/v1/process/ready": LegionDaemonApi.ProcessReady.request,
  "/legion/v1/merge-gate": LegionDaemonApi.MergeGate.request,
  "/legion/v1/process/exit": LegionDaemonApi.ProcessExit.request,
  "/legion/v1/spawn-token": LegionDaemonApi.SpawnToken.request,
  "/legion/v1/issues": LegionDaemonApi.IssueCreate.request,
  "/legion/v1/waves/release": LegionDaemonApi.WaveRelease.request,
  "/legion/v1/issues/comment": LegionDaemonApi.Comment.request,
  "/legion/v1/issues/body": LegionDaemonApi.PostBody.request,
  "/legion/v1/issues/labels": LegionDaemonApi.Labels.request,
  "/legion/v1/issues/close": LegionDaemonApi.IssueClose.request,
  "/legion/v1/escalate": LegionDaemonApi.Escalate.request,
  "/legion/v1/dispatch-threads": LegionDaemonApi.DispatchThread.request,
  "/legion/v1/phase": LegionDaemonApi.Phase.request,
  "/legion/v1/worker-session": LegionDaemonApi.WorkerSession.request,
  "/legion/v1/role-backing": LegionDaemonApi.RoleBacking.request,
  "/legion/v1/provisioning-credential": LegionDaemonApi.ProvisioningCredential.request,
  "/legion/v1/grants": LegionDaemonApi.Grant.request,
  "/legion/v1/git-credential": LegionDaemonApi.GitHubToken.request,
  "/legion/v1/gh-token": LegionDaemonApi.GitHubToken.request,
  "/legion/v1/controller/ready": LegionDaemonApi.ControllerReady.request,
  "/legion/v1/gates/approve": LegionDaemonApi.GatesApprove.request,
  "/legion/v1/admission": LegionDaemonApi.Admission.request,
  "/legion/v1/backlog": LegionDaemonApi.Backlog.request,
};

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

export function validateContractResponse<T>(schema: ContractSchema, response: T): T {
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
