import { z } from "zod";
import { ISSUE_STATUSES } from "./dispatch-tools";
import { LEGION_ROLES } from "./legion-roles";

/**
 * The version of the daemon/plugin contract, as spoken by the installed
 * `@sjawhar/pi-legion-envoy` plugin: the plugin's `package.json` carries the same number under
 * `legion.daemonApiVersion`, and the daemon refuses to start unless the installed plugin's number
 * equals this one (`verifyLegionPluginContract`, packages/daemon/src/daemon/boot-probes.ts). The
 * number covers two things. The HTTP API shapes below: a plugin built before a shape change
 * validates every daemon response against the older strict schemas and fails the
 * controller/architect boot handshake silently. And the pane contract — every environment
 * variable the daemon sets on a pane that the plugin reads or writes: `LEGION_GRANT_FILE`,
 * `LEGION_BOOT_TOKEN_FILE`, `LEGION_CONTROLLER_SECRET_FILE`, `LEGION_CONTROL_SUBJECT`,
 * `LEGION_DAEMON_URL`, `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`, `ENVOY_NATS_URL`, `ENVOY_URL`,
 * `ENVOY_TOKEN_FILE` (the listener bearer, when the daemon has one), and
 * the `LEGION_*` identity variables `LEGION_TREE`/`LEGION_ISSUE`/`LEGION_ROLE`/
 * `LEGION_GENERATION`/`LEGION_WORKSPACE`/`LEGION_STATE_DIR`/`LEGION_CONTROLLER` (set in
 * `processes.ts`'s three pane environments and the runtime's `<NAME>_FILE` pointer; read in
 * `extensions/legion.ts`, `src/legion/classify.ts`, and `@legion/envoy-client`). A plugin that
 * never writes the credential file the daemon names fails every worker the daemon spawns at its
 * first `legion gh`/`jj git push`, after the work is done, with the `legion` CLI's
 * `LEGION_GRANT_FILE names <path>, which could not be read: ENOENT …: the pi-envoy extension in
 * this pane did not write it — the installed plugin predates LEGION-54`. Both skews are kept in
 * lockstep the way `negotiate_protocol` keeps the worker RPC in lockstep. Bump rule: any change
 * to a `LegionDaemonApi` request or response shape, OR to the pane contract, bumps this constant
 * AND the plugin manifest's `legion.daemonApiVersion` in the same commit — and, like a
 * state-version bump, the number is re-read against `main` at every rebase: two branches that
 * each change a surface both take the next number, and the second to land renumbers above the
 * first.
 *
 * History: 1 — the `runtime` locator discriminant on `/legion/v1/state` (LEGION-21). 2 —
 * introduced by LEGION-20 (PR #975) for the `stateGate` and `GatesRegister` shapes and, from
 * LEGION-52, also covering the pane contract including the credential file (`LEGION_GRANT_FILE`,
 * LEGION-54); plugin release 1.23.0 is the first to declare 2. Releases 1.14.0 through 1.22.2
 * declare 1 and are refused as `speaks daemon API contract 1`; releases before 1.14.0 have no
 * field and are refused as `contract none`. 3 — LEGION-25: `ENVOY_TOKEN_FILE` on every pane and
 * pod when the daemon has an Envoy bearer (`envoy_token_file`); the plugin's bundled
 * `@legion/envoy-client` reads it ahead of `ENVOY_TOKEN`. A contract-2 plugin on a daemon with
 * `envoy_token_file` set would ignore the file and have every listener call answered 401. 4 —
 * `spawn_worker` carries a plugin-minted `requestId` and the state response carries
 * `workerAdmission`; accepted spawns survive a daemon restart (LEGION-102). 5 — LEGION-16 (PR
 * #961): the interactive controller's handshake — `controllerLocator.ompSessionFile` on
 * `/legion/v1/state`, `ompSessionFile` on `/controller/ready`, and the `/grants` request as a
 * union with its controller form. Every release built from `main` at contract 4 is refused as
 * `speaks daemon API contract 4`. 6 — LEGION-25 Part B: the operator-launched controller's
 * external record on `/legion/v1/state`'s `controllerLocator` (`{runtime:"kubernetes",
 * external:true, sessionId, registeredAt}`) and `POST /legion/v1/controller/secret`. A
 * contract-5 plugin's strict state parse fails on the external record the moment an operator's
 * controller registers. 7 — `/gh-token` no longer accepts merge intent: Legion never merges.
 */
