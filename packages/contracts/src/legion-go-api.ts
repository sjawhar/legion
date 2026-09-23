import { z } from "zod";
import { LEGION_ROLES } from "./legion-roles";

/**
 * The Go daemon's HTTP API, as its readers see it.
 *
 * Go owns this wire shape: `packages/daemon-go/internal/api` (`state.go`, `operator.go`) and the
 * claim wire in `packages/daemon-go/internal/claim/wire.go` are the source of truth, and every
 * schema here mirrors them field for field. The two are pinned to each other by
 * `packages/contracts/fixtures/daemon-api/*.json`, written by the Go golden tests
 * (`go test ./internal/api/ -update`) and parsed here by `legion-go-api.test.ts` — no generator
 * runs in either direction, so a Go field added without its line below fails that test.
 *
 * Reached as `@legion/contracts/legion-go-api`, never from the barrel. Every export is prefixed
 * `LegionGo`/`GoDaemon`, so nothing here collides with `legion-daemon-api.ts` — the *TypeScript*
 * daemon's contract until Stage 7 deletes it with `packages/daemon` — in a module that imports both.
 */

const nonEmptyString = z.string().min(1);
/** Go emits RFC 3339 through `time.Time`; a daemon on a non-UTC clock emits an offset. */
const timestamp = z.iso.datetime({ offset: true });

/** `api.Phase` — the state an admitted issue sits in, in the transition table's own order. Not
 * the role working it: `workers` is keyed by role (`LEGION_ROLES`). */
export const LEGION_GO_PHASES = [
  "admitted",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "retro",
  "merging",
  "awaiting_merge",
  "production_check",
  "done",
  "held",
] as const;

export type LegionGoPhase = (typeof LEGION_GO_PHASES)[number];

/** `supervise.ClaimState` — where a claim is in its life, in the order a launch reaches them. */
export const LEGION_GO_CLAIM_STATES = [
  "queued",
  "launch_uncertain",
  "launching",
  "shim_connected",
  "registered",
  "ready",
  "working",
  "idle",
  "suspended",
  "failed",
  "retired",
] as const;

export type LegionGoClaimState = (typeof LEGION_GO_CLAIM_STATES)[number];

/**
 * `runtime.Locator` — where a claim's process is: the runtime word, the claim token, the process
 * incarnation, and exactly one backend member, the one the runtime word names. Nested, marshalled
 * by the standard library: `tmux` (the pane on the daemon's private server; the incarnation is
 * `<pane pid>:<start ticks>`) or `sandbox` (the Agent Sandbox object; the incarnation is its pod's
 * uid).
 */
const legionGoLocator = z.discriminatedUnion("runtime", [
  z.strictObject({
    runtime: z.literal("tmux"),
    claim: nonEmptyString,
    incarnation: nonEmptyString,
    tmux: z.strictObject({ window: nonEmptyString, pane: nonEmptyString }),
  }),
  z.strictObject({
    runtime: z.literal("sandbox"),
    claim: nonEmptyString,
    incarnation: nonEmptyString,
    sandbox: z.strictObject({ namespace: nonEmptyString, name: nonEmptyString }),
  }),
]);

/** `api.ClaimView` — `session` is empty until the claim's agent registers one, and `locator` is
 * absent while no process runs (queued, suspended, failed, retired). */
const legionGoClaimView = z.strictObject({
  session: z.string(),
  state: z.enum(LEGION_GO_CLAIM_STATES),
  locator: legionGoLocator.optional(),
});

/** `api.PhaseView` — one phase worker's claim, its committed handoff, and the rounds it has run. */
const legionGoPhaseView = z.strictObject({
  claim: legionGoClaimView,
  handoffCommit: nonEmptyString.optional(),
  rounds: z.number().int().nonnegative(),
});

/** `api.PullRequestView` — the pull request as the daemon observes it from GitHub. */
const legionGoPullRequestView = z.strictObject({
  number: z.number().int().positive(),
  head: nonEmptyString,
  checksVerdict: nonEmptyString.optional(),
  reviewDecision: nonEmptyString.optional(),
  fixAttempts: z.number().int().nonnegative(),
});

