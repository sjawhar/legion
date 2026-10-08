import { createHash } from "node:crypto";
import {
  appendFileSync,
  closeSync,
  type Dirent,
  fstatSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { adviceMemory } from "./dispatch-execute";
import { shownPictures } from "./dispatch-picture-tools";
import type { PictureType, ToolImage } from "./dispatch-pictures";

/**
 * The `dispatch` CLI's state, one directory per host session under
 * `<root>/sessions/<session id>`: what a long-lived host kept in memory across calls (the triage
 * lines and pictures already shown, the asks whose follow notice was printed) in `state.json`, a
 * ledger of every call's result in `results.jsonl`, and the pictures and long results the CLI
 * wrote for the agent to open. The state names issue keys and picture addresses, so every file
 * is the owner's alone.
 *
 * Two calls racing in one session can lose a memory entry; that costs one repeated line.
 */

export interface ResultEntry {
  readonly at: string;
  readonly tool: string;
  readonly details?: unknown;
  readonly error?: string;
}

interface SessionMemory {
  readonly advice: readonly string[];
  readonly pictures: readonly string[];
  readonly follows: readonly string[];
}

const SESSION_ID = /^[A-Za-z0-9._-]{1,128}$/;
const PRIVATE_DIR = 0o700;
const PRIVATE_FILE = 0o600;
const IDLE_SESSION_MS = 14 * 86_400_000;
const PRUNE_INTERVAL_MS = 86_400_000;
const PRUNED_STAMP = ".pruned";

const PICTURE_EXTENSIONS: Record<PictureType, string> = {
  "image/png": "png",
  "image/jpeg": "jpg",
  "image/gif": "gif",
  "image/webp": "webp",
};

/** `DISPATCH_STATE_DIR`, else `<XDG_STATE_HOME or ~/.local/state>/dispatch`. Empty counts as unset. */
export function sessionStateRoot(env: Readonly<Record<string, string | undefined>>): string {
  if (env.DISPATCH_STATE_DIR) return env.DISPATCH_STATE_DIR;
  return join(env.XDG_STATE_HOME || join(homedir(), ".local", "state"), "dispatch");
}

/** `<root>/sessions/<id>`; an id that is not one plain path segment is refused. */
export function sessionDirectory(
  env: Readonly<Record<string, string | undefined>>,
  sessionId: string
): string {
  if (!SESSION_ID.test(sessionId) || sessionId === "." || sessionId === "..") {
    throw new Error(
      `dispatch: the session id ${JSON.stringify(sessionId)} is not 1-128 letters, digits, ".", "_" or "-"`
    );
  }
  return join(sessionStateRoot(env), "sessions", sessionId);
}

function ensureDirectory(dir: string): void {
  mkdirSync(dir, { recursive: true, mode: PRIVATE_DIR });
}

/** Whether a filesystem call failed because the path is not there. */
function isMissing(error: unknown): boolean {
  return (error as NodeJS.ErrnoException | null)?.code === "ENOENT";
}

/** A file's text, or undefined when it is not there. */
function readIfPresent(path: string): string | undefined {
  try {
    return readFileSync(path, "utf-8");
  } catch (error) {
    if (isMissing(error)) return undefined;
    throw error;
  }
}

/** Writes `text` to `path` through a temporary file and `rename`, so a reader sees all or none. */
function writeAtomically(path: string, text: string): void {
  const temporary = `${path}.${process.pid}.${Date.now()}.tmp`;
  writeFileSync(temporary, text, { mode: PRIVATE_FILE });
  renameSync(temporary, path);
}

function stringList(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : [];
}

/**
 * Reads `state.json`, seeds the advice and picture memories the library consults, and returns the
 * asks this session already printed a follow notice for. A missing file is an empty memory; one
 * that is not JSON throws, naming it.
 */
export function loadSessionMemory(dir: string, sessionId: string): Set<string> {
  const path = join(dir, "state.json");
  const text = readIfPresent(path);
  if (text === undefined) return new Set();
  let state: Partial<Record<keyof SessionMemory, unknown>>;
  try {
    state = JSON.parse(text);
  } catch (error) {
    throw new Error(
      `the session state ${path} is not JSON (${(error as Error).message}); remove it to start this session's memory over`
    );
  }
  const advice = adviceMemory();
  for (const key of stringList(state.advice)) advice.add(key);
  const pictures = shownPictures(sessionId);
  for (const address of stringList(state.pictures)) pictures.add(address);
  return new Set(stringList(state.follows));
}

/** Writes the advice and picture memories and `follows` back to `state.json`. */
export function saveSessionMemory(dir: string, sessionId: string, follows: Set<string>): void {
  ensureDirectory(dir);
  writeMemory(dir, sessionId, follows);
}

function writeMemory(dir: string, sessionId: string, follows: Set<string>): void {
  const state: SessionMemory = {
    advice: [...adviceMemory()],
    pictures: [...shownPictures(sessionId)],
    follows: [...follows],
  };
  writeAtomically(join(dir, "state.json"), `${JSON.stringify(state)}\n`);
}

type ResultInput = { readonly tool: string; readonly details?: unknown; readonly error?: string };

/** Appends one call's result, with `at` added, to the ledger as one JSON line. */
export function appendResult(dir: string, entry: ResultInput): void {
  ensureDirectory(dir);
  writeResult(dir, entry);
}

function writeResult(dir: string, entry: ResultInput): void {
  const line: ResultEntry = { at: new Date().toISOString(), ...entry };
  appendFileSync(join(dir, "results.jsonl"), `${JSON.stringify(line)}\n`, { mode: PRIVATE_FILE });
}

/** What one call leaves behind: its ledger line, then the session's memory. */
export function recordOutcome(
  dir: string,
  sessionId: string,
  follows: Set<string>,
  entry: ResultInput
): void {
  ensureDirectory(dir);
  writeResult(dir, entry);
  writeMemory(dir, sessionId, follows);
}

/**
 * The ledger entries appended after byte `offset`, and the offset that follows them. A trailing
 * line still being written is left for the next read. A complete line that is not JSON (two
 * processes' appends interleaved, a truncated write) is skipped and named to `onMalformed` with the
 * file and its line number, and the offset moves past it, so one bad line never stalls a reader.
 */
export function readResultsSince(
  dir: string,
  offset: number,
  onMalformed: (problem: string) => void
): { entries: ResultEntry[]; offset: number } {
  const path = join(dir, "results.jsonl");
  let fd: number;
  try {
    fd = openSync(path, "r");
  } catch (error) {
    if (isMissing(error)) return { entries: [], offset };
    throw error;
  }
  let buffer: Buffer;
  try {
    const size = fstatSync(fd).size;
    if (size <= offset) return { entries: [], offset: size };
    buffer = Buffer.alloc(size - offset);
    readSync(fd, buffer, 0, buffer.length, offset);
  } finally {
    closeSync(fd);
  }
  const complete = buffer.lastIndexOf(0x0a) + 1;
  const lines = buffer.subarray(0, complete).toString("utf-8").split("\n");
  const entries: ResultEntry[] = [];
  let firstLine: number | undefined;
  lines.forEach((line, index) => {
    if (line === "") return;
    try {
      entries.push(JSON.parse(line) as ResultEntry);
    } catch (error) {
      firstLine ??= linesBefore(path, offset) + 1;
      onMalformed(
        `${path}:${firstLine + index}: skipped a ledger line that is not JSON (${(error as Error).message})`
      );
    }
  });
  return { entries, offset: offset + complete };
}

/** How many lines end before byte `offset`: read only to name a malformed line. */
function linesBefore(path: string, offset: number): number {
  const prefix = Buffer.alloc(offset);
  const fd = openSync(path, "r");
  try {
    readSync(fd, prefix, 0, offset, 0);
  } finally {
    closeSync(fd);
  }
  let count = 0;
  for (const byte of prefix) if (byte === 0x0a) count += 1;
  return count;
}

/** The ledger's size in bytes, 0 when there is none: a reader starting there sees only later calls. */
export function resultsEnd(dir: string): number {
  try {
    return statSync(join(dir, "results.jsonl")).size;
  } catch (error) {
    if (isMissing(error)) return 0;
    throw error;
  }
}

/** Writes a picture under `pictures/<sha256 first 16 hex>.<ext>`, once; returns its path and size. */
export function writePicture(dir: string, image: ToolImage): { path: string; bytes: number } {
  const bytes = Buffer.from(image.data, "base64");
  const name = createHash("sha256").update(bytes).digest("hex").slice(0, 16);
  const pictures = join(dir, "pictures");
  const path = join(pictures, `${name}.${PICTURE_EXTENSIONS[image.mimeType]}`);
  ensureDirectory(pictures);
  try {
    writeFileSync(path, bytes, { mode: PRIVATE_FILE, flag: "wx" });
  } catch (error) {
    // The same bytes, already written under their hash.
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
  }
  return { path, bytes: bytes.length };
}

/** Writes a result too long for the host to show to `out/<timestamp>.md`; returns the path. */
export function writeLongOutput(dir: string, text: string): string {
  const out = join(dir, "out");
  ensureDirectory(out);
  const path = join(out, `${new Date().toISOString().replaceAll(":", "-")}-${process.pid}.md`);
  writeFileSync(path, text, { mode: PRIVATE_FILE });
  return path;
}

/** Writes the session's title to `title`, atomically: a concurrent read sees the old or the new. */
export function writeSessionTitle(dir: string, title: string): void {
  ensureDirectory(dir);
  writeAtomically(join(dir, "title"), title);
}

/** The session's title, or undefined when the file is missing or empty. */
export function readSessionTitle(dir: string): string | undefined {
  return readIfPresent(join(dir, "title")) || undefined;
}

/**
 * Removes the session directories nobody touched for 14 days, at most once a day: the root's
 * `.pruned` stamp records the last pass.
 */
export function pruneSessions(root: string, now: number): void {
  const stamp = join(root, PRUNED_STAMP);
  try {
    if (now - statSync(stamp).mtimeMs < PRUNE_INTERVAL_MS) return;
  } catch (error) {
    if (!isMissing(error)) throw error;
  }
  const sessions = join(root, "sessions");
  let entries: Dirent[] = [];
  try {
    entries = readdirSync(sessions, { withFileTypes: true });
  } catch (error) {
    if (!isMissing(error)) throw error;
  }
  for (const entry of entries) {
    if (!entry.isDirectory()) continue;
    const dir = join(sessions, entry.name);
    try {
      if (now - statSync(dir).mtimeMs > IDLE_SESSION_MS) {
        rmSync(dir, { recursive: true, force: true });
      }
    } catch (error) {
      // Another process removed it since the listing.
      if (!isMissing(error)) throw error;
    }
  }
  ensureDirectory(root);
  writeFileSync(stamp, "", { mode: PRIVATE_FILE });
}
