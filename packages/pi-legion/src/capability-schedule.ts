import { LEGION_ROLES } from "@legion/contracts";
import type { LegionCapabilityReportBody } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import { budgetLabel } from "./capability-report";

/**
 * INFRA-927: a capability check that loses the boot-time race stayed reported open for the
 * session's whole life, so the measurement left the boot and rides the session's own 5 s interval
 * (`SESSION_ACTIVITY_PERIOD_MS`, extensions/legion.ts) while the session is idle, for at most
 * `CAPABILITY_ATTEMPTS` measurements, until every row of one report is present.
 *
 * Three things the code below does not show:
 *
 * - A held report keeps the `measuredAt` of the measurement that made it, and `measuredAt` is what
 *   the daemon ranks reports by, so a report held through a long turn reaches the daemon carrying
 *   its own older time rather than the time it left.
 * - A ready the daemon refuses — a suspended, retired or unregistered claim among the reasons — is
 *   this machine's refusal path, never a silent success: the report is dropped, the refusal is
 *   logged with the claim and the issue, and the next attempt measures afresh.
 * - "Every row present" is read over the session's own six rows, which mirror the daemon's live
 *   table (`LIVE_CAPABILITIES`, ./capability-report): the session is settled once the daemon has
 *   taken a report whose every row read `ok`.
 *
 * The send is a plain `send` call with no retry helper, unlike the boot's ready
 * (`callReadyWithRetry`, @legion/pi-shared/daemon-client), so one failure costs one log line and
 * one attempt and the next due tick is the retry. It is bounded at
 * `CAPABILITY_SEND_TIMEOUT_MS`: a daemon that takes the connection and never answers would
 * otherwise leave this machine's in-flight guard set for the session's life, so the session would
 * measure once, send once, log nothing and never measure again — the very failure this schedule
 * exists to end.
 *
 * Every collaborator is injected — the claim, the idle answer, the measurement, the send and the
 * log — so the only local module it reaches is `./capability-report`, for the label the bound's
 * message shares with the checks' own budgets, and a test drives the machine tick by tick on its
 * own clock.
 */

/** The earliest tick at which the architect measures: 30 s after the interval was registered,
 * which leaves the boot and the first task well clear. */
export const CAPABILITY_FIRST_DUE_TICK = 6;
/** The ticks between the end of one attempt and the next due tick: 25 s. */
export const CAPABILITY_RETRY_TICKS = 5;
/** The measurements one process makes in all. */
export const CAPABILITY_ATTEMPTS = 3;
/** How long one `claims/ready` carrying a report may take: the same 10 s the whole measurement is
 * bounded at (`CAPABILITY_REPORT_TIMEOUT_MS`, ./capability-report), and far inside the 25 s
 * `CAPABILITY_RETRY_TICKS` leaves before the next due tick, so a send that times out never
 * overlaps the attempt that follows it. */
export const CAPABILITY_SEND_TIMEOUT_MS = 10_000;

export interface CapabilitySchedule {
  /** One reading of the session's own interval. It never rejects, and the caller never awaits it:
   * a tick measures, sends a report, or does nothing, and never two of those at once. */
  readonly tick: () => Promise<void>;
}

/**
 * The tick machine behind one session's capability reporting: one schedule per process, driven by
 * the host's managed interval.
 *
 * `claim` is the registered claim, read for its `role` (which spreads the six roles of an issue
 * pod one tick apart), and for its `issue` and `claimToken` (the log lines). `isIdle` is the live
 * idle answer, read afresh at every reading: `undefined` is a host that answers nothing about its
 * idleness, and counts as idle. `measure` is one whole measurement of the six rows, `send` is one
 * `claims/ready` carrying a report, which resolves when the daemon took it and rejects on a
 * refusal or a transport failure, `sendTimeoutMs` is how long that send may take, and `log`
 * writes one line with its fields.
 */