/** `api.GateView` — the design gate; `approvedVersion` is null until a human approves one, and
 * the gate is open exactly when it equals `currentVersion`. */
const legionGoGateView = z.strictObject({
  artifactId: nonEmptyString,
  currentVersion: z.number().int().positive(),
  approvedVersion: z.number().int().positive().nullable(),
});

/** `api.SlotView` — the admission slot the issue occupies and when it took it. */
const legionGoSlotView = z.strictObject({
  index: z.number().int().nonnegative(),
  admittedAt: timestamp,
});

/** `api.PendingStatusWrite` — one due `dispatch_status` effect the outbox has not finished. The
 * workflow owns the payload's detailed shape, so the state surface preserves it as JSON. */
const legionGoPendingStatusWrite = z.strictObject({
  issue: nonEmptyString,
  payload: z.record(z.string(), z.unknown()),
  attempts: z.number().int().nonnegative(),
  nextAt: timestamp,
  lastError: z.string().optional(),
});

/** `api.Issue` — `workers` is keyed by role and partial: a phase that has not run has no entry
 * (Zod's plain `record` over an enum demands every key). */
const legionGoIssue = z.strictObject({
  key: nonEmptyString,
  generation: z.number().int().nonnegative(),
  phase: z.enum(LEGION_GO_PHASES),
  status: nonEmptyString,
  architect: legionGoClaimView.optional(),
  workers: z.partialRecord(z.enum(LEGION_ROLES), legionGoPhaseView),
  pullRequest: legionGoPullRequestView.optional(),
  designGate: legionGoGateView.optional(),
  slot: legionGoSlotView.optional(),
});

/** `api.DaemonInfo` — the running daemon: its project, its store's schema, and its boot history. */
const goDaemonInfo = z.strictObject({
  project: nonEmptyString,
  schemaVersion: z.number().int().nonnegative(),
  boots: z.number().int().nonnegative(),
  firstBootAt: timestamp,
  startedAt: timestamp,
});

/** `api.Admission` — the issue cap and the issues under it, in Dispatch rank order. Concurrency
 * is capped on issues, never on workers: there is no worker queue on this wire. */
const legionGoAdmission = z.strictObject({
  cap: z.number().int().nonnegative(),
  active: z.array(nonEmptyString),
  waiting: z.array(nonEmptyString),
});

/** `api.State`, the body of `GET /legion/v1/state`. */
export const LegionGoStateResponse = z.strictObject({
  daemon: goDaemonInfo,
  admission: legionGoAdmission,
  issues: z.record(z.string(), legionGoIssue),
  pendingStatusWrites: z.array(legionGoPendingStatusWrite),
});

export type LegionGoState = z.output<typeof LegionGoStateResponse>;
export type LegionGoIssue = LegionGoState["issues"][string];

/** `claim.RegisterResponse`, the body of `POST /legion/v1/claims/register`: the claim the agent
 * holds, and the secret its ready and exit authenticate with. */
export const LegionGoRegisterResponse = z.strictObject({
  claimToken: nonEmptyString,
  tree: nonEmptyString,
  issue: nonEmptyString,
  role: z.enum(LEGION_ROLES),
  generation: z.number().int().positive(),
  secret: nonEmptyString,
});

export type LegionGoRegistration = z.output<typeof LegionGoRegisterResponse>;

/** Claim routes refuse with only a sentence; credential and workflow routes add a stable code. */
export const LegionGoErrorResponse = z.union([
  z.strictObject({ error: nonEmptyString }),
  z.strictObject({ code: nonEmptyString, error: nonEmptyString }),
]);

/** `api.DeliveryView` — the claim's pending task; `deliveredAt` is the latest send's
 * acknowledgement and `confirmedAt` the turn it started, each absent until it happens. */
const legionGoDeliveryView = z.strictObject({
  id: nonEmptyString,
  task: nonEmptyString,
  queuedAt: timestamp,
  deliveredAt: timestamp.optional(),
  confirmedAt: timestamp.optional(),
});

