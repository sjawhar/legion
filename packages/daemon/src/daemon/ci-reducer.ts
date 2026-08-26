import type { PrState } from "./legion-state";

export type CiEmission =
  | { type: "ci-first-red"; check: string; sha: string }
  | { type: "ci-settled-red"; failing: string[]; sha: string }
  | { type: "ci-green"; sha: string };

export interface CheckObservation {
  sha: string;
  name: string;
  status: string;
  conclusion: string | null;
}

export function reduceCheck(pr: PrState, observation: CheckObservation, now: number): void {
  if (observation.sha !== pr.headSha) return;

  pr.checks[observation.name] = {
    status: observation.status,
    conclusion: observation.conclusion,
  };
  pr.lastEventAt = now;
}

export function settle(
  pr: PrState,
  now: number,
  quietMs: number,
  mode: "eager"
): Array<Extract<CiEmission, { type: "ci-first-red" }>>;
export function settle(pr: PrState, now: number, quietMs: number, mode?: "settled"): CiEmission[];
export function settle(
  pr: PrState,
  now: number,
  quietMs: number,
  mode: "eager" | "settled" = "settled"
): CiEmission[] {
  const failing = failingChecks(pr);

  if (failing.length > 0 && !pr.firstRedEmitted) {
    pr.firstRedEmitted = true;
    return [{ type: "ci-first-red", check: failing[0], sha: pr.headSha }];
  }
  if (mode === "eager") return [];

  if (now - pr.lastEventAt < quietMs) return [];

  if (failing.length > 0 && allChecksCompleted(pr) && !pr.settledRedEmitted) {
    pr.settledRedEmitted = true;
    return [{ type: "ci-settled-red", failing, sha: pr.headSha }];
  }

  if (allChecksCompletedWithoutFailure(pr) && !pr.greenEmitted) {
    pr.greenEmitted = true;
    return [{ type: "ci-green", sha: pr.headSha }];
  }

  return [];
}

function failingChecks(pr: PrState): string[] {
  return Object.entries(pr.checks)
    .filter(([, check]) => isFailingCheck(check))
    .map(([name]) => name)
    .sort();
}

function isFailingCheck(check: { status: string; conclusion: string | null }): boolean {
  return (
    check.status === "completed" &&
    check.conclusion !== "success" &&
    check.conclusion !== "neutral" &&
    check.conclusion !== "skipped"
  );
}

function allChecksCompleted(pr: PrState): boolean {
  const checks = Object.values(pr.checks);
  return checks.length > 0 && checks.every((check) => check.status === "completed");
}

function allChecksCompletedWithoutFailure(pr: PrState): boolean {
  const checks = Object.values(pr.checks);
  return (
    checks.length > 0 &&
    checks.every((check) => check.status === "completed" && !isFailingCheck(check))
  );
}
