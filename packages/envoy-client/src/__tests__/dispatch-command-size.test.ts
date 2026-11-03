import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

// Where the command dispatcher lives, and where its handlers live now that it is split: the entry
// validates the call, builds one context and calls the handler its table names, and every
// command's body is a module under `dispatch-commands/`. This file holds the bound on both.
const SOURCE = path.join(import.meta.dir, "..");
const ENTRY = path.join(SOURCE, "dispatch-execute.ts");
const HANDLERS = path.join(SOURCE, "dispatch-commands");

/**
 * How long `executeDispatchTool` is now the split has landed: 23 lines, against the 1,668 of the
 * one function that held a 24-arm switch. The test below asserts the bound rather than this
 * number, because a handler moved into or out of the entry is a change to review, not a test to
 * re-record; the number says what the entry costs today.
 */
const ENTRY_LINES_BASE = 23;

/**
 * How long `dispatch-execute.ts` is now, counted as its text's lines rather than as its newline
 * count, so a file with a final newline counts the empty line after it: 60 lines, against the
 * 5,105 it held before the split. It is the entry and the re-exports its callers import, so it is
 * held to the bound every module of the split is held to, for the same reason.
 */
const FILE_LINES_BASE = 60;

/** What the acceptance asks of the split: a small entry, and no module of it longer than this. */
const ENTRY_BOUND = 100;
const MODULE_BOUND = 1000;

/** The lines of `declaration`'s body in `source`, counted from its first line to its closing brace. */
function declarationLines(source: string, declaration: string): number {
  const lines = source.split("\n");
  const start = lines.findIndex((line) => line.startsWith(declaration));
  if (start === -1) throw new Error(`${declaration} is not declared at the head of a line`);
  const end = lines.findIndex((line, index) => index > start && line === "}");
  if (end === -1) throw new Error(`${declaration} has no closing brace at the head of a line`);
  return end - start + 1;
}

/** Every `.ts` file under `root`, at any depth; an empty list where `root` does not exist. */
function sourceFiles(root: string): string[] {
  let entries: string[];
  try {
    entries = readdirSync(root);
  } catch {
    return [];
  }
  return entries.flatMap((entry) => {
    const full = path.join(root, entry);
    if (statSync(full).isDirectory()) return sourceFiles(full);
    return full.endsWith(".ts") ? [full] : [];
  });
}

describe("the size of the command dispatcher", () => {
  test("executeDispatchTool is an entry no longer than the bound", () => {
    const lines = declarationLines(
      readFileSync(ENTRY, "utf8"),
      "export async function executeDispatchTool("
    );
    expect(
      lines,
      `executeDispatchTool is ${lines} lines; it was ${ENTRY_LINES_BASE} when the split landed`
    ).toBeLessThanOrEqual(ENTRY_BOUND);
  });

  test("dispatch-execute.ts is no longer than the bound", () => {
    const lines = readFileSync(ENTRY, "utf8").split("\n").length;
    expect(
      lines,
      `dispatch-execute.ts is ${lines} lines; it was ${FILE_LINES_BASE} when the split landed`
    ).toBeLessThanOrEqual(MODULE_BOUND);
  });

  // The split left 20 modules under `dispatch-commands/`, the longest of them `documents.ts` at
  // 642 lines, so this walk reads real files where it used to read an empty directory.
  test("no handler module is longer than the bound", () => {
    const tooLong = sourceFiles(HANDLERS)
      .map((file) => ({
        file: path.relative(SOURCE, file),
        lines: readFileSync(file, "utf8").split("\n").length,
      }))
      .filter(({ lines }) => lines > MODULE_BOUND);
    expect(tooLong).toEqual([]);
  });
});