export const LEGION_DAEMON_API_VERSION = 8;

const nonEmptyString = z.string().min(1);
const legionRole = z.enum(LEGION_ROLES);
const requiredUnknown = z.unknown().refine((value) => value !== undefined, {
  message: "Required",
});

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
// The operator-launched controller (LEGION-25 Part B): no pane or pod — the Envoy session that
// called `/controller/ready` and when (unix ms). `runtime` stays "kubernetes" because that is
// the runtime that recorded it; `external: true` is what tells it from a pod locator.
const stateExternalControllerLocator = z.strictObject({
  runtime: z.literal("kubernetes"),
  external: z.literal(true),
  sessionId: nonEmptyString,
  registeredAt: z.number().int().nonnegative(),
});
const stateIssue = z.strictObject({
  key: nonEmptyString,
  title: z.string(),
  status: z.enum(ISSUE_STATUSES).optional(),
  children: z.array(nonEmptyString),
  parent: nonEmptyString.optional(),
  lastAppliedSeq: z.number().int().nonnegative().optional(),
});
/** Recovery provenance is operational state, not a credential: an operator needs it to distinguish
 * a fresh session caused by a lost tree volume from an ordinary same-session resume. */
const stateWorkspaceLost = z.strictObject({
  at: nonEmptyString,
  generation: z.number().int().nonnegative(),
  fromRef: nonEmptyString,
  previousSessionId: nonEmptyString.optional(),
});
const stateTree = z.strictObject({
  status: z.enum(TREE_STATUSES),
  generation: z.number().int().nonnegative(),
  launchFailures: z.number().int().nonnegative(),
  readyConfirmedAt: z.number().optional(),
  locator: stateTreeLocator.optional(),
  workspaceLost: stateWorkspaceLost.optional(),
});
// The design gate as the daemon records it: the root spec document (`artifactId`) with the
// highest version the daemon has seen and, once a human approves, the version they approved.
// The gate is open exactly when `approvedVersion === latestVersion` (see `designGateOpen`).
const stateGate = z.strictObject({
  artifactId: nonEmptyString,
  latestVersion: z.number().int().positive(),
  approvedVersion: z.number().int().positive().optional(),
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
  workspaceLost: stateWorkspaceLost.optional(),
});
// One FIFO entry of the running-worker queue (`state.workerAdmission.queue`): a stale entry whose
// claim has lost its pending task has only its identity; a pending task carries both `kind` and
// `queuedAt`. Neither shape includes task text — it is long and already sits in the architect's
// transcript.
const stateQueuedWorkerIdentity = {
  roleToken: nonEmptyString,
  issue: nonEmptyString,
  role: nonEmptyString,
};
const stateQueuedWorker = z.union([
  z.strictObject(stateQueuedWorkerIdentity),
  z.strictObject({
    ...stateQueuedWorkerIdentity,
    kind: z.enum(["assignment", "catchup"]),
    queuedAt: nonEmptyString,
  }),
]);

