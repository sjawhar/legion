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
import { isAbsolute, join, resolve } from "node:path";
import { hasErrnoCode, messageFor } from "./errors";

/**
 * AskGate's enforcement at the posting `dispatch` CLI, shared by both sides.
 *
 * The record. While an Oh My Pi process whose gate is bound runs a session, pi-envoy keeps an
 * owner file in the `askgate/` directory of that session's CLI state directory
 * (`<root>/sessions/<id>/askgate/<pid>-<start>.json`): the process's id, its Linux start time
 * (field 22 of `/proc/<pid>/stat`, null elsewhere) and its marker store. Each process writes and
 * removes only its own file, so two processes that bind one session at the same time never drop
 * each other. An owner counts while its process lives and, on Linux, has that start time, so a
 * crashed session's file, or one whose pid another process took, is inert. A session no live
 * process records (a terminal, CI, Claude Code, OpenCode, a pod without pi-envoy, a session with
 * Dispatch or AskGate off) needs no marker.
 *
 * The marker. For each literal `dispatch` call it lets run, the gate mints one file in the store,
 * `<store>/markers/<sha256 of the argv>.<32 random hex>`, created owner-only and exclusively,
 * bound to the exact argument vector the CLI receives and, for a reviewed write, to the digest of
 * the arguments its dry run printed and to the sha256 of each file the argv names
 * (`fileOperands`), as the gate read it for the review. Nothing of it is in the command the agent
 * sees.
 *
 * The CLI. Before a gated write posts, with a live record, it reads each file the argv names once,
 * and consumes one matching marker by unlinking it: exactly one process wins an unlink, so a
 * marker posts one write and a replay finds none. A file whose bytes differ from the reviewed ones
 * matches no marker. An upload's `--path` sends the bytes the CLI read for that check. A record
 * that is there but cannot be read, or names a store that is gone, is a refusal. The store's
 * `mode` file holds the gate's mode: `warn` lets an unmarked write post with a warning, anything
 * else refuses it.
 */

/** The record's directory in a session's state directory: one file for each owner. */
export const ASKGATE_RECORD = "askgate";
/**
 * How old a marker may be when the CLI consumes it. The gate mints a marker when it prepares the
 * calls of a message, and the call can then wait behind an exclusive call of the same message for
 * up to that call's limit (an eval cell's is 3,600 s), so this is above the longest tool limit. The
 * gate releases each marker when its call ends, so this only bounds one a call left behind: a
 * backgrounded call's, or a killed session's.
 */
export const MARKER_MAX_AGE_MS = 2 * 60 * 60_000;

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

/** One file a `dispatch` call makes the CLI read, as its argv names it. */
export interface FileOperand {
  /** The flag that names it: a `--<field>-file`, or an upload's `--path`. */
  readonly flag: string;
  /** The operand as written. */
  readonly operand: string;
}

/** One file of a reviewed call: its operand, and the sha256 of its bytes; null when it was not read. */
export interface FileDigest extends FileOperand {
  readonly sha256: string | null;
}

/**
 * The files a `dispatch` argv (the words after the program) makes the CLI read, in argv order: the
 * operand of each `--<field>-file` but `-` (standard input), and an `artifact` upload's `--path`.
 * A flag's operand is the word after it, or the text after its `=`. The gate and the CLI both use
 * this list, so the digests a marker binds and the ones the CLI computes name the same files.
 */
export function fileOperands(argv: readonly string[]): FileOperand[] {
  const [command, ...flags] = argv;
  const files: FileOperand[] = [];
  for (let index = 0; index < flags.length; index++) {
    const word = flags[index] as string;
    const eq = word.indexOf("=");
    const flag = eq === -1 ? word : word.slice(0, eq);
    const named = flag.startsWith("--") && flag.endsWith("-file");
    if (!named && !(command === "artifact" && flag === "--path")) continue;
    const operand = eq === -1 ? flags[index + 1] : word.slice(eq + 1);
    if (operand !== undefined && operand !== "-") files.push({ flag, operand });
  }
  return files;
}

/**
 * The path the CLI opens for a file operand, from its working directory `cwd`. A `--<field>-file`
 * goes to `readFileSync` as written, so the kernel resolves it and a `..` after a symbolic link
 * leaves the link's target; an upload's `--path` goes through `resolve` (dispatch-execute.ts),
 * which removes a `..` first.
 */
export function operandPath(cwd: string, file: FileOperand): string {
  if (file.flag === "--path") return resolve(cwd, file.operand);
  return isAbsolute(file.operand) ? file.operand : `${cwd}/${file.operand}`;
}