export function createCapabilitySchedule(deps: {
  readonly claim: ClaimCapability;
  readonly isIdle: () => boolean | undefined;
  readonly measure: () => Promise<LegionCapabilityReportBody>;
  /** One `claims/ready` carrying the report. `signal` aborts once the bound passes, and the send
   * is to abort the request with it (`LegionDaemonClient.ready`'s `signal`), so a daemon that
   * hangs is left no open request. */
  readonly send: (report: LegionCapabilityReportBody, signal: AbortSignal) => Promise<void>;
  /** The bound on one send, `CAPABILITY_SEND_TIMEOUT_MS` in production. */
  readonly sendTimeoutMs: number;
  readonly log: (message: string, fields: Readonly<Record<string, string>>) => void;
}): CapabilitySchedule {
  /** The ticks since the interval was registered; `1` at the first tick. */
  let count = 0;
  /** The tick the next attempt is due at: the architect at the sixth (30 s), the merger at the
   * eleventh (55 s), so one pod's roles start their attempts on different ticks. Each role counts
   * from its own interval, which starts at its own boot, and a role busy at its due tick measures
   * at its first idle tick instead, so the offset spreads the attempts rather than keeping two
   * measurements from ever overlapping. */
  let dueTick = CAPABILITY_FIRST_DUE_TICK + LEGION_ROLES.indexOf(deps.claim.role);
  /** The measurements spent, bounded by `CAPABILITY_ATTEMPTS` for the life of the process. */
  let attempts = 0;
  /** True while a measurement or a send is in flight. */
  let running = false;
  /** A measured report whose send waits for an idle tick. */
  let held: LegionCapabilityReportBody | undefined;
  /** True once the daemon has taken a report whose every row was present. */
  let settled = false;

  /** The live idle answer. Only `false` is busy; a throw is the host telling nothing, so it counts
   * as idle like `undefined` does. */
  const isIdle = (): boolean => {
    try {
      return deps.isIdle() !== false;
    } catch {
      return true;
    }
  };

  /** One line naming the claim, its issue and the error, with a log that throws swallowed: this
   * machine's failures stay inside it. */
  const note = (message: string, error: unknown): void => {
    try {
      deps.log(message, {
        claim: deps.claim.claimToken,
        issue: deps.claim.issue,
        error: messageFor(error),
      });
    } catch {
      // A log that fails is no reason to drop the schedule.
    }
  };

  /** One `claims/ready` carrying `report`, which the caller has already marked running, bounded
   * at `deps.sendTimeoutMs`: the bound aborts the request and rejects, so a daemon that takes
   * the connection and never answers ends this send rather than the session's reporting. A send
   * that times out takes the path a refused one takes — the report dropped, one line logged, the
   * attempt spent, the next attempt due `CAPABILITY_RETRY_TICKS` later. The hold is cleared
   * however it ends — a refused report is dropped rather than sent again as it stands — and
   * every ending but a settled one leaves the next attempt due `CAPABILITY_RETRY_TICKS` after
   * the send ended, read then, so a slow send does not shorten the wait. */
  const sendReport = async (report: LegionCapabilityReportBody): Promise<void> => {
    // The abort is what ends the real request; the race is what guarantees this machine ends even
    // against a `send` that ignores the signal, so `running` always clears.
    const controller = new AbortController();
    const deadline = Promise.withResolvers<never>();
    const timer = setTimeout(() => {
      const late = new Error(
        `claims/ready did not answer within ${budgetLabel(deps.sendTimeoutMs)}`
      );
      deadline.reject(late);
      controller.abort(late);
    }, deps.sendTimeoutMs);
    try {
      await Promise.race([deps.send(report, controller.signal), deadline.promise]);
      // "Every row present" over the session's own six rows. A report with no rows settles
      // nothing: it proves no capability.
      if (report.rows.length > 0 && report.rows.every((row) => row.ok)) {
        settled = true;
        return;
      }
    } catch (error) {
      note("legion: this session's capability report did not reach the daemon", error);
    } finally {
      clearTimeout(timer);
      held = undefined;
    }
    dueTick = count + CAPABILITY_RETRY_TICKS;
  };

  const tick = async (): Promise<void> => {
    count += 1;
    if (settled || running || !isIdle()) return;
    // A held report is sent whatever the attempts say: its measurement is good, and only its
    // send waited for the turn to end.
    if (held === undefined && (count < dueTick || attempts >= CAPABILITY_ATTEMPTS)) return;
    running = true;
    try {
      if (held !== undefined) {
        await sendReport(held);
        return;
      }
      // The attempt is spent whatever follows, so a measurement that fails costs one of the three.
      attempts += 1;
      let report: LegionCapabilityReportBody;
      try {
        report = await deps.measure();
      } catch (error) {
        dueTick = count + CAPABILITY_RETRY_TICKS;
        note("legion: this session's capability measurement failed", error);
        return;
      }
      // A turn can start while a measurement runs, so the idle answer is read again: the report
      // waits for an idle tick rather than reaching the daemon from under a working session.
      if (!isIdle()) {
        held = report;
        return;
      }
      await sendReport(report);
    } finally {
      running = false;
    }
  };

  return { tick };
}
