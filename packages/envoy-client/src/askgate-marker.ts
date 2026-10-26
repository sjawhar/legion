import { createHash, randomBytes } from "node:crypto";
import {
  closeSync,
  constants,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { hasErrnoCode, messageFor } from "./errors";

/**
 * AskGate's enforcement at the posting `dispatch` CLI, shared by both sides.
 *
 * The record. While an Oh My Pi process whose gate is bound runs a session, pi-envoy keeps
 * `askgate.json` in that session's CLI state directory (`<root>/sessions/<id>/`): the owners, each
 * one process's id, its Linux start time (field 22 of `/proc/<pid>/stat`, null elsewhere) and its
 * marker store. An owner counts while its process lives and, on Linux, has that start time, so a
 * crashed session's record, or one whose pid another process took, is inert. A session no live
 * process records (a terminal, CI, Claude Code, OpenCode, a pod without pi-envoy, a session with
 * Dispatch or AskGate off) needs no marker.
 *
 * The marker. For each literal `dispatch` call it lets run, the gate mints one file in the store,
 * `<store>/markers/<sha256 of the argv>.<32 random hex>`, created owner-only and exclusively,
 * bound to the exact argument vector the CLI receives and, for a reviewed write, to the digest of
 * the arguments its dry run printed. Nothing of it is in the command the agent sees.
 *
 * The CLI. Before a gated write posts, with a live record, it consumes one matching marker by
 * unlinking it: exactly one process wins an unlink, so a marker posts one write and a replay finds
 * none. A record that is there but cannot be read, or names a store that is gone, is a refusal.
 * The store's `mode` file holds the gate's mode: `warn` lets an unmarked write post with a
 * warning, anything else refuses it.
 */

/** The record's name in a session's state directory. */
export const ASKGATE_RECORD = "askgate.json";
/**
 * How old a marker may be when the CLI consumes it. The gate releases each marker when its call
 * ends, so this only bounds one a call left behind: a backgrounded call's, or a killed session's.
 */
export const MARKER_MAX_AGE_MS = 10 * 60_000;

const PRIVATE_DIR = 0o700;
const PRIVATE_FILE = 0o600;
const MARKERS = "markers";
const MODE_FILE = "mode";

export type AskgateMode = "warn" | "block";

/** One process that keeps a session's record live. */
export interface AskgateOwner {
  readonly pid: number;
  /** Field 22 of `/proc/<pid>/stat`; null where there is no `/proc`. */
  readonly start: string | null;
  readonly store: string;
}

/** The Dispatch tools that always post text AskGate reviews. */
const ALWAYS_GATED: Record<string, true> = {
  dispatch_ask: true,
  dispatch_message: true,
  dispatch_edit_ask: true,
  dispatch_comment: true,
  dispatch_doc_edit: true,
  dispatch_request_approval: true,
};
const MARKDOWN_NAME = /\.md$/i;
const MARKDOWN_PATH = /\.(md|markdown)$/i;

/**
 * Whether an upload's arguments make a markdown document: inline text, or a name (as Dispatch
 * trims it) ending in `.md`, or a path ending in `.md` or `.markdown`, whose type Bun takes from it.
 */
export function uploadIsDocument(args: Readonly<Record<string, unknown>>): boolean {
  const { content, name, path } = args;
  return (
    typeof content === "string" ||
    (typeof name === "string" && MARKDOWN_NAME.test(name.trim())) ||
    (typeof path === "string" && MARKDOWN_PATH.test(path))
  );
}

/**
 * The one predicate of a gated write, the gate's and the CLI's: `ask`, `message`, `edit-ask`,
 * `comment`, `doc-edit` and `request-approval`, an `issue` with a spec, and an `artifact` that
 * uploads a markdown document.
 */
export function gatedWrite(tool: string, args: Readonly<Record<string, unknown>>): boolean {
  // `=== true`, so a name on Object's prototype (`constructor`) is no gated tool.
  if (ALWAYS_GATED[tool] === true) return true;
  const { spec } = args;
  if (tool === "dispatch_issue") return spec !== undefined;
  if (tool === "dispatch_artifact") return uploadIsDocument(args);
  return false;
}

const digest = (value: unknown): string =>
  createHash("sha256").update(JSON.stringify(value)).digest("hex");

/** The key of a marker: the digest of the words after the program, as the CLI receives them. */
export function argvDigest(argv: readonly string[]): string {
  return digest(argv);
}

/** The digest a reviewed marker binds: of the arguments the call's dry run printed. */
export function argsDigest(args: Readonly<Record<string, unknown>>): string {
  return digest(args);
}

/** Field 22 of `/proc/<pid>/stat`, the process's start in clock ticks since boot; null without one. */
export function processStart(pid: number): string | null {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
    // The name in parentheses may hold spaces and parentheses; the fields after it do not.
    return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19] ?? null;
  } catch {
    return null;
  }
}

