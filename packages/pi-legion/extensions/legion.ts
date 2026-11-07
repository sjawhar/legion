import { randomUUID } from "node:crypto";
import path from "node:path";
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config";
import { resolveIssueDocumentId } from "@legion/envoy-client/dispatch-execute";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { sessionDirectory as dispatchSessionDirectory } from "@legion/envoy-client/dispatch-session-directory";
import { writeSessionTitle } from "@legion/envoy-client/dispatch-session-state";
import { messageFor } from "@legion/envoy-client/errors";
import {
  sessionConfigFile,
  sessionDispatchSettings,
  sessionEnvoySettings,
} from "@legion/envoy-client/session-settings";
import { createEnvoyClient } from "@legion/envoy-client/transport";
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import { currentSessionID } from "@legion/pi-shared/dispatch-session-id";
import { findPushedBranchHead, paneHandoffReport } from "@legion/pi-shared/handoff-record";
import { matchInjectedUserTurn } from "@legion/pi-shared/injected-user-turns";
import {
  ENVOY_PLUGIN_INTERFACE_VERSION,
  LEGACY_LEGION_LOADED_KEY,
  LEGION_PLUGIN_LOADED_KEY,
  readEnvoyPluginInterface,
} from "@legion/pi-shared/interface";
import { createPhaseReminder, type EnvoyPublish } from "@legion/pi-shared/phase-reminder";
import {
  assistantRanTool,
  assistantText,
  inboundKind,
  PHASE_STALL_ENTRY,
  restorePhaseStallEntry,
} from "@legion/pi-shared/phase-stall";
import type { CommandContext, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { legionRoleClaimBridge } from "@legion/pi-shared/role-claim-bridge";
import {
  asyncJobKinds,
  type SessionActivityReport,
  sameSessionActivity,
  sessionActivity,
  sessionActivityRequest,
} from "@legion/pi-shared/session-activity";
import { stallLimits } from "@legion/pi-shared/stall-limits";
import {
  createStallWatchdog,
  type StallWatchdog,
  type StallWatchdogProcesses,
} from "@legion/pi-shared/stall-watchdog";
import { clearStartupTurn } from "@legion/pi-shared/startup-turn";
import { subagentSessionCheck } from "@legion/pi-shared/subagent-session";
import { logger } from "@oh-my-pi/pi-utils";
import {
  type CapabilityBudgets,
  type CapabilityHost,
  measureCapabilities,
  ompCapabilityHost,
} from "../src/capability-report";
import {
  CAPABILITY_SEND_TIMEOUT_MS,
  type CapabilitySchedule,
  createCapabilitySchedule,
} from "../src/capability-schedule";
import { createClaimSession } from "../src/claim-session";
import { classifySession, type LegionSessionKind, requiredEnvironment } from "../src/classify";
import { createControllerSession } from "../src/controller-session";
import { applySessionTitle, legionSessionTitle } from "../src/session-title";
import { daemonLaunchedController } from "../src/slack-channels";
import { createStallAborter } from "../src/stall-abort";
import { createStallTeller } from "../src/stall-evidence";
import { createStallExits } from "../src/stall-exits";
import { createStallProcesses, type StallProcessesDeps } from "../src/stall-recovery";
import { createLegionTool } from "../src/tools";
import {
  branchHoldsWorkspaceRecreatedNotice,
  type WorkspaceRecreatedNotice,
  withoutRepeatedWorkspaceRecreatedNotice,
  workspaceRecreatedMessage,
  workspaceRecreatedNotice,
} from "../src/workspace-recreated";

// Fatal bootstrap failures call this instead of `process.exit` directly, so a
// test can substitute a throwing stand-in without killing the test runner. The stall watch's
// last step reaches this same seam, since that exit is the one the daemon reads as a death.
// Production callers never override it.
let exitProcess: (code: number) => never = (code) => process.exit(code) as never;
export function setLegionBootstrapExitForTests(hook: (code: number) => never): void {
  exitProcess = hook;
}

// The process table the stall watch reads its evidence from and ends a stuck call's processes
// through (`src/stall-recovery.ts`): this machine's own `/proc` and `process.kill`. A test
// substitutes a fake, since the production one reads the live tree of whatever machine the test
// runs on and ends what it finds there.
let stallProcessHost: (deps: StallProcessesDeps) => StallWatchdogProcesses = createStallProcesses;
export function setLegionStallProcessesForTests(
  host: ((deps: StallProcessesDeps) => StallWatchdogProcesses) | undefined
): void {
  stallProcessHost = host ?? createStallProcesses;
}

// The host the claim session's capability report measures through: Oh My Pi's own modules and
// the pane's `dispatch` and `gh`. A test substitutes a fake, since the production host reaches
// modules that exist only inside Oh My Pi's bundle and runs the pane's commands.
let capabilityHost: () => CapabilityHost = ompCapabilityHost;
export function setLegionCapabilityHostForTests(host: () => CapabilityHost): void {
  capabilityHost = host;
}

// The budgets the capability measurement runs under. Production never sets this, so
// `measureCapabilities` keeps its own 8 s per check and 10 s per report; a test sets a short pair,
// since one row that fails on its own budget would otherwise cost 8 s, past Bun's 5 s default for
// a single test.
let capabilityBudgets: CapabilityBudgets | undefined;
export function setLegionCapabilityBudgetsForTests(budgets: CapabilityBudgets | undefined): void {
  capabilityBudgets = budgets;
}

// The bound on one `claims/ready` the capability schedule sends. Production never sets this, so
// the send keeps `CAPABILITY_SEND_TIMEOUT_MS`, the real 10 s; a test sets a short one, since a
// send the daemon never answers would otherwise hold its tick for 10 s, past Bun's 5 s default
// for a single test.
let capabilitySendTimeoutMs = CAPABILITY_SEND_TIMEOUT_MS;
export function setLegionCapabilitySendTimeoutForTests(ms: number | undefined): void {
  capabilitySendTimeoutMs = ms ?? CAPABILITY_SEND_TIMEOUT_MS;
}

async function persistedTranscript(
  context: CommandContext | SessionContext
): Promise<{ readonly sessionFile: string; readonly agentId: string }> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile?.endsWith(".jsonl")) {
    throw new Error("Legion session must have a persisted transcript");
  }
  const agentId = path.basename(sessionFile, ".jsonl");
  if (!agentId) throw new Error("Legion session transcript has no agent id");
  return { sessionFile, agentId };
}

