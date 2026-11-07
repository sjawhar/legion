import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import {
  controllerToken,
  isLegionProjectToken,
  type LegionRole,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import type { LegionState } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import {
  architectLive,
  type EnvoyPublish,
  RECORD_READ_BOUND_MS,
  withBound,
} from "@legion/pi-shared/phase-reminder";
import type {
  StallCall,
  StallEvidence,
  StallProcess,
  StallProcessTree,
  StallRule,
  StallStep,
  StallTold,
  StallWatchdogLog,
  StallWatchdogTeller,
} from "@legion/pi-shared/stall-watchdog";
import type { LegionSessionKind } from "./classify";

/**
 * The evidence one stuck tool call leaves, and the one notice its owner gets (INFRA-1274, R3).
 * `packages/pi-shared/src/stall-watchdog.ts` is the detector: it holds the open calls, decides
 * which one is overdue and drives the ladder. This module is the half of that watch which has to
 * touch the machine and the listener, which is why it lives here and reaches the detector as the
 * `StallWatchdogTeller` and the tree half of `StallWatchdogProcesses` (`src/stall-recovery.ts`
 * builds that pair, since the ending of a call's processes is the other half).
 *
 * **What the evidence is for.** A worker that ends its own turn, or its own process, with nothing
 * written behind it is the failure this issue exists to end: the next turn, and the person reading
 * the session afterwards, must be able to say what the call was and what the machine was doing.
 * So the record names the tool, a digest of the call's arguments bounded in length, when the
 * execution started, how long the call has been open, and the agent's process tree, and it is
 * written to the session's transcript before any step of the ladder runs.
 *
 * **What was measured, and when.** On 2026-11-07, in an issue pod of this deployment, against
 * `/opt/omp/bin/omp` (`omp/18.16.0`) in `--mode rpc`:
 *
 * 1. **A redirect from `/proc/self/environ` reads empty under this pod's gVisor**, while `env`
 *    from inside a process and `/proc/<pid>/environ` by number both read correctly
 *    (`FOO=bar sh -c "tr '\0' '\n' < /proc/self/environ | grep -c ."` printed 0 against 151 for
 *    the same read through `/proc/$$/environ`). So the reader never walks the `self` link and
 *    never reads the agent's own environment: it reads other pids, by number.
 * 2. **`PI_TOOL_CALL_ID` on a process of a call is exactly the frame's `toolCallId`**, which is
 *    what `StallProcess.toolCallId` carries and what `src/stall-recovery.ts` matches on.
 * 3. **`getconf CLK_TCK` printed 100**, which is Linux's `USER_HZ` on every architecture this
 *    deployment runs; nothing in Bun answers `sysconf`, so the number is stated here.
 */

/** Linux's `USER_HZ`: the unit `/proc/<pid>/stat` counts its times in (measurement 3). */
const CLOCK_TICKS_PER_SECOND = 100;

/**
 * The environment names the reader keeps from another process, and the only ones. An environment
 * routinely carries a credential — the claim's secret, a token file's contents — and this record
 * is written to a transcript and published to the owner, so everything else is dropped where it
 * is read rather than filtered later.
 *
 * `PI_SESSION_ID` is not among them because a tool's runner does not carry it: the `eval` kernel
 * names the session it serves in `PI_SESSION_FILE`, measured 2026-11-07.
 */
export const STALL_READ_ENVIRONMENT = ["PI_TOOL_CALL_ID", "PI_SESSION_FILE"] as const;

/** How long a digest of a call's arguments, or of one process's command, may be. Long enough for
 * a shell command a person can recognise, short enough that a notice stays one paragraph and a
 * transcript entry stays small. */
export const STALL_DIGEST_LIMIT = 512;

/** The transcript entry every record of this watch is written under (`pi.appendEntry`), beside
 * the phase reminder's `legion-phase-stall`. One name, three record kinds, so a reader of the
 * session — a person, or the live proof's harness — subscribes to one thing. */
export const STALL_WATCH_ENTRY = "legion-stall-watch";

/** One process of the agent's tree as the reader found it: what the evidence carries, plus the
 * two things it never carries — the parent, which is how the tree is built, and the few
 * environment names `src/stall-recovery.ts` reads to decide what belongs to a call. */
export interface StallScannedProcess extends StallProcess {
  readonly ppid: number;
  /** The names of `STALL_READ_ENVIRONMENT` this process carries, and nothing else. Empty for the
   * agent's own process, whose environment is never read (measurement 1). */
  readonly environment: Readonly<Record<string, string>>;
}

/** The agent's tree as the reader found it, or the fact that it could not be read: a machine
 * whose `/proc` the reader cannot read records that it could not read the tree, never an empty
 * one. */
export type StallProcessScan =
  | { readonly read: true; readonly processes: readonly StallScannedProcess[] }
  | { readonly read: false; readonly reason: string };

export interface StallProcessReaderDeps {
  /** The agent's own process, the root of the tree the evidence reports; `process.pid` unless a
   * test names another. */
  readonly agentPid?: number;
  /** Where the kernel's process table is mounted; `/proc` unless a test points the reader at a
   * path it cannot read, which is the machine this reader must report rather than answer empty
   * for. */
  readonly procRoot?: string;
}

/** The agent's process and its descendants, each with its identifier, command, start time, state,
 * processor time and the tool call its environment names. The agent comes first and its
 * descendants follow it parent before child, so two readings of the same tree print in the same
 * order. */
export async function readStallProcesses(
  deps: StallProcessReaderDeps = {}
): Promise<StallProcessScan> {
  const procRoot = deps.procRoot ?? "/proc";
  const agentPid = deps.agentPid ?? process.pid;
  let entries: string[];
  try {
    entries = await readdir(procRoot);
  } catch (error) {
    return { read: false, reason: `${procRoot} could not be read: ${messageFor(error)}` };
  }
  const bootMs = await bootTimeMs(procRoot);
  if (bootMs === undefined) {
    return {
      read: false,
      reason:
        `neither ${path.join(procRoot, "stat")}'s btime nor ${path.join(procRoot, "uptime")} ` +
        "could be read, so no process's start time could be told",
    };
  }

  /** Every process on the machine, by pid: the parent of each is what makes the agent's tree. */
  const all = new Map<number, ProcessStat>();
  const numbered = entries.filter((entry) => {
    const pid = Number.parseInt(entry, 10);
    return Number.isInteger(pid) && String(pid) === entry;
  });
  // Every `stat` at once rather than one awaited read per pid: this reader runs twice on the
  // stall path — the tree and the call's own processes — ahead of the step that frees the worker.
  const read = await Promise.all(
    numbered.map((entry) => readTextFile(path.join(procRoot, entry, "stat")))
  );
  for (const text of read) {
    // A pid that ended between the listing and the read is gone, not a tree that cannot be read.
    if (text === undefined) continue;
    const stat = parseProcessStat(text);
    if (stat !== undefined) all.set(stat.pid, stat);
  }
  if (!all.has(agentPid)) {
    return {
      read: false,
      reason: `${path.join(procRoot, String(agentPid))}, the agent's own process, could not be read`,
    };
  }

  const children = new Map<number, number[]>();
  for (const stat of all.values()) {
    if (stat.pid === agentPid) continue;
    const siblings = children.get(stat.ppid);
    if (siblings === undefined) children.set(stat.ppid, [stat.pid]);
    else siblings.push(stat.pid);
  }

  const processes: StallScannedProcess[] = [];
  // One level of the tree at a time, so a node's two reads are one `Promise.all` and a level's
  // nodes are read together. The order is the breadth-first one the record documents: the agent,
  // then its children by pid, then theirs.
  let level = [agentPid];
  while (level.length > 0) {
    const nodes = level.flatMap((pid) => {
      const stat = all.get(pid);
      return stat === undefined ? [] : [stat];
    });
    const scanned = await Promise.all(
      nodes.map(async (stat): Promise<StallScannedProcess> => {
        const directory = path.join(procRoot, String(stat.pid));
        const reads: [Promise<Record<string, string>>, Promise<string>] = [
          // Measurement 1: the agent's own environment is the one `/proc` will not hand it, and
          // it is not evidence about anything — the reader reads other pids only.
          stat.pid === agentPid
            ? Promise.resolve({})
            : readEnvironment(path.join(directory, "environ")),
          readCommand(path.join(directory, "cmdline"), stat.comm),
        ];
        const [environment, command] = await Promise.all(reads);
        const toolCallId = environment.PI_TOOL_CALL_ID;
        return {
          pid: stat.pid,
          ppid: stat.ppid,
          command,
          startedAt: bootMs + Math.round((stat.startTicks / CLOCK_TICKS_PER_SECOND) * 1_000),
          state: stat.state,
          cpuMs: Math.round((stat.cpuTicks / CLOCK_TICKS_PER_SECOND) * 1_000),
          environment,
          ...(toolCallId === undefined ? {} : { toolCallId }),
        };
      })
    );
    for (const node of scanned) processes.push(node);
    level = nodes.flatMap((stat) => (children.get(stat.pid) ?? []).sort((a, b) => a - b));
  }
  return { read: true, processes };
}

/** The scan as the evidence carries it: the environments and the parents are dropped, because a
 * record that is written to a transcript and published to an owner carries no environment. */
export function stallProcessTree(scan: StallProcessScan): StallProcessTree {
  if (!scan.read) return scan;
  return {
    read: true,
    processes: scan.processes.map(({ pid, command, startedAt, state, cpuMs, toolCallId }) => ({
      pid,
      command,
      startedAt,
      state,
      cpuMs,
      ...(toolCallId === undefined ? {} : { toolCallId }),
    })),
  };
}

interface ProcessStat {
  readonly pid: number;
  readonly comm: string;
  readonly state: string;
  readonly ppid: number;
  readonly cpuTicks: number;
  readonly startTicks: number;
}

/**
 * One line of `/proc/<pid>/stat`. The second field is the executable's name in parentheses and may
 * itself hold spaces and parentheses, so the fields after it are read from the last `)` of the
 * line, as `proc(5)` says. The fields taken, counting from the state: state, parent, user time
 * (11) and system time (12), and the start time in ticks since boot (19).
 */
function parseProcessStat(text: string): ProcessStat | undefined {
  const open = text.indexOf("(");
  const close = text.lastIndexOf(")");
  if (open < 0 || close < open) return undefined;
  const pid = Number.parseInt(text.slice(0, open).trim(), 10);
  const fields = text
    .slice(close + 1)
    .trim()
    .split(/\s+/);
  const state = fields[0];
  const ppid = Number.parseInt(fields[1] ?? "", 10);
  const utime = Number.parseInt(fields[11] ?? "", 10);
  const stime = Number.parseInt(fields[12] ?? "", 10);
  const startTicks = Number.parseInt(fields[19] ?? "", 10);
  if (
    !Number.isInteger(pid) ||
    state === undefined ||
    !Number.isInteger(ppid) ||
    !Number.isInteger(utime) ||
    !Number.isInteger(stime) ||
    !Number.isInteger(startTicks)
  ) {
    return undefined;
  }
  return {
    pid,
    comm: text.slice(open + 1, close),
    state,
    ppid,
    cpuTicks: utime + stime,
    startTicks,
  };
}

/** When the machine booted, which is what turns a process's start time in ticks into a wall-clock
 * moment: `btime` of `/proc/stat`, and `/proc/uptime` where a kernel writes no `btime`. */
async function bootTimeMs(procRoot: string): Promise<number | undefined> {
  const stat = await readTextFile(path.join(procRoot, "stat"));
  const btime = stat?.split("\n").find((line) => line.startsWith("btime "));
  if (btime !== undefined) {
    const seconds = Number.parseInt(btime.slice("btime ".length).trim(), 10);
    if (Number.isFinite(seconds)) return seconds * 1_000;
  }
  const uptime = await readTextFile(path.join(procRoot, "uptime"));
  const seconds = Number.parseFloat(uptime?.split(" ")[0] ?? "");
  if (Number.isFinite(seconds)) return Date.now() - seconds * 1_000;
  return undefined;
}

/** `/proc/<pid>/cmdline`, whose arguments are separated by NUL bytes, bounded as every other text
 * here is; the name `stat` carries for a process that writes no command line, a kernel thread
 * among them. */
async function readCommand(file: string, comm: string): Promise<string> {
  const text = await readTextFile(file);
  const argv = (text ?? "").split("\0").filter((part) => part !== "");
  return boundedText(oneLine(argv.length === 0 ? `[${comm}]` : argv.join(" ")), STALL_DIGEST_LIMIT);
}

/** The names of `STALL_READ_ENVIRONMENT` this process carries. A process that ended, or one this
 * process may not read, carries none as far as this reader is concerned: an unreadable
 * environment is one process the evidence says less about, never a tree that could not be read. */
async function readEnvironment(file: string): Promise<Record<string, string>> {
  const text = await readTextFile(file);
  const environment: Record<string, string> = {};
  if (text === undefined) return environment;
  for (const pair of text.split("\0")) {
    const split = pair.indexOf("=");
    if (split <= 0) continue;
    const name = pair.slice(0, split);
    if (!STALL_READ_ENVIRONMENT.some((wanted) => wanted === name)) continue;
    environment[name] = pair.slice(split + 1);
  }
  return environment;
}

async function readTextFile(file: string): Promise<string | undefined> {
  try {
    return await readFile(file, "utf8");
  } catch {
    return undefined;
  }
}

/** One line, so a record reads the same in a transcript, a log line and a notice. */
function oneLine(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/** `text` cut to `limit` characters, saying how long it was. The marker's own length is counted
 * against the limit, so the answer is never longer than the bound, whatever was cut. */
export function boundedText(text: string, limit: number): string {
  if (text.length <= limit) return text;
  const marker = `… (${text.length} characters in all)`;
  if (marker.length >= limit) return text.slice(0, limit);
  return text.slice(0, limit - marker.length) + marker;
}

/**
 * The call's arguments as the evidence names them: every argument, with its value rendered as
 * JSON so a string is told from a number, on one line and bounded in length. The arguments
 * themselves are never written anywhere else — a shell command routinely holds a credential, and
 * `StallCall.input` says so — and this digest is the one thing that leaves the session.
 */
export function stallArgumentsDigest(
  input: Record<string, unknown>,
  limit: number = STALL_DIGEST_LIMIT
): string {
  const parts = Object.entries(input).map(([name, value]) => `${name}=${renderArgument(value)}`);
  return boundedText(oneLine(parts.join(" ")), limit);
}

function renderArgument(value: unknown): string {
  try {
    return JSON.stringify(value) ?? String(value);
  } catch (error) {
    return `<unreadable: ${messageFor(error)}>`;
  }
}

/** One process of the tree, as the record names it: the times as ISO 8601, so a person reading a
 * transcript entry or a notice reads a moment rather than a number. */
export interface StallEvidenceProcess {
  readonly pid: number;
  readonly command: string;
  readonly startedAt: string;
  readonly state: string;
  readonly cpuMs: number;
  readonly toolCallId?: string;
}

export type StallEvidenceTree =
  | { readonly read: true; readonly processes: readonly StallEvidenceProcess[] }
  | { readonly read: false; readonly reason: string };

/** What one overdue call leaves behind: the record that goes into the session's transcript and
 * into the owner's notice. */
export interface StallEvidenceRecord {
  readonly kind: "evidence";
  /** When the evidence was taken. */
  readonly at: string;
  /** Which kind of session this is (`src/classify.ts`), which is what decides who is told. */
  readonly session: LegionSessionKind["kind"];
  readonly issue?: string;
  readonly role?: LegionRole;
  readonly toolCallId: string;
  readonly tool: string;
  /** `stallArgumentsDigest` of the arguments the call was made with. */
  readonly arguments: string;
  /** When the host handed the call to its tool, which is when it started being judged. */
  readonly startedAt: string | null;
  /** How long the call has been open. */
  readonly openMs: number;
  /** How long it was allowed to be: its own deadline plus the grace, or its role's bound. */
  readonly allowedMs: number;
  readonly rule: StallRule;
  readonly tree: StallEvidenceTree;
}

/** One step of the ladder, as the transcript records it. */
export interface StallStepTranscript {
  readonly kind: "step";
  readonly at: string;
  readonly toolCallId: string;
  readonly tool: string;
  readonly step: StallStep;
  readonly openMs: number;
  readonly outcome: string;
}

/** The ladder stopping short of a step it would otherwise have run. */
export interface StallStoppedTranscript {
  readonly kind: "stopped";
  readonly at: string;
  readonly toolCallId: string;
  readonly tool: string;
  readonly reason: string;
}

export type StallWatchRecord = StallEvidenceRecord | StallStepTranscript | StallStoppedTranscript;

/** Who this session is, for a record that names it. */
export interface StallSessionIdentity {
  readonly session: LegionSessionKind;
  readonly claim: ClaimCapability | undefined;
}

/** How long the rule that judged this call allowed it to run. The words a notice says about it
 * are this module's; the two numbers are `StallRule`'s. */
function allowedMs(rule: StallRule): number {
  return rule.kind === "deadline" ? rule.deadlineMs + rule.graceMs : rule.boundMs;
}

export function stallEvidenceRecord(
  evidence: StallEvidence,
  at: number,
  who: StallSessionIdentity
): StallEvidenceRecord {
  const { call, overdue, tree } = evidence;
  return {
    kind: "evidence",
    at: new Date(at).toISOString(),
    session: who.session.kind,
    ...(who.claim === undefined ? {} : { issue: who.claim.issue, role: who.claim.role }),
    toolCallId: call.toolCallId,
    tool: call.toolName,
    arguments: stallArgumentsDigest(call.input),
    startedAt: call.startedAt === undefined ? null : new Date(call.startedAt).toISOString(),
    openMs: overdue.openMs,
    allowedMs: allowedMs(overdue.rule),
    rule: overdue.rule,
    tree: tree.read
      ? {
          read: true,
          processes: tree.processes.map((process) => ({
            pid: process.pid,
            command: process.command,
            startedAt: new Date(process.startedAt).toISOString(),
            state: process.state,
            cpuMs: process.cpuMs,
            ...(process.toolCallId === undefined ? {} : { toolCallId: process.toolCallId }),
          })),
        }
      : { read: false, reason: tree.reason },
  };
}

/** The sentence the owner is sent, with the whole record travelling as the message's payload. */
export function stallNoticeMessage(record: StallEvidenceRecord): string {
  const open = Math.round(record.openMs / 1_000);
  const allowed = Math.round(record.allowedMs / 1_000);
  const who =
    record.role === undefined || record.issue === undefined
      ? "This session"
      : `The ${record.role} of ${record.issue}`;
  const allowance =
    record.rule.kind === "deadline"
      ? "its own deadline and the grace allow"
      : "its role's bound allows";
  const tree = record.tree.read
    ? `${record.tree.processes.length} process${record.tree.processes.length === 1 ? "" : "es"} in its tree`
    : `a process tree it could not read (${record.tree.reason})`;
  return (
    `${who} has a \`${record.tool}\` call that has not returned: it has been running ${open} ` +
    `seconds, past the ${allowed} seconds ${allowance}. Its arguments: ${record.arguments}. ` +
    `The worker is recovering it in steps, with ${tree}; the whole record is in this message's ` +
    "payload and in the worker's own transcript."
  );
}

/** Where one session's notice goes, or why it goes nowhere. */
export type StallNotice =
  | { readonly kind: "topic"; readonly topic: string }
  | { readonly kind: "none"; readonly why: string };

export interface StallNoticeInput {
  readonly session: LegionSessionKind;
  readonly claim: ClaimCapability | undefined;
  /** `LEGION_PROJECT`: the project token every role topic carries. */
  readonly project: string | undefined;
  /** Whether the issue's own architect claim is live, read as the phase reminder reads it
   * (`pi-shared/src/phase-reminder.ts:219-300`). */
  readonly liveArchitect: boolean;
}

/**
 * Who hears about one session's stuck call, chosen as the phase reminder chooses it today:
 *
 * - a phase worker's session tells its issue's architect, preferring the issue's own live
 *   architect and falling back to the tree's;
 * - a root architect's session tells the controller, on `legion-<project>-controller`
 *   (`packages/contracts/src/legion-roles.ts:108`);
 * - the controller's own session tells nobody: it is the owner, and its evidence is in its
 *   transcript and its log.
 *
 * **A session never publishes to the role it holds itself.** A sub-architect is a phase worker
 * whose own claim may be its issue's live architect, so that choice would hand the session its
 * own message; the tree's architect is told instead.
 */
export function stallNoticeTopic(input: StallNoticeInput): StallNotice {
  const { session, claim, project } = input;
  if (session.kind === "controller") {
    return {
      kind: "none",
      why: "the controller's own session is the owner, so its evidence is its transcript and its log",
    };
  }
  if (session.kind === "not-legion")
    return { kind: "none", why: "this session is not a Legion role" };
  if (project === undefined || !isLegionProjectToken(project)) {
    return {
      kind: "none",
      why: `LEGION_PROJECT is not a Legion project token (${project ?? "unset"})`,
    };
  }
  if (claim === undefined) return { kind: "none", why: "this session holds no claim yet" };
  if (session.kind === "root-architect") {
    return { kind: "topic", topic: roleTopic(controllerToken(project)) };
  }
  const own = roleToken(project, claim.issue, claim.role);
  const issueArchitect = roleToken(project, claim.issue, "architect");
  const treeArchitect = roleToken(project, claim.tree, "architect");
  const token = input.liveArchitect && issueArchitect !== own ? issueArchitect : treeArchitect;
  if (token === own) {
    return {
      kind: "none",
      why: "this session holds the only architect role its notice could reach",
    };
  }
  return { kind: "topic", topic: roleTopic(token) };
}

export interface StallTellerDeps {
  readonly now: () => number;
  /** This session's Legion identity (`classifySession`), which decides who is told. */
  readonly session: () => LegionSessionKind;
  /** This session's claim, or undefined before it has registered one. */
  readonly claim: () => ClaimCapability | undefined;
  /** `LEGION_PROJECT`. */
  readonly project: string | undefined;
  /** The daemon's whole state, read as `read_record` reads it; `signal` is aborted once the
   * record check's bound passes without an answer. */
  readonly readState: (signal: AbortSignal) => Promise<LegionState>;
  /** The Envoy listener the notice goes to, or undefined when Envoy is off for this session.
   * Built lazily and once by the caller, as the phase reminder's is. */
  readonly publisher: () => EnvoyPublish | undefined;
  /** Writes one record to the session's transcript (`pi.appendEntry`). */
  readonly appendEntry: (customType: string, data: unknown) => void;
  readonly log: StallWatchdogLog;
  /** The bound on one daemon read; `RECORD_READ_BOUND_MS` when it is left out. */
  readonly recordBoundMs?: number;
}

/**
 * The detector's teller: the evidence into the session's transcript, and one notice to the owner
 * per stuck call.
 *
 * **One message per stuck call.** The detector tells a call's owner once, before the first step,
 * and a sweep that finds the same call still open carries it a step further without telling
 * anyone again; this teller holds that answer by call id as well, so a second telling of the same
 * call publishes nothing whatever reaches it. A publish the listener refuses, and a session with
 * Envoy off, leave a line in the log and `ownerTold` false: the ladder goes on, because an owner
 * that cannot be reached is no reason to leave a worker stuck.
 *
 * **The ladder stopping short is a second thing, and it is told too.** `tellStopped` is the exit
 * cap, an unreadable counter file and unwritten evidence — the cases where the worker will not
 * take the step that would free it — so the owner hears about each of them once, and the "one
 * message per stuck call" rule above is about the overdue notice that opens the ladder.
 */
export function createStallTeller(deps: StallTellerDeps): StallWatchdogTeller {
  /** What each call's telling achieved, so a second telling of the same call repeats nothing. */
  const told = new Map<string, StallTold>();
  /** The stops already told, by call and reason. */
  const stopsTold = new Set<string>();

  const write = (record: StallWatchRecord): boolean => {
    try {
      deps.appendEntry(STALL_WATCH_ENTRY, record);
      return true;
    } catch (error) {
      deps.log.warn("legion: the stall watch could not write its record to the transcript", {
        toolCallId: record.toolCallId,
        kind: record.kind,
        error: messageFor(error),
      });
      return false;
    }
  };

  /**
   * Whether the issue's own architect claim is live, as the phase reminder reads it. Everything
   * fails open to the tree's architect and is logged at debug: an unreachable daemon is exactly
   * when a stuck worker must still be reported.
   */
  const liveArchitect = async (issue: string): Promise<boolean> => {
    try {
      const state = await withBound(deps.readState, deps.recordBoundMs ?? RECORD_READ_BOUND_MS);
      return architectLive(state.issues[issue]);
    } catch (error) {
      deps.log.debug("legion: the daemon did not answer the stall notice's record check", {
        issue,
        error: messageFor(error),
      });
      return false;
    }
  };

  /** One publish, and what it achieved. Nothing here throws: the ladder runs either way. */
  const publishNotice = async (
    notice: StallNotice,
    input: { readonly message: string; readonly payload: string; readonly key: string },
    context: Record<string, unknown>
  ): Promise<boolean> => {
    if (notice.kind === "none") {
      deps.log.warn("legion: nobody was told about this session's stuck tool call", {
        ...context,
        why: notice.why,
      });
      return false;
    }
    const claim = deps.claim();
    const publish = deps.publisher();
    if (publish === undefined || claim === undefined) {
      deps.log.warn("legion: Envoy is off for this session, so its owner was not told", {
        ...context,
        topic: notice.topic,
      });
      return false;
    }
    try {
      const answer = await publish({
        topic: notice.topic,
        message: input.message,
        // The listener refuses an object here: the facts travel as a JSON string.
        payload: input.payload,
        idempotencyKey: input.key,
        sourceSessionID: claim.sessionID,
        // This notice is sent once and never again, so a listener that keeps a copy for a role
        // nobody holds would have this worker record an owner as told that no owner was handed
        // it. The reminder sends `keep: false` for the same reason.
        keep: false,
      });
      if (answer.kept === true) {
        deps.log.warn("legion: the listener kept the stall notice; no owner has taken it", {
          ...context,
          topic: notice.topic,
        });
        return false;
      }
      return true;
    } catch (error) {
      deps.log.warn("legion: the owner was not told about this session's stuck tool call", {
        ...context,
        topic: notice.topic,
        error: messageFor(error),
      });
      return false;
    }
  };

  const noticeFor = async (): Promise<StallNotice> => {
    const session = deps.session();
    const claim = deps.claim();
    // Only a phase worker's notice depends on the daemon's record, so only it pays the read.
    const live =
      session.kind === "phase-worker" && claim !== undefined
        ? await liveArchitect(claim.issue)
        : false;
    try {
      return stallNoticeTopic({ session, claim, project: deps.project, liveArchitect: live });
    } catch (error) {
      // An issue key of a shape `roleToken` refuses is one owner not told, never a ladder that
      // stops: the evidence is written and the steps run either way.
      return {
        kind: "none",
        why: `this session's role topic could not be built: ${messageFor(error)}`,
      };
    }
  };

  return {
    tellOverdue: async (evidence: StallEvidence): Promise<StallTold> => {
      const { toolCallId } = evidence.call;
      const already = told.get(toolCallId);
      if (already !== undefined) {
        deps.log.debug("legion: this stuck call's owner has already been told", { toolCallId });
        return already;
      }
      const record = stallEvidenceRecord(evidence, deps.now(), {
        session: deps.session(),
        claim: deps.claim(),
      });
      const evidenceWritten = write(record);
      deps.log.warn("legion: a tool call of this session has not returned", {
        toolCallId,
        tool: record.tool,
        openMs: record.openMs,
        allowedMs: record.allowedMs,
        arguments: record.arguments,
        tree: record.tree.read ? record.tree.processes.length : record.tree.reason,
      });
      const ownerTold = await publishNotice(
        await noticeFor(),
        {
          message: stallNoticeMessage(record),
          payload: JSON.stringify(record),
          key: `legion-stall:${toolCallId}`,
        },
        { toolCallId, tool: record.tool }
      );
      const answer = { evidenceWritten, ownerTold };
      told.set(toolCallId, answer);
      return answer;
    },

    recordStep: ({ call, step, openMs, outcome }) => {
      write({
        kind: "step",
        at: new Date(deps.now()).toISOString(),
        toolCallId: call.toolCallId,
        tool: call.toolName,
        step,
        openMs,
        outcome,
      });
      deps.log.warn("legion: the stall watch carried a stuck tool call one step", {
        toolCallId: call.toolCallId,
        tool: call.toolName,
        step,
        openMs,
        outcome,
      });
    },

    tellStopped: async (call: StallCall, reason: string): Promise<void> => {
      write({
        kind: "stopped",
        at: new Date(deps.now()).toISOString(),
        toolCallId: call.toolCallId,
        tool: call.toolName,
        reason,
      });
      deps.log.warn("legion: the stall watch stopped short of a step", {
        toolCallId: call.toolCallId,
        tool: call.toolName,
        reason,
      });
      const key = `${call.toolCallId}:${reason}`;
      if (stopsTold.has(key)) return;
      stopsTold.add(key);
      const claim = deps.claim();
      const who = claim === undefined ? "A session" : `The ${claim.role} of ${claim.issue}`;
      await publishNotice(
        await noticeFor(),
        {
          message:
            `${who} has a \`${call.toolName}\` call it cannot recover from on its own: ${reason}. ` +
            "The evidence for the call is in that session's transcript.",
          payload: JSON.stringify({ toolCallId: call.toolCallId, tool: call.toolName, reason }),
          key: `legion-stall-stopped:${call.toolCallId}`,
        },
        { toolCallId: call.toolCallId, tool: call.toolName, reason }
      );
    },
  };
}
