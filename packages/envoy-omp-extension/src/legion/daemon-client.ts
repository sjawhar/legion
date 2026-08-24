import type { LegionRole } from "@legion/contracts";
import { z } from "zod";

const EmptyResponseSchema = z.object({}).passthrough();
const ProcessStartedResponseSchema = z.object({
  roleTokens: z.record(z.string(), z.string()),
  controlSubject: z.string(),
  secret: z.string(),
});
const IssueCreateResponseSchema = z.object({ issue: z.string(), url: z.string() });
const WaveReleaseResponseSchema = z.object({ released: z.array(z.string()) });
const CommentResponseSchema = z.object({ commentId: z.number().int(), url: z.string() });
const LabelsResponseSchema = z.object({ labels: z.array(z.string()) });
const PhaseResponseSchema = z.object({
  secret: z.string(),
  gitName: z.string(),
  gitEmail: z.string(),
});
const SpawnTokenResponseSchema = z.object({ spawnToken: z.string() });
const AdmissionResponseSchema = z.object({
  result: z.union([z.literal("spawned"), z.literal("queued")]),
});
const GrantResponseSchema = z.object({ grantId: z.string(), expiresAt: z.string() });
const DispatchThreadResponseSchema = z.object({ thread: z.number().int(), url: z.string() });

export interface ProcessStartedInput {
  readonly tree: string;
  readonly generation: number;
  readonly rootSessionId: string;
  readonly bootToken: string;
  readonly ompSessionFile: string;
}

export interface IssueCreateInput {
  readonly tree: string;
  readonly title: string;
  readonly body: string;
  readonly labels: readonly string[];
}

export interface WaveReleaseInput {
  readonly tree: string;
  readonly children: readonly string[];
}

export interface IssueTextInput {
  readonly tree: string;
  readonly issue: string;
  readonly body: string;
}

export interface IssueCloseInput {
  readonly tree: string;
  readonly issue: string;
  readonly comment?: string;
}

export interface LabelsInput {
  readonly tree: string;
  readonly issue: string;
  readonly add: readonly string[];
  readonly remove: readonly string[];
}

export interface EscalateInput {
  readonly tree: string;
  readonly kind: "re-file" | "capacity" | "cross-tree";
  readonly context: unknown;
}

export interface DispatchThreadInput {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly sessionId: string;
  readonly secret: string;
  readonly parent: string;
  readonly subject: string;
  readonly body: string;
  readonly ask?: unknown;
  readonly urgency?: "low" | "med" | "high" | "blocking";
}

export interface SpawnTokenInput {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly sessionId: string;
}

export interface RoleBackingInput extends SpawnTokenInput {
  readonly agentId: string;
  readonly spawnToken: string;
}

export interface PhaseInput {
  readonly tree: string;
  readonly issue: string;
  readonly phase: string;
  readonly sessionId: string;
  readonly spawnToken?: string;
}

export interface ControllerIssueInput {
  readonly secret: string;
  readonly issue: string;
}

export interface BacklogInput extends ControllerIssueInput {
  readonly marker: string;
}

export interface ProcessExitInput {
  readonly tree: string;
  readonly generation: number;
}

export interface GrantInput {
  readonly tree: string;
  readonly issue: string;
  readonly sessionId: string;
  readonly secret: string;
}

export interface LegionDaemonClient {
  readonly processStarted: (
    input: ProcessStartedInput
  ) => Promise<{ roleTokens: Record<string, string>; controlSubject: string; secret: string }>;
  readonly issueCreate: (input: IssueCreateInput) => Promise<{ issue: string; url: string }>;
  readonly waveRelease: (input: WaveReleaseInput) => Promise<{ released: string[] }>;
  readonly comment: (input: IssueTextInput) => Promise<{ commentId: number; url: string }>;
  readonly postBody: (input: IssueTextInput) => Promise<void>;
  readonly labels: (input: LabelsInput) => Promise<{ labels: string[] }>;
  readonly escalate: (input: EscalateInput) => Promise<void>;
  readonly issueClose: (input: IssueCloseInput) => Promise<void>;
  readonly dispatchThread: (
    input: DispatchThreadInput
  ) => Promise<{ thread: number; url: string }>;
  readonly spawnToken: (input: SpawnTokenInput) => Promise<{ spawnToken: string }>;
  readonly roleBacking: (input: RoleBackingInput) => Promise<void>;
  readonly phase: (
    input: PhaseInput
  ) => Promise<{ secret: string; gitName: string; gitEmail: string }>;
  readonly gatesApprove: (input: ControllerIssueInput) => Promise<void>;
  readonly admission: (input: ControllerIssueInput) => Promise<{ result: "spawned" | "queued" }>;
  readonly backlog: (input: BacklogInput) => Promise<void>;
  readonly processExit: (input: ProcessExitInput) => Promise<void>;
  readonly grant: (input: GrantInput) => Promise<{ grantId: string; expiresAt: string }>;
}

export class LegionDaemonApiError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly responseBody: string
  ) {
    super(`${method} ${path} failed with ${status}: ${responseBody}`);
  }
}

export function createLegionDaemonClient(
  baseUrl: string,
  fetchFn: typeof fetch = fetch
): LegionDaemonClient {
  const endpoint = baseUrl.replace(/\/+$/, "");

  const post = async <T>(path: string, body: object, schema: z.ZodType<T>): Promise<T> => {
    const response = await fetchFn(`${endpoint}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const responseBody = await response.text();
    if (!response.ok) throw new LegionDaemonApiError("POST", path, response.status, responseBody);
    return schema.parse(JSON.parse(responseBody));
  };

  const noContent = async (path: string, body: object): Promise<void> => {
    await post(path, body, EmptyResponseSchema);
  };

  return {
    processStarted: (input) =>
      post("/legion/v1/process/started", input, ProcessStartedResponseSchema),
    issueCreate: (input) => post("/legion/v1/issues", input, IssueCreateResponseSchema),
    waveRelease: (input) => post("/legion/v1/waves/release", input, WaveReleaseResponseSchema),
    comment: (input) => post("/legion/v1/issues/comment", input, CommentResponseSchema),
    postBody: (input) => noContent("/legion/v1/issues/body", input),
    labels: (input) => post("/legion/v1/issues/labels", input, LabelsResponseSchema),
    escalate: (input) => noContent("/legion/v1/escalate", input),
    dispatchThread: (input) =>
      post("/legion/v1/dispatch-threads", input, DispatchThreadResponseSchema),
    spawnToken: (input) => post("/legion/v1/spawn-token", input, SpawnTokenResponseSchema),
    roleBacking: (input) => noContent("/legion/v1/role-backing", input),
    issueClose: (input) => noContent("/legion/v1/issues/close", input),
    phase: (input) => post("/legion/v1/phase", input, PhaseResponseSchema),
    gatesApprove: (input) => noContent("/legion/v1/gates/approve", input),
    admission: (input) => post("/legion/v1/admission", input, AdmissionResponseSchema),
    backlog: (input) => noContent("/legion/v1/backlog", input),
    processExit: (input) => noContent("/legion/v1/process/exit", input),
    grant: (input) => post("/legion/v1/grants", input, GrantResponseSchema),
  };
}
