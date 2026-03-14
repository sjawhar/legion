import type { HandoffMessage, HandoffPhase, PhaseHandoff } from "./types";

export const HANDOFF_SCHEMA_VERSION = 1 as const;
export const LEGION_DIR_NAME = ".legion";
export const MESSAGES_DIR_NAME = "messages";

export const HANDOFF_PHASES: readonly HandoffPhase[] = [
  "architect",
  "plan",
  "implement",
  "test",
  "review",
  "retro",
];

export const PHASE_FILE_NAMES: Record<HandoffPhase, string> = {
  architect: "architect.json",
  plan: "plan.json",
  implement: "implement.json",
  test: "test.json",
  review: "review.json",
  retro: "retro.json",
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isIsoTimestamp(value: unknown): value is string {
  if (typeof value !== "string") {
    return false;
  }
  return !Number.isNaN(new Date(value).getTime());
}

export function isHandoffPhase(value: unknown): value is HandoffPhase {
  return typeof value === "string" && HANDOFF_PHASES.includes(value as HandoffPhase);
}

export function validatePhaseHandoff(value: unknown): PhaseHandoff | null {
  if (!isRecord(value)) {
    return null;
  }

  // TODO: When bumping to v2, add a migration path for v1 data instead of rejecting it.
  // Currently all v1 data becomes invisible on a version bump.
  if (value.schemaVersion !== HANDOFF_SCHEMA_VERSION) {
    return null;
  }

  if (!isHandoffPhase(value.phase)) {
    return null;
  }

  if (!isIsoTimestamp(value.completed)) {
    return null;
  }

  return value as unknown as PhaseHandoff;
}

export function validateHandoffMessage(value: unknown): HandoffMessage | null {
  if (!isRecord(value)) {
    return null;
  }

  if (!isHandoffPhase(value.from) || !isHandoffPhase(value.to)) {
    return null;
  }

  if (typeof value.body !== "string") {
    return null;
  }

  if (!isIsoTimestamp(value.timestamp)) {
    return null;
  }

  return value as unknown as HandoffMessage;
}
