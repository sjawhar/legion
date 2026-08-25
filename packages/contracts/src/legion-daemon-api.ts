import { z } from "zod";
import { LEGION_ROLES } from "./legion-roles";

const nonEmptyString = z.string().min(1);
const legionRole = z.enum(LEGION_ROLES);
const requiredUnknown = z.unknown().refine((value) => value !== undefined, {
  message: "Required",
});
const architectCapability = z.object({
  tree: nonEmptyString,
  sessionId: nonEmptyString,
  secret: nonEmptyString,
});
const controllerIssue = z.object({
  secret: nonEmptyString,
  issue: nonEmptyString,
});

export const LegionDaemonApi = {
  State: {
    response: z.object({ project: nonEmptyString }),
  },
  ControllerReady: {
    request: z.object({ secret: nonEmptyString, sessionId: nonEmptyString }),
    response: z.object({}),
  },
  ProcessStarted: {
    request: z.object({
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
      secret: nonEmptyString,
    }),
  },
  ProcessReady: {
    request: architectCapability,
    response: z.object({}),
  },
  ProcessExit: {
    request: architectCapability.extend({ generation: z.number().int() }),
    response: z.object({}),
  },
  IssueCreate: {
    request: architectCapability.extend({
      title: nonEmptyString,
      body: nonEmptyString,
      labels: z.array(nonEmptyString).optional(),
    }),
    response: z.object({ issue: nonEmptyString, url: nonEmptyString }),
  },
  WaveRelease: {
    request: architectCapability.extend({ children: z.array(nonEmptyString).optional() }),
    response: z.object({ released: z.array(nonEmptyString) }),
  },
  Comment: {
    request: architectCapability.extend({ issue: nonEmptyString, body: nonEmptyString }),
    response: z.object({ commentId: z.number().int(), url: nonEmptyString }),
  },
  PostBody: {
    request: architectCapability.extend({ issue: nonEmptyString, body: nonEmptyString }),
    response: z.object({}),
  },
  Labels: {
    request: architectCapability.extend({
      issue: nonEmptyString,
      add: z.array(nonEmptyString).optional(),
      remove: z.array(nonEmptyString).optional(),
    }),
    response: z.object({ labels: z.array(nonEmptyString) }),
  },
  Escalate: {
    request: architectCapability.extend({
      kind: z.enum(["re-file", "capacity", "cross-tree"]),
      context: requiredUnknown,
    }),
    response: z.object({}),
  },
  IssueClose: {
    request: architectCapability.extend({ issue: nonEmptyString, comment: z.string().optional() }),
    response: z.object({}),
  },
  DispatchThread: {
    request: z.object({
      tree: nonEmptyString,
      issue: nonEmptyString,
      role: legionRole,
      sessionId: nonEmptyString,
      secret: nonEmptyString,
      parent: nonEmptyString,
      subject: nonEmptyString,
      body: nonEmptyString,
      ask: z.unknown().optional(),
      urgency: z.enum(["low", "med", "high", "blocking"]).optional(),
    }),
    response: z.object({ thread: z.number().int(), url: nonEmptyString }),
  },
  SpawnToken: {
    request: architectCapability.extend({ issue: nonEmptyString, role: legionRole }),
    response: z.object({ spawnToken: nonEmptyString }),
  },
  ProvisioningCredential: {
    request: architectCapability.extend({ issue: nonEmptyString }),
    response: z.object({ token: nonEmptyString }),
  },
  RoleBacking: {
    request: z.object({
      tree: nonEmptyString,
      issue: nonEmptyString,
      role: legionRole,
      sessionId: nonEmptyString,
      agentId: nonEmptyString,
      spawnToken: nonEmptyString,
    }),
    response: z.object({}),
  },
  Phase: {
    request: z.object({
      tree: nonEmptyString,
      issue: nonEmptyString,
      phase: nonEmptyString,
      sessionId: nonEmptyString,
      spawnToken: nonEmptyString,
    }),
    response: z.object({
      secret: nonEmptyString,
      gitName: nonEmptyString,
      gitEmail: nonEmptyString,
    }),
  },
  WorkerSession: {
    request: z.object({
      sessionId: nonEmptyString,
      agentId: nonEmptyString,
    }),
    response: z.object({
      tree: nonEmptyString,
      issue: nonEmptyString,
      role: legionRole,
      secret: nonEmptyString,
    }),
  },
  GatesApprove: {
    request: controllerIssue,
    response: z.object({}),
  },
  Admission: {
    request: controllerIssue,
    response: z.object({ result: z.enum(["spawned", "queued"]) }),
  },
  Backlog: {
    request: controllerIssue.extend({ marker: nonEmptyString }),
    response: z.object({}),
  },
  Grant: {
    request: z.object({
      tree: nonEmptyString,
      issue: nonEmptyString,
      sessionId: nonEmptyString,
      secret: nonEmptyString,
    }),
    response: z.object({ grantId: nonEmptyString, expiresAt: nonEmptyString }),
  },
  GitHubToken: {
    request: z.object({ grantId: nonEmptyString }),
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
export type ProcessExitInput = InputOf<typeof LegionDaemonApi.ProcessExit.request>;
export type IssueCreateInput = InputOf<typeof LegionDaemonApi.IssueCreate.request>;
export type IssueCreateResponse = OutputOf<typeof LegionDaemonApi.IssueCreate.response>;
export type WaveReleaseInput = InputOf<typeof LegionDaemonApi.WaveRelease.request>;
export type WaveReleaseResponse = OutputOf<typeof LegionDaemonApi.WaveRelease.response>;
export type IssueTextInput = InputOf<typeof LegionDaemonApi.Comment.request>;
export type CommentResponse = OutputOf<typeof LegionDaemonApi.Comment.response>;
export type LabelsInput = InputOf<typeof LegionDaemonApi.Labels.request>;
export type LabelsResponse = OutputOf<typeof LegionDaemonApi.Labels.response>;
export type EscalateInput = InputOf<typeof LegionDaemonApi.Escalate.request>;
export type IssueCloseInput = InputOf<typeof LegionDaemonApi.IssueClose.request>;
export type DispatchThreadInput = InputOf<typeof LegionDaemonApi.DispatchThread.request>;
export type DispatchThreadResponse = OutputOf<typeof LegionDaemonApi.DispatchThread.response>;
export type SpawnTokenInput = InputOf<typeof LegionDaemonApi.SpawnToken.request>;
export type SpawnTokenResponse = OutputOf<typeof LegionDaemonApi.SpawnToken.response>;
export type RoleBackingInput = InputOf<typeof LegionDaemonApi.RoleBacking.request>;
export type PhaseInput = InputOf<typeof LegionDaemonApi.Phase.request>;
export type PhaseResponse = OutputOf<typeof LegionDaemonApi.Phase.response>;
export type WorkerSessionInput = InputOf<typeof LegionDaemonApi.WorkerSession.request>;
export type WorkerSessionResponse = OutputOf<typeof LegionDaemonApi.WorkerSession.response>;
export type ControllerIssueInput = InputOf<typeof LegionDaemonApi.GatesApprove.request>;
export type AdmissionResponse = OutputOf<typeof LegionDaemonApi.Admission.response>;
export type BacklogInput = InputOf<typeof LegionDaemonApi.Backlog.request>;
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