export const LegionDaemonApi = {
  State: {
    // Redacted projection of durable `LegionState` for `GET /legion/v1/state` — never a
    // `*Hash`/`*Secret`/`*Token` field, a `spawnCapabilities`/grant record, or a `socketPath`
    // (every locator here is one of the `stateLocator`/`stateTreeLocator` shapes above; the
    // controller locator is a `stateTreeLocator` because its OMP session file is what
    // `ensureController` resumes — or the external record of an operator-launched controller;
    // a `discriminatedUnion` cannot hold two members with `runtime: "kubernetes"`, and a plain
    // `z.union` of two strict objects is unambiguous because each rejects the other's keys).
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
      controllerLocator: z.union([stateTreeLocator, stateExternalControllerLocator]).optional(),
      roles: z.record(z.string(), stateRole),
      controllerPendingNotices: z.number().int().nonnegative(),
      pendingStatusWrites: z.array(nonEmptyString),
      workerAdmission: z.strictObject({ queue: z.array(stateQueuedWorker) }),
    }),
  },
  ControllerReady: {
    request: z.strictObject({
      secret: nonEmptyString,
      sessionId: nonEmptyString,
      ompSessionFile: nonEmptyString.optional(),
      pluginVersion: nonEmptyString,
    }),
    response: z.object({}),
  },
  // `legion controller start` presents the operator token as `Authorization: Bearer` and gets a
  // fresh controller capability; the body is empty. Not a plugin call — the CLI's only.
  ControllerSecret: {
    request: z.strictObject({}),
    response: z.object({ secret: nonEmptyString }),
  },
  ProcessStarted: {
    request: z.strictObject({
      tree: nonEmptyString,
      generation: z.number().int(),
      rootSessionId: nonEmptyString,
      agentId: nonEmptyString,
      bootToken: nonEmptyString,
      ompSessionFile: nonEmptyString,
      pluginVersion: nonEmptyString,
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
      pluginVersion: nonEmptyString,
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
      // Minted once per `legion` `spawn_worker` tool call by the plugin; the daemon dedupes
      // repeats of it (`handleSpawnWorker`) so a transport retry can never queue the task twice.
      requestId: z.uuid(),
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
      status: z.enum(ISSUE_STATUSES),
      tree: nonEmptyString.optional(),
      sessionId: nonEmptyString.optional(),
    }),
    response: z.object({}),
  },
  GatesRegister: {
    // `artifactId` and `version` are the `artifact` and `version` values from
    // `dispatch_request_approval`'s result: the document a human must approve, at the version the
    // architect requested approval of. Dispatch artifact ids are UUIDs, and the approval events the
    // daemon matches carry that UUID as `artifact_id` — so a slug or file name (`spec`, `spec.md`)
    // is rejected here, naming the field, instead of registering a gate no event can ever open.
    request: architectCapability.extend({
      issue: nonEmptyString,
      artifactId: z.uuid(),
      version: z.number().int().positive(),
    }),
    response: z.object({}),
  },
  // Two forms: both `tree` and `issue` select a session-capability grant for a phase worker or
  // root architect; neither selects the controller-capability grant (`secret` is then the
  // controller secret) and mints `role: "controller"`. The union is the contract: a half form
  // (one of the two keys) is not a member and is rejected on both sides before any handler runs.
  Grant: {
    request: z.union([
      z.strictObject({
        sessionId: nonEmptyString,
        secret: nonEmptyString,
        tree: nonEmptyString,
        issue: nonEmptyString,
      }),
      z.strictObject({ sessionId: nonEmptyString, secret: nonEmptyString }),
    ]),
    response: z.object({ grantId: nonEmptyString, expiresAt: nonEmptyString }),
  },
  GitHubToken: {
    request: z.strictObject({ grantId: nonEmptyString }),
    response: z.object({ token: nonEmptyString, appLogin: z.string().endsWith("[bot]") }),
  },
  GitCredential: {
    request: z.strictObject({ grantId: nonEmptyString }),
  },
} as const;

type InputOf<T extends z.ZodType> = z.input<T>;
type OutputOf<T extends z.ZodType> = z.output<T>;

export type DaemonStateResponse = OutputOf<typeof LegionDaemonApi.State.response>;
export type ControllerReadyInput = InputOf<typeof LegionDaemonApi.ControllerReady.request>;
export type ControllerSecretResponse = OutputOf<typeof LegionDaemonApi.ControllerSecret.response>;
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
