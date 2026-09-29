#!/usr/bin/env bun
/** Single-line mutations of the write reader, measured against the same live canary rows.
 * Usage: nice -n 19 bun <this file> <absolute path to pane-guard.ts>
 * Each temporary guard stays beside its imports; the source guard is never modified. */
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { type GuardFactory, measureWriteRow, WRITE_ROWS } from "../src/legion/pane-guard-write-rows";

// [name, old line fragment, replacement on that line, optional preceding scope anchor].
const mutants: readonly (readonly [string, string, string, string?])[] = [
  ["option terminator", 'if (!rest && text !== "--") inspect?.(arg);', 'inspect?.(arg);'],
  ["GNU separate value", 'inspect === undefined ? text : longOption(text, longValued)', 'text'],
  ["consumed separate value", 'if (valueAt !== -1 && valueAt === text.length - 2 && valued.includes(last)) i += 1;', 'if (false) i += 1;'],
  ["mv target directory", 'const targets = [...found.operands, ...writeDestinations(found)];', 'const targets = [...found.operands];'],
  ["cp directory contents", 'checkCopyContents(found.operands, destinations, st, ctx, site);', 'void destinations;'],
  ["install value grammar", 'writeArguments(rest, "mogtS", [', 'writeArguments(rest, "tS", ['],
  ["ln value grammar", 'writeArguments(rest, "St", [', 'writeArguments(rest, "t", ['],
  ["install directory flag", 'found.flags.has("d") || found.flags.has("--directory")', 'false'],
  ["ln one operand", 'found.operands.length === 1 && found.directory === undefined && !found.uncertain', 'false'],
  ["ln symbolic operands", 'const symbolic = found.flags.has("s") || found.flags.has("--symbolic");', 'const symbolic = rest.some((arg) => literalText(arg.exp)?.includes("s"));'],
  ["long joined value", 'return completeName(text.split("=", 1)[0] as string, names);', 'return completeName(text, names);'],
  ["long abbreviation", 'name.startsWith(prefix)', 'name === prefix'],
  ["unique completion", 'reachable.length === 1 ? reachable[0] : undefined', 'undefined'],
  ["unknown option", 'if (!whole && mayBeOption(arg, () => false)) uncertain = true;', 'void whole;'],
  ["operand is not a flag", 'if (!text.startsWith("-") || text === "-") return;', 'if (text === "-") return;'],
  ["long flag record", 'if (option !== undefined) flags.add(option);', 'void option;'],
  ["long target offset", 'if (option === "--target-directory") offset = text.indexOf("=") + 1 || text.length;', 'if (option === "--target-directory") offset = text.length;'],
  ["short flag record", 'flags.add(letter);', 'void letter;'],
  ["short flag before value", 'if (!valued.includes(letter)) continue;', 'if (letter !== "t") continue;'],
  ["short target offset", 'if (letter === "t") offset = index + 1;', 'if (letter === "t") offset = index + 2;'],
  ["stop cluster at value", '        break;', '        continue;', "function writeArguments("],
  ["ignore nontarget option", 'if (offset === undefined) return;', 'if (offset === undefined) offset = text.length;'],
  ["attached target value", 'const value = valueInWord(arg, offset);', 'const value = undefined;'],
  ["separate target value", 'directory = value === undefined ? rest[rest.indexOf(arg) + 1] : { text: arg.text, exp: value };', 'directory = value === undefined ? undefined : { text: arg.text, exp: value };'],
  ["no target directory", 'found.flags.has("T") || found.flags.has("--no-target-directory")', 'false'],
  ["known target only", 'if (found.directory !== undefined && !found.uncertain) return [found.directory];', 'if (found.directory !== undefined && !found.uncertain) return [...found.operands, found.directory];'],
  ["unreadable destination", 'return [{ text: "destination", exp: [unknown("UNREADABLE write destination")] }];', 'return [];'],
  ["last destination", '  return [found.operands.at(-1) as Arg];', '  return [found.operands[0] as Arg];', 'unknown("UNREADABLE write destination")'],
  ["directory test", 'if (!statSync(dir, { throwIfNoEntry: false })?.isDirectory()) continue;', 'continue;'],
  ["exclude destination from sources", 'if (destinations.includes(source)) continue;', 'void source;'],
  ["unknown source basename", 'exp: [unknown("UNREADABLE copy source basename")]', 'exp: [literal(".")]'],
  ["source resolution", 'const from = path.resolve(st.cwd ?? "/", name);', 'const from = path.resolve("/", name);'],
  ["destination basename", 'const to = path.join(dir, path.basename(from));', 'const to = dir;'],
  ["nested source directory", 'if (statSync(from, { throwIfNoEntry: false })?.isDirectory()) {', 'if (false) {'],
  ["recursive descendants", 'readdirSync(from, { recursive: true })', 'readdirSync(from)'],
  ["descendant path", 'const target = path.join(to, entry);', 'const target = to;'],
  ["descendant target", 'targets.push({ text: target, exp: [literal(target)] });', 'void target;'],
  ["judge copy children", 'checkTargets("cp", "overwrite", targets, WRITES_THROUGH_LINK, st, ctx, site);', 'void targets;'],
  ["sed in-place abbreviation", 'if (option === "--in-place") inPlace = true;', 'if (text === "--in-place") inPlace = true;'],
  ["sed follow abbreviation", 'if (option === "--follow-symlinks") follow = true;', 'if (text === "--follow-symlinks") follow = true;'],
  ["sed expression abbreviation", 'if (option === "--expression" || option === "--file") {', 'if (text === "--expression" || text === "--file") {'],
  ["sed joined expression", 'if (!text.includes("=")) index += 1;', 'index += 1;'],
  ["sed line length", 'if (option === "--line-length" && !text.includes("=")) index += 1;', 'void option;'],
  ["minimum operands", 'found.operands.length < 2 || found.uncertain', 'found.operands.length < 1 || found.uncertain'],
  ["cp short backup", 'found.flags.has("b")', 'false'],
  ["cp long backup", 'found.flags.has("--backup")', 'false'],
  ["cp replacement flag", 'found.flags.has("--remove-destination")', 'false'],
  ["cp unknown replacement", 'found.flags.has("--remove-destination") || found.uncertain', 'found.flags.has("--remove-destination")'],
  ["cp value grammar", 'writeArguments(rest, "tS", [', 'writeArguments(rest, "t", [', 'case "cp": {'],
  ["resolved destination directory", 'const dir = path.resolve(st.cwd ?? "/", text);', 'const dir = path.resolve("/", text);'],
  ["direct child target", 'const targets = [{ text: to, exp: [literal(to)] }];', 'const targets: Arg[] = [];'],
];