/**
 * Why this Legion entry cannot run in this process, or undefined when it can. Legion claims roles
 * and reads deliveries through the interface the Envoy plugin publishes
 * (`@legion/pi-shared/interface`), so a process without `@sjawhar/pi-envoy`, or with one that
 * speaks another interface version, has nothing to claim through; and the pre-split
 * `@sjawhar/pi-legion-envoy` still installed beside this plugin would run a second Legion entry
 * against the same pane. The sentence names the remedy, since the daemon's boot log and the
 * operator's own session are where it lands.
 */
function envoyPluginRefusal(): string | undefined {
  const legacy = (globalThis as Record<symbol, unknown>)[LEGACY_LEGION_LOADED_KEY];
  if (legacy !== undefined) {
    const where = typeof legacy === "string" ? ` from ${legacy}` : "";
    return `Legion plugin at ${import.meta.url} found the pre-split @sjawhar/pi-legion-envoy loaded${where}; uninstall @sjawhar/pi-legion-envoy (omp plugin uninstall @sjawhar/pi-legion-envoy) and keep @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
  }
  const reading = readEnvoyPluginInterface();
  switch (reading.kind) {
    case "present":
      return undefined;
    case "absent":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${ENVOY_PLUGIN_INTERFACE_VERSION}, found none; install @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
    case "mismatch":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${reading.expected}, found version ${reading.found} from ${reading.from}; install the @sjawhar/pi-envoy released with this @sjawhar/pi-legion`;
  }
}

