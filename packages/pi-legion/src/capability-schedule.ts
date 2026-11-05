import { LEGION_ROLES } from "@legion/contracts";
import type { LegionCapabilityReportBody } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";

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
 * one attempt and the next due tick is the retry.
 *
 * Every collaborator is injected — the claim, the idle answer, the measurement, the send and the
 * log — so this module reaches no Legion module of its own, and a test drives the machine tick
 * by tick on its own clock.
 */

/** The earliest tick at which the architect measures: 30 s after the interval was registered,
 * which leaves the boot and the first task well clear. */
export const CAPABILITY_FIRST_DUE_TICK = 6;
/** The ticks between the end of one attempt and the next due tick: 25 s. */
export const CAPABILITY_RETRY_TICKS = 5;
/** The measurements one process makes in all. */
export const CAPABILITY_ATTEMPTS = 3;

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
 * refusal or a transport failure, and `log` writes one line with its fields.
 */
export function createCapabilitySchedule(deps: {
  readonly claim: ClaimCapability;
  readonly isIdle: () => boolean | undefined;
  readonly measure: () => Promise<LegionCapabilityReportBody>;
  readonly send: (report: LegionCapabilityReportBody) => Promise<void>;
  readonly log: (message: string, fields: Readonly<Record<string, string>>) => void;
}): CapabilitySchedule {
  /** The ticks since the interval was registered; `1` at the first tick. */
  let count = 0;
  /** The tick the next attempt is due at: the architect at the sixth (30 s), the merger at the
   * eleventh (55 s), so one pod's roles never measure on the same tick. */
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

  /** One `claims/ready` carrying `report`, which the caller has already marked running. The hold
   * is cleared however it ends — a refused report is dropped rather than sent again as it
   * stands — and every ending but a settled one leaves the next attempt due
   * `CAPABILITY_RETRY_TICKS` after the send ended, read then, so a slow send does not shorten the
   * wait. */
  const sendReport = async (report: LegionCapabilityReportBody): Promise<void> => {
    try {
      await deps.send(report);
      // "Every row present" over the session's own six rows. A report with no rows settles
      // nothing: it proves no capability.
      if (report.rows.length > 0 && report.rows.every((row) => row.ok)) {
        settled = true;
        return;
      }
    } catch (error) {
      note("legion: this session's capability report did not reach the daemon", error);
    } finally {
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