const sourcePath = process.argv[2];
if (sourcePath === undefined) throw new Error("name the guard module");
const source = readFileSync(sourcePath, "utf8");
// The requested build and each freshly written mutant are runtime-selected modules.
const baseline = await import(sourcePath) as { createPaneGuard: GuardFactory };
const base = mkdtempSync(path.join(os.tmpdir(), "pane-write-mutants-"));
let survivors = 0;
try {
  const expected = WRITE_ROWS.map((row) => measureWriteRow(base, row, baseline.createPaneGuard));
  for (const [index, [name, from, to, scope]] of mutants.entries()) {
    const start = scope === undefined ? 0 : source.indexOf(scope);
    const at = source.indexOf(from, start);
    if (start < 0 || at < 0 || from.includes("\n") || to.includes("\n")) throw new Error(`bad single-line mutant: ${name}`);
    const modulePath = path.join(path.dirname(sourcePath), `.pane-guard-mutant-${process.pid}-${index}.ts`);
    try {
      writeFileSync(modulePath, source.slice(0, at) + to + source.slice(at + from.length));
      const mutant = await import(modulePath) as { createPaneGuard: GuardFactory };
      const killed: string[] = [];
      for (const result of expected) {
        try {
          const measured = measureWriteRow(base, result.row, mutant.createPaneGuard);
          if ((result.refusal === undefined) !== (measured.refusal === undefined)) killed.push(result.row.name);
        } catch (error) {
          if (!(error instanceof Error) || !error.message.startsWith("fixture plant refused:")) throw error;
          killed.push(`must-allow plant for ${result.row.name}`);
        }
      }
      const line = source.slice(0, at).split("\n").length;
      console.log(`${killed.length > 0 ? "KILLED" : "SURVIVED"} L${line} ${name}: ${killed.join(" | ")}`);
      if (killed.length === 0) survivors += 1;
    } finally {
      rmSync(modulePath, { force: true });
    }
  }
  console.log(`${mutants.length - survivors}/${mutants.length} single-line mutants killed; survivors=${survivors}`);
} finally {
  rmSync(base, { recursive: true, force: true });
}
process.exitCode = survivors === 0 ? 0 : 1;