/** `<root>/askgate/<pid>-<start>`: a store of its own for each process, never reused by a pid's next owner. */
export function askgateStore(root: string, pid: number, start: string | null): string {
  return join(root, "askgate", start === null ? String(pid) : `${pid}-${start}`);
}

/** This process as an owner, its store under the CLI state root `root`. */
export function selfOwner(root: string): AskgateOwner {
  const start = processStart(process.pid);
  return { pid: process.pid, start, store: askgateStore(root, process.pid, start) };
}

function ownerLive(owner: AskgateOwner): boolean {
  if (owner.start !== null) return processStart(owner.pid) === owner.start;
  try {
    process.kill(owner.pid, 0);
    return true;
  } catch (error) {
    // EPERM: a process of another user holds the pid, which is still a live process.
    return !hasErrnoCode(error, "ESRCH");
  }
}

function isOwner(value: unknown): value is AskgateOwner {
  if (typeof value !== "object" || value === null) return false;
  const { pid, start, store } = value as Record<string, unknown>;
  return (
    typeof pid === "number" &&
    Number.isSafeInteger(pid) &&
    pid > 0 &&
    (start === null || typeof start === "string") &&
    typeof store === "string" &&
    store.startsWith("/")
  );
}

/** The owners a record's text names, or undefined when it is no record. */
function parseRecord(text: string): AskgateOwner[] | undefined {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof data !== "object" || data === null) return undefined;
  const { v, owners } = data as Record<string, unknown>;
  if (v !== 1 || !Array.isArray(owners) || !owners.every(isOwner)) return undefined;
  return owners;
}

/** Writes `text` to `file` through an exclusive owner-only temporary file and `rename`. */
function writeAtomically(file: string, text: string): void {
  const temporary = `${file}.${randomBytes(8).toString("hex")}.tmp`;
  writeFileSync(temporary, text, { flag: "wx", mode: PRIVATE_FILE });
  renameSync(temporary, file);
}

/** The live owners of the record in `dir` other than `owner`; none when it is missing or no record. */
function otherLiveOwners(dir: string, owner: AskgateOwner): AskgateOwner[] {
  let text: string;
  try {
    text = readFileSync(join(dir, ASKGATE_RECORD), "utf-8");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return [];
    throw error;
  }
  return (parseRecord(text) ?? []).filter(
    (other) => !(other.pid === owner.pid && other.start === owner.start) && ownerLive(other)
  );
}

/** Makes `owner` one of the owners of session directory `dir`'s record, keeping the other live ones. */
export function writeAskgateRecord(dir: string, owner: AskgateOwner): void {
  mkdirSync(dir, { recursive: true, mode: PRIVATE_DIR });
  const owners = [...otherLiveOwners(dir, owner), owner];
  writeAtomically(join(dir, ASKGATE_RECORD), JSON.stringify({ v: 1, owners }));
}

/** Takes `owner` off session directory `dir`'s record, and removes the record it leaves with none. */
export function dropAskgateRecord(dir: string, owner: AskgateOwner): void {
  const owners = otherLiveOwners(dir, owner);
  if (owners.length === 0) rmSync(join(dir, ASKGATE_RECORD), { force: true });
  else writeAtomically(join(dir, ASKGATE_RECORD), JSON.stringify({ v: 1, owners }));
}

/** Makes a store and its markers directory, owner-only. */
export function openStore(store: string): void {
  mkdirSync(join(store, MARKERS), { recursive: true, mode: PRIVATE_DIR });
}

/** Removes a store and every marker in it. */
export function closeStore(store: string): void {
  rmSync(store, { recursive: true, force: true });
}

/** Records the gate's mode in its store, for the CLI to read. */
export function setStoreMode(store: string, mode: AskgateMode): void {
  openStore(store);
  writeAtomically(join(store, MODE_FILE), mode);
}

function storeMode(store: string): AskgateMode {
  try {
    return readFileSync(join(store, MODE_FILE), "utf-8") === "warn" ? "warn" : "block";
  } catch {
    return "block";
  }
}

/** What the record of a session's state directory requires of a gated write. */
export type AskgateState =
  | { readonly kind: "off" }
  | { readonly kind: "on"; readonly stores: readonly string[]; readonly mode: AskgateMode }
  | { readonly kind: "broken"; readonly problem: string };

/**
 * The state of session directory `dir`'s record: off when there is none or no owner lives, on with
 * each live owner's store (warn only when every one is in warn mode), broken when the record
 * cannot be read or a live owner's store is gone.
 */
export function askgateState(dir: string): AskgateState {
  const file = join(dir, ASKGATE_RECORD);
  let text: string;
  try {
    text = readFileSync(file, "utf-8");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return { kind: "off" };
    return { kind: "broken", problem: `${file}, could not be read: ${messageFor(error)}` };
  }
  const owners = parseRecord(text);
  if (owners === undefined) return { kind: "broken", problem: `${file}, is not an AskGate record` };
  const live = owners.filter(ownerLive);
  if (live.length === 0) return { kind: "off" };
  for (const { store } of live) {
    try {
      if (!statSync(join(store, MARKERS)).isDirectory()) throw new Error("not a directory");
    } catch (error) {
      return {
        kind: "broken",
        problem: `${file}, names the marker store ${store}, which cannot be read: ${messageFor(error)}`,
      };
    }
  }
  return {
    kind: "on",
    stores: live.map(({ store }) => store),
    mode: live.every(({ store }) => storeMode(store) === "warn") ? "warn" : "block",
  };
}

