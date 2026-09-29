#!/usr/bin/env bun
/** Single-line mutations of the write reader, measured against the same live canary rows.
 * Usage: nice -n 19 bun <this file> <absolute path to pane-guard.ts>
 * Each temporary guard stays beside its imports; the source guard is never modified. */
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  type GuardFactory,
  measureWriteRow,
  WRITE_ROWS,
  writeRefusalMatches,
} from "../src/legion/pane-guard-write-rows";

// [name, old line fragment, replacement on that line, optional preceding scope anchor].
const mutants: readonly (readonly [string, string, string, string?])[] = [
  ["option terminator", 'if (!rest && text !== "--") inspect?.(arg);', "inspect?.(arg);"],
  ["GNU separate value", "inspect === undefined ? text : longOption(text, longValued)", "text"],
  [
    "consumed separate value",
    "if (valueAt !== -1 && valueAt === text.length - 2 && valued.includes(last)) i += 1;",
    "if (false) i += 1;",
  ],
  [
    "mv target directory",
    "const targets = [...found.operands, ...writeDestinations(found, site)];",
    "const targets = [...found.operands];",
  ],
  [
    "cp directory contents",
    "checkCopyContents(found, destinations, st, ctx, site);",
    "void destinations;",
  ],
  ["install value grammar", 'writeArguments(rest, "mogtS", [', 'writeArguments(rest, "tS", ['],
  ["ln value grammar", 'writeArguments(rest, "St", [', 'writeArguments(rest, "t", ['],
  ["install directory flag", 'found.flags.has("d") || found.flags.has("--directory")', "false"],
  [
    "ln one operand",
    "found.operands.length === 1 && found.directory === undefined && !found.uncertain",
    "false",
  ],
  [
    "ln symbolic operands",
    'const symbolic = found.flags.has("s") || found.flags.has("--symbolic");',
    'const symbolic = rest.some((arg) => literalText(arg.exp)?.includes("s"));',
  ],
  [
    "long joined value",
    'return completeName(text.split("=", 1)[0] as string, names);',
    "return completeName(text, names);",
  ],
  ["long abbreviation", "name.startsWith(prefix)", "name === prefix"],
  ["unique completion", "reachable.length === 1 ? reachable[0] : undefined", "undefined"],
  [
    "unknown option",
    "if (!whole && mayBeOption(arg, () => false)) uncertain = arg;",
    "void whole;",
  ],
  [
    "operand is not a flag",
    'if (!text.startsWith("-") || text === "-") return;',
    'if (text === "-") return;',
  ],
  ["long flag record", "if (option !== undefined) flags.add(option);", "void option;"],
  [
    "long target offset",
    'if (option === "--target-directory") offset = text.indexOf("=") + 1 || text.length;',
    'if (option === "--target-directory") offset = text.length;',
  ],
  ["short flag record", "flags.add(letter);", "void letter;"],
  [
    "short flag before value",
    "if (!valued.includes(letter)) continue;",
    'if (letter !== "t") continue;',
  ],
  [
    "short target offset",
    'if (letter === "t") offset = index + 1;',
    'if (letter === "t") offset = index + 2;',
  ],
  ["stop cluster at value", "        break;", "        continue;", "function writeArguments("],
  [
    "ignore nontarget option",
    "if (offset === undefined) return;",
    "if (offset === undefined) offset = text.length;",
  ],
  ["attached target value", "const value = valueInWord(arg, offset);", "const value = undefined;"],
  [
    "separate target value",
    "directory = value === undefined ? rest[rest.indexOf(arg) + 1] : { text: arg.text, exp: value };",
    "directory = value === undefined ? undefined : { text: arg.text, exp: value };",
  ],
  [
    "no target directory",
    'found.flags.has("T") || found.flags.has("--no-target-directory")',
    "false",
  ],
  [
    "known target only",
    "if (found.directory !== undefined && !found.uncertain) return [found.directory];",
    "if (found.directory !== undefined && !found.uncertain) return [...found.operands, found.directory];",
  ],
  [
    "unreadable destination",
    "throw new Refusal(",
    "return []; throw new Refusal(",
    "function writeDestinations(",
  ],
  [
    "last destination",
    "  return [found.operands.at(-1) as Arg];",
    "  return [found.operands[0] as Arg];",
    "the guard cannot read a write destination from",
  ],
  [
    "directory test",
    "if (!statSync(dir, { throwIfNoEntry: false })?.isDirectory()) continue;",
    "continue;",
  ],
  [
    "exclude destination from sources",
    "if (destinations.includes(source)) continue;",
    "void source;",
  ],
  [
    "unknown source candidates",
    "if (!recursive && !(name === undefined && target === root)) continue;",
    "if (!recursive) continue;",
  ],
  [
    "source resolution",
    'const from = name === undefined ? undefined : path.resolve(st.cwd ?? "/", name);',
    'const from = name === undefined ? undefined : path.resolve("/", name);',
  ],
  [
    "written source basename",
    "path.basename(name)",
    "path.basename(path.resolve(name))",
    "function checkCopyContents(",
  ],
  [
    "untouched destination entries",
    "from !== undefined && statSync(from, { throwIfNoEntry: false })?.isDirectory()",
    "false",
  ],
  ["recursive descendants", "found.flags.has(flag)", "false", 'const recursive = ["r"'],
  ["descendant path", "const child = path.join(target, entry.name);", "const child = target;"],
  ["descendant target", "pending.push(child);", "void child;"],
  [
    "judge copy children",
    "exp: [literal(target)]",
    "exp: [literal(root)]",
    "function checkCopyContents(",
  ],
  [
    "sed in-place abbreviation",
    'if (option === "--in-place") inPlace = true;',
    'if (text === "--in-place") inPlace = true;',
  ],
  [
    "sed follow abbreviation",
    'if (option === "--follow-symlinks") follow = true;',
    'if (text === "--follow-symlinks") follow = true;',
  ],
  [
    "sed expression abbreviation",
    'if (option === "--expression" || option === "--file") {',
    'if (text === "--expression" || text === "--file") {',
  ],
  ["sed joined expression", 'if (!text.includes("=")) index += 1;', "index += 1;"],
  [
    "sed line length",
    'if (option === "--line-length" && !text.includes("=")) index += 1;',
    "void option;",
  ],
  [
    "minimum operands",
    "found.operands.length < 2 || found.uncertain",
    "found.operands.length < 1 || found.uncertain",
  ],
  ["cp short backup", 'found.flags.has("b")', "false"],
  ["cp long backup", 'found.flags.has("--backup")', "false"],
  ["cp replacement flag", 'found.flags.has("--remove-destination")', "false"],
  ["cp unknown replacement", "found.uncertain;", "false;", 'case "cp": {'],
  [
    "cp value grammar",
    'writeArguments(rest, "tS", [',
    'writeArguments(rest, "t", [',
    'case "cp": {',
  ],
  [
    "resolved destination directory",
    'const dir = realExisting(path.resolve(st.cwd ?? "/", text));',
    'const dir = realExisting(path.resolve("/", text));',
  ],
  ["direct child target", "const pending = [root];", "const pending: string[] = [];"],
  ["copy contents under -T", "direct || name === undefined", "name === undefined"],
  [
    "copy parents",
    'found.flags.has("--parents") ? name : path.basename(name)',
    "path.basename(name)",
  ],
  [
    "known source filtering",
    "if (knownTree && !existsSync(path.join(from, path.relative(root, child)))) continue;",
    "if (false) continue;",
  ],
  [
    "scan nested directories",
    "if (!lstatSync(target, { throwIfNoEntry: false })?.isDirectory()) continue;",
    "continue;",
  ],
  [
    "do not descend directory symlinks",
    "lstatSync(target, { throwIfNoEntry: false })",
    "statSync(target, { throwIfNoEntry: false })",
  ],
  [
    "skip absent destination subtree",
    "if (!lstatSync(target, { throwIfNoEntry: false })?.isDirectory()) continue;",
    "void target;",
  ],
  ["retain loop glob prefix", 'words[0]?.exp.some((piece) => piece.kind === "glob")', "false"],
  ["cp sparse value", '"--sparse",', '"--unused-sparse",', 'case "cp": {'],
  ["cp no-preserve value", '"--no-preserve",', '"--unused-no-preserve",', 'case "cp": {'],
  [
    "cp help",
    'if (found.flags.has("--help") || found.flags.has("--version")) return;',
    "void found;",
    'case "cp": {',
  ],
  [
    "mv help",
    'if (found.flags.has("--help") || found.flags.has("--version")) return;',
    "void found;",
    'case "mv": {',
  ],
  [
    "install help",
    'if (found.flags.has("--help") || found.flags.has("--version")) return;',
    "void found;",
    'case "install": {',
  ],
  [
    "ln help",
    'if (found.flags.has("--help") || found.flags.has("--version")) return;',
    "void found;",
    'case "ln": {',
  ],
  [
    "unreadable operand identity",
    'const word = found.uncertain?.text ?? found.operands.at(-1)?.text ?? "missing destination";',
    'const word = "destination";',
  ],
  [
    "unreadable operand remedy",
    "put \\`--\\` before operands, or use \\`-T\\`",
    "use explicit operands",
  ],
  [
    "directory entries share the syntax budget",
    "if (++ctx.steps.count > MAX_WALK_STEPS) {",
    "if (++ctx.steps.count > 1) {",
    "function checkCopyContents(",
  ],
  [
    "dd informational options",
    'if (whole && longOption(text, ["--help", "--version"]) !== undefined) return;',
    "void whole;",
  ],
  [
    "sed informational options",
    "return { inPlace: false, inferred: false, follow: false, files: [] };",
    "continue;",
  ],
];

