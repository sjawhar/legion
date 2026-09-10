import { z } from "zod";
import { LEGION_ROLES } from "./legion-roles";

const nonEmptyString = z.string().min(1);
const legionRole = z.enum(LEGION_ROLES);
const requiredUnknown = z.unknown().refine((value) => value !== undefined, {
  message: "Required",
});

const LIFECYCLE_STATUSES = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;

const architectCapability = z.strictObject({
  tree: nonEmptyString,
  sessionId: nonEmptyString,
  secret: nonEmptyString,
});
const controllerIssue = z.strictObject({
  secret: nonEmptyString,
  issue: nonEmptyString,
});

export const LegionDaemonApi = {
  State: {
    response: z.object({ project: nonEmptyString }),
  },
  ControllerReady: {
    request: z.strictObject({ secret: nonEmptyString, sessionId: nonEmptyString }),
    response: z.object({}),
  },
  ProcessStarted: {
    request: z.strictObject({
      tree: nonEmptyString,
      generation: z.number().int(),
      rootSessionId: nonEmptyString,
      agentId: nonEmptyString,
      bootToken: nonEmptyString,
      ompSessionFile: nonEmptyString,
    }),
    response: z.object({
      roleTokens: z.record(z.string(), z.string()),
      controlSubject: nonEmptyString,
      // Sent by the daemon on every start; optional so a client may ignore gate policy.
      gates: z
        .object({
          design: z.enum(["root-issues", "off"]),
          merge: z.enum(["human", "off"]),
        })
        .optional(),
      secret: nonEmptyString,
    }),
  },
  ProcessReady: {
    request: architectCapability,
    response: z.object({}),
  },
  MergeGate: {
    request: architectCapability.extend({ pr: z.number().int().positive() }),
    response: z.object({
      approved: z.boolean(),
      pr: z.number().int().positive(),
      headSha: nonEmptyString,
    }),
  },
  ProcessExit: {
    request: architectCapability.extend({ generation: z.number().int() }),
    response: z.object({}),
  },
  WaveRelease: {
    request: architectCapability.extend({ children: z.array(nonEmptyString).optional() }),
    response: z.object({ released: z.array(nonEmptyString) }),
  },
  Escalate: {
    request: architectCapability.extend({
      kind: z.enum(["re-file", "capacity", "cross-tree"]),
      context: requiredUnknown,
    }),
    response: z.object({}),
  },
  ProvisioningCredential: {
    request: architectCapability.extend({ issue: nonEmptyString }),
    response: z.object({ token: nonEmptyString }),
  },
  WorkerStarted: {
    request: z.strictObject({
      tree: nonEmptyString,
      issue: nonEmptyString,
      role: legionRole,
      bootToken: nonEmptyString,
      sessionId: nonEmptyString,
      agentId: nonEmptyString,
      ompSessionFile: nonEmptyString,
    }),
    response: z.object({
      roleToken: nonEmptyString,
      secret: nonEmptyString,
      gitName: nonEmptyString,
      gitEmail: nonEmptyString,
    }),
  },
  WorkerReady: {
    request: z.strictObject({
      tree: nonEmptyString,
      issue: nonEmptyString,
      role: legionRole,
      sessionId: nonEmptyString,
      generation: z.number().int().nonnegative(),
      secret: nonEmptyString,
    }),
    response: z.object({}),
  },
  PhaseComplete: {
    request: z.strictObject({
      grantId: nonEmptyString,
      summary: nonEmptyString,
    }),
    response: z.object({}),
  },
  SpawnWorker: {
    request: architectCapability.extend({
      issue: nonEmptyString,
      role: legionRole,
      task: nonEmptyString,
    }),
    response: z.object({
      status: z.enum(["spawned", "resumed", "queued"]),
      roleToken: nonEmptyString,
    }),
  },
  WorkerSession: {
    request: z.strictObject({
      sessionId: nonEmptyString,
      recoveryToken: nonEmptyString,
    }),
    response: z.object({
      tree: nonEmptyString,
      issue: nonEmptyString,
      role: legionRole,
      secret: nonEmptyString,
    }),
  },
  // Controller capability (bare `secret`) may set `todo`/`backlog`/`icebox` on any issue in the
  // project; architect capability (`tree`+`sessionId` alongside `secret`) may set any status on
  // an issue within its own tree. The route handler rejects a body presenting both or neither —
  // this schema only shapes the fields, `tree`/`sessionId` optional together.
  IssueStatus: {
    request: controllerIssue.extend({
      status: z.enum(LIFECYCLE_STATUSES),
      tree: nonEmptyString.optional(),
      sessionId: nonEmptyString.optional(),
    }),
    response: z.object({}),
  },
  GatesRegister: {
    request: architectCapability.extend({ issue: nonEmptyString, askId: nonEmptyString }),
    response: z.object({}),
  },
  Grant: {
    request: z.strictObject({
      tree: nonEmptyString,
      issue: nonEmptyString,
      sessionId: nonEmptyString,
      secret: nonEmptyString,
    }),
    response: z.object({ grantId: nonEmptyString, expiresAt: nonEmptyString }),
  },
  GitHubToken: {
    request: z.strictObject({ grantId: nonEmptyString }),
    response: z.object({ token: nonEmptyString, appLogin: z.string().endsWith("[bot]") }),
  },
} as const;