/** The sha256 of a file's bytes, as a marker binds it. */
export function bytesDigest(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
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

/** `<pid>-<start>`, or `<pid>` with no start time: an owner's name, for its store and its file. */
function ownerName(pid: number, start: string | null): string {
  return start === null ? String(pid) : `${pid}-${start}`;
}

/** `<root>/askgate/<pid>-<start>`: a store of its own for each process, never reused by a pid's next owner. */
export function askgateStore(root: string, pid: number, start: string | null): string {
  return join(root, "askgate", ownerName(pid, start));
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

/** The owner an owner file's text names, or undefined when it is no owner file. */
function parseOwner(text: string): AskgateOwner | undefined {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (!isOwner(data) || !("v" in data) || data.v !== 1) return undefined;
  const { pid, start, store } = data;
  return { pid, start, store };
}

/** Writes `text` to `file` through an exclusive owner-only temporary file and `rename`. */
function writeAtomically(file: string, text: string): void {
  const temporary = `${file}.${randomBytes(8).toString("hex")}.tmp`;
  writeFileSync(temporary, text, { flag: "wx", mode: PRIVATE_FILE });
  renameSync(temporary, file);
}

const OWNER_FILE = ".json";

/** `owner`'s own file in session directory `dir`'s record. */
function ownerFile(dir: string, owner: AskgateOwner): string {
  return join(dir, ASKGATE_RECORD, `${ownerName(owner.pid, owner.start)}${OWNER_FILE}`);
}

/**
 * Makes `owner` an owner of session directory `dir`'s record by writing its own file, and removes
 * the files of owners whose process is gone. A gone owner with a start time never writes again,
 * since no later process has its pid and start time; one with none is left, as its pid can return.
 */
export function writeAskgateRecord(dir: string, owner: AskgateOwner): void {
  const record = join(dir, ASKGATE_RECORD);
  mkdirSync(record, { recursive: true, mode: PRIVATE_DIR });
  const own = ownerFile(dir, owner);
  writeAtomically(
    own,
    JSON.stringify({ v: 1, pid: owner.pid, start: owner.start, store: owner.store })
  );
  for (const name of readdirSync(record)) {
    const file = join(record, name);
    if (file === own || !name.endsWith(OWNER_FILE)) continue;
    let other: AskgateOwner | undefined;
    try {
      other = parseOwner(readFileSync(file, "utf-8"));
    } catch {
      continue;
    }
    if (other !== undefined && other.start !== null && !ownerLive(other)) {
      rmSync(file, { force: true });
    }
  }
}

/**
 * Takes `owner` off session directory `dir`'s record by removing its own file. The directory
 * stays: an empty one names no owner, and removing it could fail another process's write there.
 */
export function dropAskgateRecord(dir: string, owner: AskgateOwner): void {
  rmSync(ownerFile(dir, owner), { force: true });
}

/** Makes a store and its markers directory, owner-only. */
export function openStore(store: string): void {
  mkdirSync(join(store, MARKERS), { recursive: true, mode: PRIVATE_DIR });
}

/** Removes a store and every marker in it. */
export function closeStore(store: string): void {
  rmSync(store, { recursive: true, force: true });
}

/** A store's name under `<root>/askgate/`, as `askgateStore` makes it: `<pid>-<start>`, or `<pid>`. */
const STORE_NAME = /^([1-9][0-9]*)(?:-([0-9]+))?$/;

/** The last change to a store: its own or its markers directory's, whichever is later. */
function storeTouched(store: string): number {
  const own = statSync(store).mtimeMs;
  try {
    return Math.max(own, statSync(join(store, MARKERS)).mtimeMs);
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return own;
    throw error;
  }
}

/**
 * Removes each store under `<root>/askgate/` whose process is gone (a crashed process's, whose
 * markers no CLI reads) and that nothing changed for MARKER_MAX_AGE_MS. The age also keeps the
 * store of a process this one cannot see, as in another pid namespace, while that process mints.
 * A store it cannot remove stays: this is tidiness, so it never stops a call.
 */
export function sweepStores(root: string, now: number): void {
  const parent = join(root, "askgate");
  let names: string[];
  try {
    names = readdirSync(parent);
  } catch {
    return;
  }
  for (const name of names) {
    const match = STORE_NAME.exec(name);
    if (match === null) continue;
    const store = join(parent, name);
    try {
      if (ownerLive({ pid: Number(match[1]), start: match[2] ?? null, store })) continue;
      if (now - storeTouched(store) <= MARKER_MAX_AGE_MS) continue;
      rmSync(store, { recursive: true, force: true });
    } catch {
      // Gone since the listing, or not this user's to remove.
    }
  }
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
 * each live owner's store (warn only when every one is in warn mode), broken when the record or an
 * owner file cannot be read or a live owner's store is gone.
 */
export function askgateState(dir: string): AskgateState {
  const record = join(dir, ASKGATE_RECORD);
  let names: string[];
  try {
    names = readdirSync(record);
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return { kind: "off" };
    return { kind: "broken", problem: `${record}, could not be read: ${messageFor(error)}` };
  }
  const live: { readonly file: string; readonly store: string }[] = [];
  for (const name of names) {
    // Not an owner file: a temporary file that its write's `rename` has yet to make one.
    if (!name.endsWith(OWNER_FILE)) continue;
    const file = join(record, name);
    let text: string;
    try {
      text = readFileSync(file, "utf-8");
    } catch (error) {
      // Its owner removed it after the listing.
      if (hasErrnoCode(error, "ENOENT")) continue;
      return { kind: "broken", problem: `${file}, could not be read: ${messageFor(error)}` };
    }
    const owner = parseOwner(text);
    if (owner === undefined)
      return { kind: "broken", problem: `${file}, is not an AskGate record` };
    if (ownerLive(owner)) live.push({ file, store: owner.store });
  }
  if (live.length === 0) return { kind: "off" };
  for (const { file, store } of live) {
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

/**
 * What one marker binds. `args` null: an argv-only marker, minted for a call no review stands
 * behind, which binds no file either.
 */
export interface MarkerInput {
  readonly argv: readonly string[];
  /** The arguments the call's dry run printed. */
  readonly args: Readonly<Record<string, unknown>> | null;
  /** Each file the argv names (`fileOperands`), with the digest of the bytes AskGate reviewed. */
  readonly files: readonly FileDigest[];
  readonly toolCallId: string;
  /** The gate's outcome for the call, or `subagent` for a task subagent's call. */
  readonly outcome: string;
}

interface Marker {
  readonly argv: string;
  readonly args: string | null;
  readonly files: readonly FileDigest[];
  readonly createdAt: number;
}

const isFileDigest = (value: unknown): value is FileDigest =>
  typeof value === "object" &&
  value !== null &&
  typeof (value as FileDigest).flag === "string" &&
  typeof (value as FileDigest).operand === "string" &&
  typeof (value as FileDigest).sha256 === "string";

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
      v: 2,
      argv: key,
      args: input.args === null ? null : argsDigest(input.args),
      files: input.args === null ? [] : input.files,
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
    const { v, argv, args, files, createdAt } = data as Record<string, unknown>;
    if (v !== 2 || typeof argv !== "string" || typeof createdAt !== "number") return undefined;
    if (args !== null && typeof args !== "string") return undefined;
    if (!Array.isArray(files) || !files.every(isFileDigest)) return undefined;
    return { argv, args, files, createdAt };
  } catch {
    return undefined;
  }
}

/**
 * Whether the CLI may post: one marker consumed, or why none was. `changed` names the first file
 * whose bytes differ from the ones AskGate reviewed, when that is why.
 */
export type Consumed =
  | { readonly ok: true }
  | { readonly ok: false; readonly why: string; readonly changed?: FileOperand };

/**
 * Consumes one marker for `argv` from `stores`: a marker bound to `args` and to `files` (each file
 * the argv names, as the CLI read it) first, else an argv-only one. A marker past
 * MARKER_MAX_AGE_MS is removed and never used; one bound to other files or other arguments is left
 * for the call it was minted for. A lost race for a marker (another process unlinked it first)
 * goes on to the next.
 */
export function consumeMarker(
  stores: readonly string[],
  argv: readonly string[],
  args: Readonly<Record<string, unknown>>,
  files: readonly FileDigest[],
  now: number
): Consumed {
  const key = argvDigest(argv);
  const reviewed = argsDigest(args);
  let changed: FileOperand | undefined;
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
      if (marker.args !== null) {
        // The argv keys the marker, so both lists name the same files; a file whose bytes differ
        // says more than the arguments it changed with it.
        const differs = marker.files.find(
          (bound, index) =>
            bound.flag !== files[index]?.flag ||
            bound.operand !== files[index]?.operand ||
            bound.sha256 !== files[index]?.sha256
        );
        if (differs !== undefined) {
          changed ??= { flag: differs.flag, operand: differs.operand };
          continue;
        }
        if (marker.files.length !== files.length || marker.args !== reviewed) {
          mismatched = true;
          continue;
        }
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
  if (changed !== undefined)
    return {
      ok: false,
      why: `a file this write posts changed after AskGate reviewed it (\`${changed.flag} ${changed.operand}\`)`,
      changed,
    };
  return {
    ok: false,
    why: mismatched
      ? "its arguments differ from the ones AskGate reviewed"
      : expired
        ? "its AskGate marker expired before it ran"
        : "this call carries no AskGate marker",
  };
}