/** `api.OperatorClaim`, the body of the operator routes that act on one claim (`POST
 * /legion/v1/operator/claims`, `…/{token}/deliver|suspend|resume|stop`): the claim as its
 * supervisor holds it, but its hashes. */
export const LegionGoOperatorClaimResponse = z.strictObject({
  token: nonEmptyString,
  tree: nonEmptyString,
  issue: nonEmptyString,
  role: z.enum(LEGION_ROLES),
  generation: z.number().int().nonnegative(),
  state: z.enum(LEGION_GO_CLAIM_STATES),
  session: z.string(),
  sessionFile: z.string(),
  locator: legionGoLocator.optional(),
  budgets: z.strictObject({
    launchFailures: z.number().int().nonnegative(),
    promptFailures: z.number().int().nonnegative(),
    promptRetires: z.number().int().nonnegative(),
  }),
  uncertainStreak: z.number().int().nonnegative(),
  pending: legionGoDeliveryView.optional(),
});

export type LegionGoOperatorClaim = z.output<typeof LegionGoOperatorClaimResponse>;

/** `api.OperatorClaims`, the body of `GET /legion/v1/operator/claims`, in token order. */
export const LegionGoOperatorClaimsResponse = z.strictObject({
  claims: z.array(LegionGoOperatorClaimResponse),
});

/** Credential grant, GitHub token, and git-helper bodies from `internal/api/credentials.go`. */
export const LegionGoGrantResponse = z.strictObject({
  grantId: nonEmptyString,
  expiresAt: timestamp,
});
export const LegionGoGitHubTokenResponse = z.strictObject({
  token: nonEmptyString,
  appLogin: z.string().endsWith("[bot]"),
});
export const LegionGoGitCredentialResponse = z.strictObject({
  username: z.literal("x-access-token"),
  password: nonEmptyString,
});

/** Every completed fact route returns an intentional empty JSON object, never an unconstrained body. */
export const LegionGoEmptyResponse = z.strictObject({});
export const LegionGoWaveReleaseResponse = z.strictObject({
  released: z.array(nonEmptyString),
});

/** `api.GrantRequest`, the session form that mints one short-lived, single-use credential grant. */
export const LegionGoGrantRequest = z.strictObject({
  sessionId: nonEmptyString,
  secret: nonEmptyString,
  tree: nonEmptyString,
  issue: nonEmptyString,
});

/** `api.GrantCredentialRequest`, shared by the three grant-redemption routes. */
export const LegionGoGrantCredentialRequest = z.strictObject({
  grantId: nonEmptyString,
});

/** `api.HandoffCompleteRequest`, the observation one worker reports to the workflow. */
export const LegionGoHandoffCompleteRequest = z.strictObject({
  grantId: nonEmptyString,
  summary: nonEmptyString,
  verdict: z.string(),
  ready: z.boolean(),
  commit: nonEmptyString,
});

/** `api.IssueStatusRequest`, the controller's explicit board-status write. */
export const LegionGoIssueStatusRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  status: z.enum(["todo", "backlog", "icebox"]),
});

/** `api.GateRegisterRequest`, the architect's current Dispatch document approval target. */
export const LegionGoGateRegisterRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  artifactId: z.uuid(),
  version: z.number().int().positive(),
});

/** `api.WaveReleaseRequest`, the architect's selected child issues. */
export const LegionGoWaveReleaseRequest = z.strictObject({
  grantId: nonEmptyString,
  issues: z.array(nonEmptyString),
});

/** `api.PhaseBackwardRequest`, the active worker's request to return to an earlier phase. */
export const LegionGoPhaseBackwardRequest = z.strictObject({
  grantId: nonEmptyString,
  to: z.enum(LEGION_GO_PHASES),
  reason: nonEmptyString,
});

/** `api.PhaseRetryRequest`, the tree architect's retry-or-escalate decision for a held issue. */
export const LegionGoPhaseRetryRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  decision: z.enum(["retry", "escalate"]),
});

/** `api.SignOffRequest`, the owning architect's post-production-check sign-off. */
export const LegionGoSignOffRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
});
