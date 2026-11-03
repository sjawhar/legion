import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

// Where the command dispatcher lives, and where its handlers will live once it is split. The
// directory does not exist yet; the walk below answers an empty list for a directory that is not
// there, so this file holds the bound from the commit before the split to every commit after it.
const SOURCE = path.join(import.meta.dir, "..");
const ENTRY = path.join(SOURCE, "dispatch-execute.ts");
const HANDLERS = path.join(SOURCE, "dispatch-commands");

/**
 * How long `executeDispatchTool` is today: one function holding a 24-arm switch, every command's
 * body inside it. The split lowers this to at most ENTRY_BOUND, so this literal changes with it.
 */
const ENTRY_LINES_BASE = 1668;

/**
 * How long `dispatch-execute.ts` is today, counted as its text's lines rather than as its newline
 * count, so a file with a final newline counts the empty line after it. The split moves each
 * command's body into a module of its own under `dispatch-commands/`, so this literal changes
 * with it too.
 */
const FILE_LINES_BASE = 5105;

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
  test("executeDispatchTool is one function of the length the base holds", () => {
    const lines = declarationLines(
      readFileSync(ENTRY, "utf8"),
      "export async function executeDispatchTool("
    );
    expect(lines).toBe(ENTRY_LINES_BASE);
    expect(ENTRY_LINES_BASE).toBeGreaterThan(ENTRY_BOUND);
  });

  test("dispatch-execute.ts is the length the base holds", () => {
    const lines = readFileSync(ENTRY, "utf8").split("\n").length;
    expect(lines).toBe(FILE_LINES_BASE);
  });

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