const sourcePath = process.argv[2];
if (sourcePath === undefined) throw new Error("name the guard module");
const source = readFileSync(sourcePath, "utf8");
// The requested build and each freshly written mutant are runtime-selected modules.
const baseline = (await import(sourcePath)) as { createPaneGuard: GuardFactory };
const base = mkdtempSync(path.join(os.tmpdir(), "pane-write-mutants-"));
let survivors = 0;
try {
  const expected = WRITE_ROWS.map((row) => measureWriteRow(base, row, baseline.createPaneGuard));
  for (const [index, [name, from, to, scope]] of mutants.entries()) {
    const start = scope === undefined ? 0 : source.indexOf(scope);
    const at = source.indexOf(from, start);
    if (start < 0 || at < 0 || from.includes("\n") || to.includes("\n"))
      throw new Error(`bad single-line mutant: ${name}`);
    const modulePath = path.join(
      path.dirname(sourcePath),
      `.pane-guard-mutant-${process.pid}-${index}.ts`
    );
    try {
      writeFileSync(modulePath, source.slice(0, at) + to + source.slice(at + from.length));
      const mutant = (await import(modulePath)) as { createPaneGuard: GuardFactory };
      const killed: string[] = [];
      for (const result of expected) {
        try {
          const measured = measureWriteRow(base, result.row, mutant.createPaneGuard);
          if (
            (result.refusal === undefined) !== (measured.refusal === undefined) ||
            measured.error !== undefined ||
            !writeRefusalMatches(measured)
          )
            killed.push(
              result.row.name + (measured.error === undefined ? "" : ` (ERROR: ${measured.error})`)
            );
        } catch (error) {
          if (!(error instanceof Error) || !error.message.startsWith("fixture plant refused:"))
            throw error;
          killed.push(`must-allow plant for ${result.row.name}`);
        }
      }
      const line = source.slice(0, at).split("\n").length;
      console.log(
        `${killed.length > 0 ? "KILLED" : "SURVIVED"} L${line} ${name}: ${killed.join(" | ")}`
      );
      if (killed.length === 0) survivors += 1;
    } finally {
      rmSync(modulePath, { force: true });
    }
  }
  console.log(
    `${mutants.length - survivors}/${mutants.length} single-line mutants killed; survivors=${survivors}`
  );
} finally {
  rmSync(base, { recursive: true, force: true });
}
process.exitCode = survivors === 0 ? 0 : 1;
