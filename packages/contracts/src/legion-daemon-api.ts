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

const TREE_STATUSES = ["queued", "active", "lingering", "dead", "launch-failed", "closed"] as const;

// `/legion/v1/state`'s redaction contract: every schema below is a `strictObject` so an
// accidentally-forwarded field (a `*Hash`/`*Secret`/`*Token`/grant, or a raw `socketPath`) fails
// `validateContractResponse`'s parse instead of silently reaching the wire — the controller and
// any other reader of this endpoint get only what they need to triage, never a capability. A
// locator is discriminated by the `runtime` that owns the process (the daemon's `Locator` union):
// a tmux window/pane, or a Kubernetes pod.
const stateTmuxLocator = z.strictObject({
  runtime: z.literal("tmux"),
  tmuxSession: nonEmptyString,
  tmuxWindowId: nonEmptyString,
  tmuxPaneId: nonEmptyString.optional(),
});
const stateK8sLocator = z.strictObject({
  runtime: z.literal("kubernetes"),
  namespace: nonEmptyString,
  podName: nonEmptyString,
  podUid: nonEmptyString,
  pvcName: nonEmptyString,
});
const stateLocator = z.discriminatedUnion("runtime", [stateTmuxLocator, stateK8sLocator]);
const stateTreeLocator = z.discriminatedUnion("runtime", [
  stateTmuxLocator.extend({ ompSessionFile: nonEmptyString.optional() }),
  stateK8sLocator.extend({ ompSessionFile: nonEmptyString.optional() }),
]);
const stateIssue = z.strictObject({
  key: nonEmptyString,
  title: z.string(),
  status: z.enum(LIFECYCLE_STATUSES).optional(),
  children: z.array(nonEmptyString),
  parent: nonEmptyString.optional(),
  lastAppliedSeq: z.number().int().nonnegative().optional(),
});
const stateTree = z.strictObject({
  status: z.enum(TREE_STATUSES),
  generation: z.number().int().nonnegative(),
  launchFailures: z.number().int().nonnegative(),
  readyConfirmedAt: z.number().optional(),
  locator: stateTreeLocator.optional(),
});
const stateGate = z.strictObject({
  designAskId: nonEmptyString.optional(),
  designApproved: nonEmptyString.optional(),
});
// `role` matches `RoleClaim.role`'s own persisted type (a plain non-empty string, not the
// stricter `legionRole` enum request schemas use): a stored claim's role always belongs to
// `LEGION_ROLES` or is `"controller"`, but this projection reflects durable state as recorded
// rather than re-validating it against the daemon's current role list.
const stateRole = z.strictObject({
  role: nonEmptyString,
  issue: nonEmptyString.optional(),
  generation: z.number().int().nonnegative().optional(),
  sessionId: nonEmptyString.optional(),
  readyConfirmedAt: z.number().optional(),
  launchFailures: z.number().int().nonnegative().optional(),
  locator: stateLocator.optional(),
});

export const LegionDaemonApi = {
  State: {
    // Redacted projection of durable `LegionState` for `GET /legion/v1/state` — never a
    // `*Hash`/`*Secret`/`*Token` field, a `spawnCapabilities`/grant record, or a `socketPath`
    // (every locator here is one of the `stateLocator`/`stateTreeLocator` shapes above).
    response: z.strictObject({
      project: nonEmptyString,
      version: z.number().int(),
      issues: z.record(z.string(), stateIssue),
      trees: z.record(z.string(), stateTree),
      admission: z.strictObject({
        cap: z.number().int().nonnegative(),
        active: z.array(nonEmptyString),
        queue: z.array(nonEmptyString),
      }),
      gates: z.record(z.string(), stateGate),
      controllerLocator: stateLocator.optional(),
      roles: z.record(z.string(), stateRole),
      controllerPendingNotices: z.number().int().nonnegative(),
      pendingStatusWrites: z.array(nonEmptyString),
    }),
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
        })
        .optional(),
      secret: nonEmptyString,
    }),
  },
  ProcessReady: {
    request: architectCapability.extend({ generation: z.number().int() }),
    response: z.object({}),
  },
  ProcessExit: {
    request: architectCapability.extend({ generation: z.number().int() }),
    response: z.object({}),
  },
  WaveRelease: {
    request: architectCapability.extend({ issues: z.array(nonEmptyString).optional() }),
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
  // A bare controller credential sets `todo`/`backlog`/`icebox` project-wide. An architect
  // credential includes both `tree` and `sessionId` and may set any status within that tree.
  // Exactly one of `tree` or `sessionId` is invalid; neither selects controller access and both
  // select architect access.
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
export type ArchitectCapabilityInput = InputOf<typeof architectCapability>;
export type ProcessStartedInput = InputOf<typeof LegionDaemonApi.ProcessStarted.request>;
export type ProcessStartedResponse = OutputOf<typeof LegionDaemonApi.ProcessStarted.response>;
export type ProcessReadyInput = InputOf<typeof LegionDaemonApi.ProcessReady.request>;
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
