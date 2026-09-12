import { z } from "zod";
import { LEGION_ROLES } from "./legion-roles";

/**
 * The version of the daemon HTTP API contract below, as spoken by the installed
 * `@sjawhar/pi-legion-envoy` plugin: the plugin's `package.json` carries the same number under
 * `legion.daemonApiVersion`, and the daemon refuses to start unless the installed plugin's number
 * equals this one (`verifyLegionPluginContract`, packages/daemon/src/daemon/index.ts). A plugin
 * built before a shape change validates every daemon response against the older strict schemas
 * and fails the controller/architect boot handshake silently, so the two sides are kept in
 * lockstep the way `negotiate_protocol` keeps the worker RPC in lockstep. Bump rule: any change
 * to a `LegionDaemonApi` request or response shape bumps this constant AND the plugin manifest.
 */
export const LEGION_DAEMON_API_VERSION = 2;

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
// accidentally-forwarded field (a `*Hash`/`*Secret`/`*Token`/grant, or a worker's raw
// `socketPath`) fails `validateContractResponse`'s parse instead of silently reaching the wire —
// the controller and any other reader of this endpoint get only what they need to triage, never
// a capability. A locator is discriminated by the `runtime` that owns the process (the daemon's
// `Locator` union): a tmux window/pane, or a Kubernetes pod. The controller's own locator has no
// socket at all: it is an interactive OMP pane.
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
    // (every locator here is one of the `stateLocator`/`stateTreeLocator` shapes above; the
    // controller locator is a `stateTreeLocator` because its OMP session file is what
    // `ensureController` resumes).
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
      controllerLocator: stateTreeLocator.optional(),
      roles: z.record(z.string(), stateRole),
      controllerPendingNotices: z.number().int().nonnegative(),
      pendingStatusWrites: z.array(nonEmptyString),
    }),
  },
  ControllerReady: {
    request: z.strictObject({
      secret: nonEmptyString,
      sessionId: nonEmptyString,
      ompSessionFile: nonEmptyString.optional(),
    }),
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
  // Both `tree` and `issue` select a session-capability grant for a phase worker or root
  // architect; neither selects the controller-capability grant (`secret` is then the controller
  // secret) and mints `role: "controller"`. Exactly one of `tree` or `issue` is invalid (the
  // handler 400s).
  Grant: {
    request: z.strictObject({
      sessionId: nonEmptyString,
      secret: nonEmptyString,
      tree: nonEmptyString.optional(),
      issue: nonEmptyString.optional(),
    }),
    response: z.object({ grantId: nonEmptyString, expiresAt: nonEmptyString }),
  },
  // `merge: true` declares merge intent (`legion gh -- pr merge`); the daemon honours it only for
  // a controller grant and answers 403 for every phase-worker grant. Bound to both `/gh-token`
  // and `/git-credential`.
  GitHubToken: {
    request: z.strictObject({ grantId: nonEmptyString, merge: z.literal(true).optional() }),
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