type InputOf<T extends z.ZodType> = z.input<T>;
type OutputOf<T extends z.ZodType> = z.output<T>;

export type DaemonStateResponse = OutputOf<typeof LegionDaemonApi.State.response>;
export type ControllerReadyInput = InputOf<typeof LegionDaemonApi.ControllerReady.request>;
export type ArchitectCapabilityInput = InputOf<typeof LegionDaemonApi.ProcessReady.request>;
export type ProcessStartedInput = InputOf<typeof LegionDaemonApi.ProcessStarted.request>;
export type ProcessStartedResponse = OutputOf<typeof LegionDaemonApi.ProcessStarted.response>;
export type ProcessReadyInput = InputOf<typeof LegionDaemonApi.ProcessReady.request>;
export type MergeGateInput = InputOf<typeof LegionDaemonApi.MergeGate.request>;
export type MergeGateResponse = OutputOf<typeof LegionDaemonApi.MergeGate.response>;
export type ProcessExitInput = InputOf<typeof LegionDaemonApi.ProcessExit.request>;
export type WaveReleaseInput = InputOf<typeof LegionDaemonApi.WaveRelease.request>;
export type WaveReleaseResponse = OutputOf<typeof LegionDaemonApi.WaveRelease.response>;
export type EscalateInput = InputOf<typeof LegionDaemonApi.Escalate.request>;
export type WorkerStartedInput = InputOf<typeof LegionDaemonApi.WorkerStarted.request>;
export type WorkerStartedResponse = OutputOf<typeof LegionDaemonApi.WorkerStarted.response>;
export type WorkerReadyInput = InputOf<typeof LegionDaemonApi.WorkerReady.request>;
export type PhaseCompleteInput = InputOf<typeof LegionDaemonApi.PhaseComplete.request>;
export type SpawnWorkerInput = InputOf<typeof LegionDaemonApi.SpawnWorker.request>;
export type SpawnWorkerResponse = OutputOf<typeof LegionDaemonApi.SpawnWorker.response>;
export type WorkerSessionInput = InputOf<typeof LegionDaemonApi.WorkerSession.request>;
export type WorkerSessionResponse = OutputOf<typeof LegionDaemonApi.WorkerSession.response>;
export type IssueStatusInput = InputOf<typeof LegionDaemonApi.IssueStatus.request>;
export type GatesRegisterInput = InputOf<typeof LegionDaemonApi.GatesRegister.request>;
export type GrantInput = InputOf<typeof LegionDaemonApi.Grant.request>;
export type GrantResponse = OutputOf<typeof LegionDaemonApi.Grant.response>;
export type GitHubTokenInput = InputOf<typeof LegionDaemonApi.GitHubToken.request>;
export type GitHubTokenResponse = OutputOf<typeof LegionDaemonApi.GitHubToken.response>;
export type ProvisioningCredentialInput = InputOf<
  typeof LegionDaemonApi.ProvisioningCredential.request
>;
export type ProvisioningCredentialResponse = OutputOf<
  typeof LegionDaemonApi.ProvisioningCredential.response
>;