/** What one marker binds. `args` null: an argv-only marker, minted for a call no review stands behind. */
export interface MarkerInput {
  readonly argv: readonly string[];
  readonly args: Readonly<Record<string, unknown>> | null;
  readonly toolCallId: string;
  /** The gate's outcome for the call, or `subagent` for a task subagent's call. */
  readonly outcome: string;
}

interface Marker {
  readonly argv: string;
  readonly args: string | null;
  readonly createdAt: number;
}

/** Removes the markers and temporary files older than MARKER_MAX_AGE_MS from a markers directory. */
function sweepMarkers(dir: string, now: number): void {
  for (const name of readdirSync(dir)) {
    const file = join(dir, name);
    try {
      if (now - statSync(file).mtimeMs > MARKER_MAX_AGE_MS) unlinkSync(file);
    } catch {
      // Consumed or swept by another process since the listing.
    }
  }
}

/** Mints one marker in `store` and returns its path. Throws when the store cannot take it. */
export function mintMarker(store: string, input: MarkerInput, now: number): string {
  openStore(store);
  const dir = join(store, MARKERS);
  sweepMarkers(dir, now);
  const key = argvDigest(input.argv);
  const nonce = randomBytes(16).toString("hex");
  const temporary = join(dir, `.${nonce}.tmp`);
  writeFileSync(
    temporary,
    JSON.stringify({
      v: 1,
      argv: key,
      args: input.args === null ? null : argsDigest(input.args),
      toolCallId: input.toolCallId,
      outcome: input.outcome,
      createdAt: now,
    }),
    { flag: "wx", mode: PRIVATE_FILE }
  );
  // Whole or absent under its real name: a CLI never reads a marker half written.
  const file = join(dir, `${key}.${nonce}`);
  renameSync(temporary, file);
  return file;
}

/** A marker's binding, read without following a link; undefined when it is gone or malformed. */
function readMarker(file: string): Marker | undefined {
  let text: string;
  try {
    const fd = openSync(file, constants.O_RDONLY | constants.O_NOFOLLOW);
    try {
      text = readFileSync(fd, "utf-8");
    } finally {
      closeSync(fd);
    }
  } catch {
    return undefined;
  }
  try {
    const data: unknown = JSON.parse(text);
    if (typeof data !== "object" || data === null) return undefined;
    const { v, argv, args, createdAt } = data as Record<string, unknown>;
    if (v !== 1 || typeof argv !== "string" || typeof createdAt !== "number") return undefined;
    if (args !== null && typeof args !== "string") return undefined;
    return { argv, args, createdAt };
  } catch {
    return undefined;
  }
}

/** Whether the CLI may post: one marker consumed, or why none was. */
export type Consumed = { readonly ok: true } | { readonly ok: false; readonly why: string };

/**
 * Consumes one marker for `argv` from `stores`: a marker bound to `args` first, else an argv-only
 * one. A marker past MARKER_MAX_AGE_MS is removed and never used; one bound to other arguments is
 * left for the call it was minted for. A lost race for a marker (another process unlinked it
 * first) goes on to the next.
 */
export function consumeMarker(
  stores: readonly string[],
  argv: readonly string[],
  args: Readonly<Record<string, unknown>>,
  now: number
): Consumed {
  const key = argvDigest(argv);
  const reviewed = argsDigest(args);
  let mismatched = false;
  let expired = false;
  const usable: Array<{ file: string; bound: boolean }> = [];
  for (const store of stores) {
    const dir = join(store, MARKERS);
    for (const name of readdirSync(dir)) {
      if (!name.startsWith(`${key}.`)) continue;
      const file = join(dir, name);
      const marker = readMarker(file);
      if (marker === undefined || marker.argv !== key) continue;
      if (now - marker.createdAt > MARKER_MAX_AGE_MS) {
        expired = true;
        rmSync(file, { force: true });
        continue;
      }
      if (marker.args !== null && marker.args !== reviewed) {
        mismatched = true;
        continue;
      }
      usable.push({ file, bound: marker.args !== null });
    }
  }
  usable.sort((a, b) => Number(b.bound) - Number(a.bound));
  for (const { file } of usable) {
    try {
      unlinkSync(file);
      return { ok: true };
    } catch (error) {
      if (!hasErrnoCode(error, "ENOENT")) throw error;
    }
  }
  return {
    ok: false,
    why: mismatched
      ? "its arguments differ from the ones AskGate reviewed"
      : expired
        ? "its AskGate marker expired before it ran"
        : "this call carries no AskGate marker",
  };
}