export default function legionExtension(pi: PiApi): void {
  // One instance per session (a `task` subagent gets its own); the id tells them apart in the log.
  const instance = randomUUID().slice(0, 8);
  logger.debug("extension instance loaded", { extension: import.meta.url, instance });
  // Read by the daemon's boot gate to prove this extension actually loaded from an ambient
  // installed-plugin discovery -- not just that a manifest file exists, which stays true even when
  // the plugin is disabled or unregistered in OMP's own plugin registry -- and which interface
  // version it speaks.
  (globalThis as Record<symbol, unknown>)[LEGION_PLUGIN_LOADED_KEY] = {
    from: import.meta.url,
    envoyInterface: ENVOY_PLUGIN_INTERFACE_VERSION,
  };
  // Whether this is the daemon-launched controller, whose Slack subscriptions its role owns: the
  // Envoy entry's recovery of a resumed session's registered interests then drops every Slack
  // topic before it restores the rest. Set here, at load, because that recovery runs in the Envoy
  // entry's session_start, before this entry's claim.
  if (daemonLaunchedController(process.env)) {
    legionRoleClaimBridge().slackRoleOwned = true;
  }

  // Gates session_start and the session-change re-claim below, one call per hook; its settled
  // answer is kept, so it runs once per session.
  const checkSubagentSession = subagentSessionCheck();

  // The endless phase reminder (src/phase-stall.ts, src/phase-reminder.ts). It runs only in a
  // session holding a claim for its own id with a phase role, so never in an architect (a root or
  // a sub-architect), the controller (whose claim lives in controllerSession), a session with no
  // Legion environment, or a `task` subagent (whose instance returns at checkSubagentSession
  // before any capability exists). Restored from the transcript at session_start and appended to
  // it on every change.
  //
  // The runner's timers and its record check run outside every hook, so the pane's own session
  // context is kept here (`paneContext`, set at session_start and after every session change):
  // one Oh My Pi process holds one claim for its whole life, and this is the session that claim
  // is on.
  let paneContext: SessionContext | undefined;
  const phaseWorkerSession = (context: SessionContext): boolean => {
    const role = claimSession.capability(context.sessionManager.getSessionId())?.role;
    return role !== undefined && role !== "architect";
  };
  // The claim this pane holds, read live from whatever session the pane is on: the reminder's
  // notice, the stall watch's evidence and its owner's notice each name it.
  const paneClaim = () =>
    paneContext === undefined
      ? undefined
      : claimSession.capability(paneContext.sessionManager.getSessionId());
  // The numbers both watches of this entry run on (`@legion/pi-shared/stall-limits`), read once
  // at load: the reminder's thresholds, and the stall watch's grace over a call's own declared
  // deadline, its bound for a call no deadline judges, and the gap between its steps.
  const limits = stallLimits(process.env, logger, process.env.LEGION_ROLE);
  // The Envoy listener this entry's own notices go to — the architect's reminder notice, and the
  // stall watch's notice of a stuck call — built once and only when a notice is due: this entry
  // reaches no listener of its own otherwise. Envoy off for the session leaves it undefined, and
  // the reminder keeps coming without a notice.
  let noticePublisher: { readonly publish: EnvoyPublish | undefined } | undefined;
  const publisher = (): EnvoyPublish | undefined => {
    if (noticePublisher === undefined) {
      const envoy = sessionEnvoySettings(sessionConfigFile("omp", process.env), process.env, {
        cwd: process.cwd(),
        nats: false,
        session: currentSessionID,
      });
      noticePublisher = {
        publish: envoy.enabled
          ? createEnvoyClient({
              baseUrl: envoy.url,
              credential: envoy.credential,
              fetch,
            }).publish
          : undefined,
      };
    }
    return noticePublisher.publish;
  };
  const phaseReminder = createPhaseReminder({
    sendReminder: (reminder) =>
      pi.sendMessage(
        { customType: "legion-phase-reminder", content: reminder, display: true },
        { deliverAs: "steer", triggerTurn: true }
      ),
    armTimer: (callback, delayMs) => paneContext?.setTimeout(callback, delayMs),
    saveEntry: (entry) => pi.appendEntry(PHASE_STALL_ENTRY, entry),
    claim: paneClaim,
    readState: (signal) => roleDaemon().state({ signal }),
    publisher,
    project: process.env.LEGION_PROJECT,
    limits,
    log: logger,
  });

  /**
   * INFRA-945: what this pane is doing, which the daemon reads to decide whether its issue pod
   * may be released. A report is sent at every settle, change or no change — the daemon releases
   * no claim whose last report predates its last turn, so a quiet turn that sent nothing would
   * hold the pod for ever — and between turns only when the report's content changes.
   *
   * Oh My Pi fires no event for background work, so what it is running is read rather than
   * waited for: `SESSION_ACTIVITY_PERIOD_MS` apart, the host's own managed interval reads
   * `ctx.getAsyncJobSnapshot()`, which is an in-memory look at this session's own job manager,
   * and compares the report it builds with the last one sent. Nothing leaves the pane unless
   * that content changed, so the cost between turns is one object comparison, and five seconds
   * bounds how stale the daemon's view of a job that just started can be against a release rule
   * measured in minutes.
   */
  const SESSION_ACTIVITY_PERIOD_MS = 5_000;
  let lastActivity: SessionActivityReport | undefined;
  const reportActivity = async (
    context: SessionContext,
    options: { readonly onlyWhenChanged: boolean }
  ): Promise<void> => {
    const claim = claimSession.capability(context.sessionManager.getSessionId());
    if (claim === undefined) return;
    // An architect arms no phase reminder, so it waits on no reminder of its own; what it
    // reports is its background work.
    const report = sessionActivity(
      asyncJobKinds(context.getAsyncJobSnapshot?.() ?? null),
      phaseWorkerSession(context) ? phaseReminder.phaseWake() : { nextReminderAt: null }
    );
    if (
      options.onlyWhenChanged &&
      lastActivity !== undefined &&
      sameSessionActivity(lastActivity, report)
    ) {
      return;
    }
    const request = sessionActivityRequest({
      project: process.env.LEGION_PROJECT,
      claim,
      report,
    });
    if (request === undefined) return;
    lastActivity = report;
    try {
      await roleDaemon().activity(request);
    } catch (error) {
      // The daemon did not take it, so it is not what the daemon holds: the next reading sends
      // again whether or not its content moved.
      lastActivity = undefined;
      logger.warn("legion: this session's activity report did not reach the daemon", {
        issue: claim.issue,
        error: messageFor(error),
      });
    }
  };

  let daemonClient: LegionDaemonClient | undefined;
  const roleDaemon = (): LegionDaemonClient => {
    daemonClient ??= createLegionDaemonClient(
      requiredEnvironment(process.env, "LEGION_DAEMON_URL")
    );
    return daemonClient;
  };

  // A root architect's or phase worker's claim, and the operator-launched controller's.
  const claimSession = createClaimSession({
    daemon: roleDaemon,
    persistedTranscript,
    exitProcess: (code) => exitProcess(code),
  });
  const controllerSession = createControllerSession({
    daemon: roleDaemon,
    persistedTranscript,
    pi,
    exitProcess: (code) => exitProcess(code),
  });

  /**
   * INFRA-1274: the worker's watch on its own tool calls, and the ladder it drives when one of
   * them stops. The detector is `@legion/pi-shared/stall-watchdog`, which holds the open calls
   * and decides when a step runs; the four collaborators it reaches the machine, the listener and
   * this process through are `src/stall-evidence.ts` (the evidence and the owner's notice),
   * `src/stall-recovery.ts` (the tree, and the stuck call's own processes), `src/stall-abort.ts`
   * (the turn, ended, and the message that starts the work again) and `src/stall-exits.ts` (the
   * count that bounds the last step).
   *
   * **It is armed at `session_start`, for every Legion session**: a phase worker's, a root
   * architect's and the controller's alike. A `task` subagent's instance of this module returns
   * at `checkSubagentSession` before the arm, so it records nothing and sweeps nothing: a
   * subagent shares the worker's process, and the ladder's last step would end the parent with
   * it. The hooks below are registered at load, as every other hook of this entry is, and each
   * does nothing at all until a session arms the watch, which is what leaves a subagent's
   * instance recording nothing — and each returns for a subagent's own event where the host
   * hands it to this instance instead (`subagentCall`).
   */
  let stallWatch: StallWatchdog | undefined;
  /** The 5-second sweep, on the interval each session arms below. It is never awaited: a step of
   * the ladder can outlast the tick that armed it, and the detector takes one sweep at a time. */
  const stallSweep = (): void => {
    void stallWatch?.sweep();
  };
  const armStallWatch = (session: LegionSessionKind): void => {
    stallWatch = createStallWatchdog({
      now: Date.now,
      limits,
      // Live, as the activity report and the capability schedule read it; a host that answers
      // nothing is read as not idle, which is what a session running a stuck call is.
      isIdle: () => paneContext?.isIdle?.() ?? false,
      processes: stallProcessHost({
        // The transcript this session's tool runners name in `PI_SESSION_FILE`, which is the path
        // `persistedTranscript` put on disk at the boot below; read from the live session manager
        // rather than kept, so a session change carries the new one. A host that names no `.jsonl`
        // leaves every runner alone.
        sessionFile: () => {
          const file = paneContext?.sessionManager.getSessionFile();
          return file?.endsWith(".jsonl") === true ? file : undefined;
        },
        openCalls: () => stallWatch?.openCalls() ?? [],
        backgroundJobs: () => paneContext?.getAsyncJobSnapshot?.()?.running ?? [],
        log: logger,
      }),
      teller: createStallTeller({
        now: Date.now,
        session: () => session,
        claim: paneClaim,
        project: process.env.LEGION_PROJECT,
        readState: (signal) => roleDaemon().state({ signal }),
        publisher,
        appendEntry: (customType, data) => pi.appendEntry(customType, data),
        log: logger,
      }),
      aborter: createStallAborter({
        context: () => paneContext,
        // `pi`'s own method, called on `pi` as every other send of this entry is.
        sendMessage: (message, options) => pi.sendMessage(message, options),
      }),
      exits: createStallExits({
        stateDir: requiredEnvironment(process.env, "LEGION_STATE_DIR"),
        session,
        now: Date.now,
        log: logger,
      }),
      exitProcess: (code) => exitProcess(code),
      log: logger,
    });
  };

  /** A `task` subagent's own event, which this watch is not about: the subagent shares the
   * worker's process, so a ladder run for one of its calls would end the worker. The host hands
   * a subagent's events a context that names its runner (`agent.kind`, as envoy.ts's own
   * `tool_call` hook reads it); a host that names no agent is the top-level session. */
  const subagentEvent = (context: SessionContext): boolean => context.agent?.kind === "sub";

  /**
   * **The `tool_call` handler records the call's arguments and returns at once.** It blocks
   * nothing, revises nothing and awaits nothing, which is what makes a handler on this event safe
   * to register: the host holds the whole turn for the handler's budget
   * (`extensionHandlers.toolCallTimeoutMs`, 30 s by default) and fails the call closed when the
   * handler throws or outruns it, both measured on omp/18.16.0 on 2026-11-07. This reverses the
   * decision this entry carried since LEGION-631 — that it registers no `tool_call` hook at all —
   * and the reversal is deliberate (dispatch://INFRA-1274/spec#b-r2): what is gone with that
   * decision is the pane rule and the per-command grant mint, which refused calls and rewrote
   * their arguments; this handler does neither, and nothing a pane runs is refused or minted for.
   *
   * `tool_call` is the one event the host fires before a call can run at all, so it is where the
   * arguments come from; the clock starts at `tool_execution_start` instead, so neither the queue
   * nor the approval gate is counted against a call.
   */
  pi.on("tool_call", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.callRequested(event);
  });
  pi.on("tool_execution_start", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.executionStarted(event);
  });
  // Both ends of a call close its record, the error end of a call the host failed closed before
  // it ran among them.
  pi.on("tool_execution_end", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.executionEnded(event);
  });
  pi.on("tool_result", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.resultDelivered(event);
  });
  // The turn is over, so every call it held open is closed and no step runs for one of them. The
  // assistant message goes first: a turn the ladder's own abort ended carries
  // `stopReason: "aborted"` here, which is where an aborted turn is readable at all, since
  // `agent_end` carries no `stopReason` (`pi-shared/src/pi-types.ts`). A subagent's turn is not
  // this session's and closes none of its calls, the `task` call that is running it least of all.
  pi.on("turn_end", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.assistantMessage(event.message);
    stallWatch?.turnEnded();
  });
  // The run settled. `turn_end` fires 3 ms before it for an aborted run (measured), so this
  // closes nothing that run left open; it is what closes a run that ends with no turn end of its
  // own, whose last assistant message is the only thing that says how it ended.
  pi.on("agent_end", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.assistantMessage(event.messages?.at(-1));
    stallWatch?.turnEnded();
  });

  /**
   * Names the session by its Legion identity (`src/session-title.ts`), so every Dispatch
   * write stamps it as `origin.session_title` and the Envoy listener lists it. Runs before the
   * session claims its Envoy role: that claim registers the session, and the registration carries
   * the title then rather than at the next heartbeat. The `dispatch` command reads the title from
   * the session's `title` file, which envoy.ts rewrites only before a shell command whose session
   * name changed, so it is written here too, and the pane's first command already carries it.
   */
  const titleSession = async (context: SessionContext): Promise<void> => {
    const title = legionSessionTitle(classifySession(process.env), process.env.LEGION_PROJECT);
    if (title === undefined) return;
    await applySessionTitle(pi, context, title);
    const sessionID = context.sessionManager.getSessionId();
    if (sessionID === "") return;
    writeSessionTitle(
      dispatchSessionDirectory(process.env, sessionID),
      context.sessionManager.getSessionName?.() ?? title
    );
  };

  // The recreated-workspace notice this process owes its first run: set at session_start below,
  // with the id its copies carry, and owed until that run starts (agent_start), whatever starts it.
  let recreatedNotice: WorkspaceRecreatedNotice | undefined;

  pi.on("session_start", async (_event, context) => {
    // A `task`-spawned subagent session loads a fresh instance of this whole module: bail out
    // before classification, or the inherited LEGION_* environment would look like a fresh
    // root/worker boot and its failure would exit the parent process. See isSubagentSession.
    if (await checkSubagentSession(context)) return;
    await titleSession(context);
    // The session this pane's claim is on, for the reminder's timers and its record check, which
    // run outside every hook.
    paneContext = context;
    // A worker the daemon relaunched with --resume keeps its phase: its next turn may start from
    // an Envoy notice rather than a new assignment, and must find the phase still open. The
    // transcript is this host's store for that record; the runner is handed the record alone.
    phaseReminder.restore(restorePhaseStallEntry(context.sessionManager.getBranch?.() ?? []));
    const session = classifySession(process.env);
    if (session.kind === "not-legion") return;
    // Every Legion session (a root architect, a phase worker, the controller) claims through the
    // Envoy plugin's interface, so a process without it ends here, the way a refused boot
    // registration does: one log line, then the exit the daemon sees and relaunches from. A
    // person's own session returned above and never exits.
    const refusal = envoyPluginRefusal();
    if (refusal !== undefined) {
      logger.error(refusal);
      exitProcess(1);
    }
    // Every Legion session watches its own tool calls; a subagent's instance returned above.
    armStallWatch(session);
    // The controller registers through the controller session and gets the `legion` tool's
    // controller operations (`read_state`, `set_status`), each minting its own controller grant.
    if (session.kind === "controller") {
      await controllerSession.handleSessionStart(context);
      // The controller reports no activity — the daemon releases no pod of its — so it has no
      // managed interval of its own, and the stall watch takes one here: its session runs tool
      // calls that can stick like any other's.
      context.setInterval(stallSweep, SESSION_ACTIVITY_PERIOD_MS);
    } else {
      // A resume whose role launcher found the workspace recreated since the session was last
      // written (src/workspace-recreated.ts) is told so in a message saved to the session ahead of
      // the next turn, whatever starts that turn (a task, an Envoy delivery), so a process lost in
      // that turn resumes with the notice already in its history. It goes before the role claim, so
      // no delivery on the role's topic can start a turn ahead of it, and a subagent, which returned
      // above, never is told.
      recreatedNotice = workspaceRecreatedNotice(
        process.env,
        session.kind === "root-architect" ? session.tree : session.issue
      );
      if (recreatedNotice !== undefined) {
        pi.sendMessage(workspaceRecreatedMessage(recreatedNotice), {
          deliverAs: "steer",
          triggerTurn: false,
        });
      }
      await claimSession.bootstrap(context);
      // The claim is registered. A phase worker relaunched into an open phase gets its reminder
      // from a timer, since nothing else would wake it. The guard drops that timer when any turn
      // started meanwhile, which is exactly the case to drop — the daemon re-sending an
      // interrupted task arrives as a prompt.
      if (phaseWorkerSession(context)) phaseReminder.armResumeReminder();
      /**
       * INFRA-927: the session measures its six capability rows on this interval, while it is
       * idle, up to three times, until every row is present — a check that lost the boot-time
       * race stayed reported open for the session's whole life, since the boot measured once and
       * never again. The architect measures first at the sixth tick as every other role does
       * (`CAPABILITY_FIRST_DUE_TICK` plus its index in `LEGION_ROLES`); its extra readies make
       * its waiting notices due at once, which is the same release a relaunched architect's ready
       * already makes.
       *
       * The measurement's input is snapshotted here, since the host's accessors answer only from
       * a handler and throw at factory time, and it is read before `registerLegionTool()` below,
       * so the rows see the tool surface `session_start` exposes.
       */
      const capabilityClaim = claimSession.capability(context.sessionManager.getSessionId());
      let capabilitySchedule: CapabilitySchedule | undefined;
      if (capabilityClaim !== undefined) {
        const capabilityInput = {
          cwd: context.cwd,
          sessionId: capabilityClaim.sessionID,
          issue: capabilityClaim.issue,
          promptAgents: capabilityClaim.promptAgents ?? [],
          activeTools: pi.getActiveTools(),
          agentsExposed: pi.agents !== undefined,
        };
        capabilitySchedule = createCapabilitySchedule({
          claim: capabilityClaim,
          measure: () => measureCapabilities(capabilityHost(), capabilityInput, capabilityBudgets),
          send: (report, signal) =>
            roleDaemon().ready(
              {
                claimToken: capabilityClaim.claimToken,
                sessionId: capabilityClaim.sessionID,
                secret: capabilityClaim.secret,
                generation: capabilityClaim.generation,
                capabilities: report,
              },
              { signal }
            ),
          sendTimeoutMs: capabilitySendTimeoutMs,
          // Live, as the activity report reads it: a context captured once would answer for the
          // session as it was at its start.
          isIdle: () => paneContext?.isIdle?.(),
          log: (message, fields) => logger.warn(message, fields),
        });
      }
      // Between turns, the host's own managed interval is the only sight of this session's
      // background work; it sends nothing unless what it reads has changed. Every claimed
      // session reports, an architect's as much as a phase worker's: the daemon releases a pod
      // only when each of its live roles has said it runs nothing. The stall watch's sweep rides
      // the same 5 seconds, which bounds how late a stuck call is found against a ladder whose
      // own steps are minutes apart.
      context.setInterval(() => {
        if (paneContext !== undefined) {
          void reportActivity(paneContext, { onlyWhenChanged: true });
        }
        // None of the three is awaited: a measurement that runs for up to 10 s, or a step of the
        // ladder that outlasts the tick, never delays the activity report the daemon reads to
        // decide whether the pod may be released.
        void capabilitySchedule?.tick();
        stallSweep();
      }, SESSION_ACTIVITY_PERIOD_MS);
    }
    registerLegionTool();
    await activateLegionTool();
  });

  // A prompt (the daemon's task as an RPC `prompt`, or a person's direct message pi-envoy
  // delivers as a user turn) first recovers a failed last turn, and Oh My Pi recovers an empty
  // `length` stop by moving the branch back to that turn's parent, which takes the copy saved
  // after it off the branch. before_agent_start runs after that recovery, so it puts the notice
  // back into the run's messages when the branch no longer holds this process's copy, a notice an
  // earlier recreation saved not counting, and the run saves it on the branch. An Envoy card,
  // sent with triggerTurn, starts its turn with neither the recovery nor before_agent_start, so
  // its branch keeps the saved copy.
  let resentNoticeId: string | undefined;
  pi.on("before_agent_start", async (_event, context) => {
    if (recreatedNotice === undefined) return undefined;
    const branch = context.sessionManager.getBranch?.() ?? [];
    if (branchHoldsWorkspaceRecreatedNotice(branch, recreatedNotice.id)) return undefined;
    resentNoticeId = recreatedNotice.id;
    return { message: workspaceRecreatedMessage(recreatedNotice) };
  });
  pi.on("agent_start", async (_event, context) => {
    recreatedNotice = undefined;
    clearStartupTurn(context.sessionManager.getSessionId());
    // The operator moved on: a session change's re-claim outcome left in its widget comes down.
    controllerSession.handleAgentStart(context);
  });
  pi.on("session_shutdown", async (_event, context) => {
    clearStartupTurn(context.sessionManager.getSessionId());
    // Whatever this session had open is not this watch's business any more.
    stallWatch?.sessionChanged();
  });
  // The recovery kept the saved copy in the process's live context, so once it is sent again
  // every later request of this process would carry it twice: the first copy, where the history
  // put it, is the one each request keeps.
  pi.on("context", async (event) => {
    if (resentNoticeId === undefined) return undefined;
    const messages = withoutRepeatedWorkspaceRecreatedNotice(event.messages, resentNoticeId);
    return messages === undefined ? undefined : { messages };
  });

  // Mirrors envoy.ts: only a switch reports why the session changed; a branch or a tree
  // navigation carries no reason, and every one of them can leave the pane on a new session id.
  // The controller re-claims whatever session the pane is left on, so that session takes the
  // controller's title first; a session that keeps its id keeps its title. A `task` subagent's
  // instance does neither, and the subagent check is asked once for both.
  const afterSessionChange = async (context: SessionContext): Promise<void> => {
    if (await checkSubagentSession(context)) return;
    // Whatever session the pane is left on, every reminder timer armed on the old one is stale,
    // and the new one is where the next is armed; the calls the old session had open are closed
    // with it, so a later stuck call starts the ladder again at its first step.
    paneContext = context;
    phaseReminder.sessionChanged();
    stallWatch?.sessionChanged();
    if (classifySession(process.env).kind === "controller") await titleSession(context);
    await controllerSession.reclaimAfterSessionChange(context);
  };
  pi.on("session_switch", (_event, context) => afterSessionChange(context));
  pi.on("session_branch", (_event, context) => afterSessionChange(context));
  pi.on("session_tree", (_event, context) => afterSessionChange(context));

  // The daemon's assignment (a user message) opens the phase; an Envoy delivery re-arms a stall
  // whose reminders went unanswered or that a WAITING reply quieted. A person's direct message
  // that envoy.ts sent in as the user's own turn is a user message too (a `skill-prompt` custom
  // message for a `/skill:`), but it is an inbound event, as its Envoy card was: the record
  // envoy.ts keeps of the turns it sent in says which message that is, whichever of the two
  // extensions asks first. A turn that record misses falls through to `inboundKind` and counts as
  // an assignment; the phase-stall section of `packages/pi-legion/AGENTS.md` says which turns it
  // misses. The `legion` tool's successful `handoff_complete`, and a successful backward move,
  // close the phase (`onPhaseCompleted`, below). Every message, classified or not, says a turn is
  // running and leaves every armed reminder timer stale.
  pi.on("message_start", async (event, context) => {
    if (!phaseWorkerSession(context)) return;
    phaseReminder.turnStarted();
    const injected =
      matchInjectedUserTurn(context.sessionManager.getSessionId(), event.message) !== undefined;
    const kind = injected ? "inbound-event" : inboundKind(event.message);
    if (kind !== undefined) phaseReminder.apply({ kind });
  });

  // An assistant message that settled: one that ended aborted closes every call the stall watch
  // holds open, since the host ended the turn they belonged to, and that is where an aborted turn
  // is readable at all. One that ran at least one real tool call also says the worker is working,
  // so nothing it has been reminded of is unanswered — read from `message_end` rather than from
  // the `tool_call` handler above, which records a call's arguments and says nothing about
  // whether the turn it belongs to settled.
  pi.on("message_end", async (event, context) => {
    if (!subagentEvent(context)) stallWatch?.assistantMessage(event.message);
    if (!phaseWorkerSession(context)) return;
    if (assistantRanTool(event.message)) phaseReminder.apply({ kind: "tool-call" });
  });

  // The run is about to settle with nothing left for the host to continue: a phase still open
  // earns a reminder (src/phase-stall.ts). The first of a run comes back as the host's own
  // continuation, one more turn of this same session; every one after it is a timer, so the run
  // settles and the daemon sees the turn end (src/phase-reminder.ts).
  pi.on("session_stop", async (event, context) => {
    if (event.signal.aborted) return undefined;
    if (!phaseWorkerSession(context)) {
      // An architect has no phase of its own to settle, and its pod is released on the same rule
      // as a worker's, so its turn ends with a report of its own.
      await reportActivity(context, { onlyWhenChanged: false });
      return undefined;
    }
    const reminder = await phaseReminder.settle(assistantText(event.last_assistant_message));
    // After the settle, so the report carries the wake that settle armed: the daemon releases no
    // claim whose last report is older than its last turn, so this one goes whatever it says.
    await reportActivity(context, { onlyWhenChanged: false });
    return reminder === undefined ? undefined : { continue: true, additionalContext: reminder };
  });

  const onPhaseCompleted = (context: SessionContext): void => {
    if (phaseWorkerSession(context)) phaseReminder.apply({ kind: "phase-closed" });
  };

  // A successful `ready_for_update`: the worker has pushed what it wants kept and waits for the
  // process its claim runs in to be replaced. Its phase stays open, and the reminder must not
  // continue the turn it answered in — the work that continuation did would go with the pod, and
  // the daemon would see no turn end.
  const onUpdateAnswered = (context: SessionContext): void => {
    if (phaseWorkerSession(context)) phaseReminder.apply({ kind: "update-answered" });
  };

  const activateLegionTool = async (): Promise<void> => {
    const activeTools = pi.getActiveTools();
    if (activeTools.includes("legion")) return;
    await pi.setActiveTools([...activeTools, "legion"]);
  };

  // The session the tool answers for: a claim's (a root architect's, a sub-architect's or a phase
  // worker's, keyed by its role) or the claimed controller's; a `task` subagent holds neither and
  // is refused, as is a session whose boot has not registered yet.
  let legionToolRegistered = false;
  const registerLegionTool = (): void => {
    if (legionToolRegistered) return;
    legionToolRegistered = true;
    pi.registerTool(
      createLegionTool({
        pi,
        daemon: roleDaemon,
        session: (context) => {
          const sessionID = context.sessionManager.getSessionId();
          const active = claimSession.capability(sessionID);
          if (active !== undefined) {
            return { kind: active.role === "architect" ? "architect" : "phase-worker", ...active };
          }
          if (controllerSession.isClaimedSession(sessionID)) {
            return { kind: "controller", sessionID };
          }
          throw new Error("legion is available only to this session's registered claim");
        },
        controllerGrant: (sessionId) => controllerSession.mintGrant(sessionId),
        onPhaseCompleted,
        onUpdateAnswered,
        // The pane's own environment: its workspace and issue; PATH's jj, the one its shell runs;
        // and the Dispatch pi-envoy's session reads (Oh My Pi's rule, in the same process), which
        // the pane's `dispatch handoff-write` wrote the record to.
        handoffReport: (phase, role) =>
          paneHandoffReport(
            sessionConfigFile("omp", process.env),
            process.env,
            process.cwd(),
            currentSessionID()
          )(phase, role),
        branchHead: () => findPushedBranchHead({ env: process.env }),
        resolveDocument: async (issue, reference) => {
          // The file pi-envoy's session reads: Oh My Pi's rule, in the same process.
          const config = activeDispatchSettings(
            sessionDispatchSettings(sessionConfigFile("omp", process.env), process.env, {
              cwd: process.cwd(),
              session: currentSessionID,
            }).dispatch
          );
          if (config === null) {
            throw new Error(
              `register_gate cannot look up document "${reference}" on ${issue}: this pane has no Dispatch configured; pass the document's id`
            );
          }
          try {
            return await resolveIssueDocumentId(
              new DispatchClient(config.url, config.credential, fetch),
              issue,
              reference
            );
          } catch (error) {
            throw new Error(
              `register_gate could not look up document "${reference}" on ${issue} in Dispatch: ${messageFor(error)}`
            );
          }
        },
      })
    );
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    // Inside the controller's own session (the `LEGION_CONTROLLER` marker) this is the manual
    // override for a lost claim. From a hand-started session it is an interactive takeover: the
    // role and recorded session id move to this session (see `controllerSession.claim`), which
    // then gets the `legion` tool's controller operations as a launched controller does.
    // The operator started this session and reads it, so a missing Envoy plugin is answered as
    // the command's error, never an exit, and nothing is claimed.
    handler: async (_args, context) => {
      const refusal = envoyPluginRefusal();
      if (refusal !== undefined) throw new Error(refusal);
      await controllerSession.claim(context);
      registerLegionTool();
      await activateLegionTool();
    },
  });

  pi.registerCommand("legion-controller-start", {
    description:
      "Claim the Legion controller and send its start message as the session's first turn; `legion controller start` passes this command on Oh My Pi's command line",
    // Why a command and not a claim from session_start: `controllerSession.startCommand`.
    handler: async (_args, context) => {
      await controllerSession.startCommand(context);
    },
  });
}
