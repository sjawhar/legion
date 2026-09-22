import { z } from "zod";
import { LEGION_ROLES } from "./legion-roles";

/**
 * The Go daemon's HTTP API, as the plugin reads it.
 *
 * Go owns this wire shape: `packages/daemon-go/internal/api/state.go` is the source of truth and
 * every schema here mirrors it field for field. The two are pinned to each other by
 * `packages/contracts/fixtures/daemon-api/*.json`, written by the Go golden test
 * (`go test ./internal/api/ -update`) and parsed here by `legion-go-api.test.ts` — no generator
 * runs in either direction, so a Go field added without its line below fails that test.
 *
 * Every export is prefixed `LegionGo`/`GoDaemon`: `legion-daemon-api.ts` is the *TypeScript*
 * daemon's contract until Stage 7 deletes it with `packages/daemon`, both files are re-exported
 * by `index.ts`, and two `export *` modules exporting one name is a `TS2308` at the barrel.
 */

const nonEmptyString = z.string().min(1);
/** Go emits RFC 3339 through `time.Time`; a daemon on a non-UTC clock emits an offset. */
const timestamp = z.iso.datetime({ offset: true });

/** `api.Phase` — an admitted issue is in exactly one of these. */
export const LEGION_GO_PHASES = [
  "planner",
  "implementer",
  "tester",
  "reviewer",
  "retro",
  "merger",
  "production_check",
  "held",
  "done",
] as const;

export type LegionGoPhase = (typeof LEGION_GO_PHASES)[number];

/**
 * `api.ClaimView.Locator` — the runtime's own flat discriminated shape, defined by the tmux
 * runtime at Stage 2 and the sandbox runtime at Stage 4 and opaque to the daemon. Loose on
 * purpose: only the discriminant and the process incarnation are the daemon's to pin.
 */
const legionGoLocator = z.looseObject({
  runtime: nonEmptyString,
  incarnation: nonEmptyString,
});

/** `api.ClaimView` — `session` is empty until the claim's process registers one. */
const legionGoClaimView = z.strictObject({
  session: z.string(),
  state: nonEmptyString,
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

/** `api.Issue` — `workers` is keyed by role and partial: a phase that has not run has no entry
 * (Zod's plain `record` over an enum demands every key). */
const legionGoIssue = z.strictObject({
  key: nonEmptyString,
  generation: z.number().int().nonnegative(),
  phase: z.enum(LEGION_GO_PHASES),
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
});

export type LegionGoState = z.output<typeof LegionGoStateResponse>;
export type LegionGoIssue = LegionGoState["issues"][string];
