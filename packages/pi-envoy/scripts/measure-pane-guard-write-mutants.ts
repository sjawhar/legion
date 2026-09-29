#!/usr/bin/env bun
/** Single-line mutations of the write and find readers, measured against their live canary rows.
 * Usage: nice -n 19 bun <this file> <absolute path to pane-guard.ts>
 * Each temporary guard stays beside its imports; the source guard is never modified. */
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  type GuardFactory,
  measureWriteRow,
  WRITE_ROWS,
  writeRefusalMatches,
} from "../src/legion/pane-guard-write-rows";
import { FIND_MUTANT_ROWS, measureRow } from "../src/legion/pane-guard-path-rows";

// [name, old line fragment, replacement on that line, optional preceding scope anchor].
const mutants: readonly (readonly [string, string, string, string?])[] = [
  [
    "option terminator",
    'if (whole && text === "--") {',
    "if (false) {",
    "function writeArguments(",
  ],
  [
    "GNU separate value",
    'takesValue = long[option] === true && !text.includes("=");',
    "takesValue = false;",
  ],
  ["consumed separate value", "takesValue = index === text.length - 1;", "takesValue = false;"],
  ["install strip flag", '"--strip": false,', '"--unused": false,', "const INSTALL_OPTIONS"],
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
  [
    "install value grammar",
    'writeArguments(rest, "mogtS", INSTALL_OPTIONS)',
    'writeArguments(rest, "tS", INSTALL_OPTIONS)',
  ],
  [
    "ln value grammar",
    'writeArguments(rest, "St", LN_OPTIONS)',
    'writeArguments(rest, "t", LN_OPTIONS)',
  ],
  ["install directory flag", 'found.flags.has("d") || found.flags.has("--directory")', "false"],
  ["ln one operand", "found.operands.length === 1 &&", "false &&"],
  ["ln one read word", "readableWord(sole).whole", "true"],
  [
    "no-field simple command argument",
    'if (argv[index]?.fields === "none") argv.splice(index, 1);',
    "void argv;",
    "function commandArgs(",
  ],
  [
    "no-field command argument is unreadable",
    'if (argv[index]?.fields === "none") argv.splice(index, 1);',
    'if (argv[index]?.fields === "unknown") argv.splice(index, 1);',
    "function commandArgs(",
  ],
  [
    "no-field command argument is the only field",
    'if (argv[index]?.fields === "none") argv.splice(index, 1);',
    'if (argv[index]?.fields === undefined) argv.splice(index, 1);',
    "function commandArgs(",
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
    "if (!whole && mayBeSomeOption(text)) uncertain = arg;",
    "void whole;",
  ],
  [
    "operand is not a flag",
    'if (!whole || !text.startsWith("-") || text === "-") {',
    'if (!whole || text === "-") {',
    "function writeArguments(",
  ],
  ["long flag record", "flags.add(option);", "void option;", "function writeArguments("],
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
  ["ignore nontarget option", "if (offset === undefined) continue;", "if (false) continue;"],
  ["attached target value", "const value = valueInWord(arg, offset);", "const value = undefined;"],
  [
    "separate target value",
    "directory = value === undefined ? next : { text: arg.text, exp: value };",
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
  ["recursive descendants", "found.flags.has(flag)", "false", "const recursive ="],
  ["an unread word may be -r", "found.uncertain ||", "false ||", "const recursive ="],
  ["unknown positional count", 'name === "*" && quoted ? piece : { ...piece, unknownCount: true }', "piece"],
  ["a quoted * joins into one argument", 'name === "*" && quoted ? piece', "false ? piece"],
  ["a quoted array * joins into one", 'if (part.index === "*" && quoted) return [[...unknownElement]];', ""],
  ["an unassigned array quoted *", '!(part.index === "*" && quoted) && alt.some', "alt.some"],
  ["a stored value drops the count", "if (piece.unknownCount !== true) return piece;", "if (true) return piece;"],
  ["unknown array count", "unknownElement.map((piece) => ({ ...piece, unknownCount: true as const }))", "unknownElement.map((piece) => ({ ...piece }))"],
  ["unassigned array count", "? alt.map((piece) => ({ ...piece, unknownCount: true as const }))", "? alt"],
  ["args reads the count", "if (exp.some((piece) => piece.unknownCount === true)) {", "if (false) {"],
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
    'if (option === "--expression" || option === "--file") expression = true;',
    'if (text === "--expression" || text === "--file") expression = true;',
  ],
  [
    "sed joined expression",
    'if ((option === undefined ? !whole : SED_OPTIONS[option]) && !text.includes("=")) {',
    "if (option === undefined ? !whole : SED_OPTIONS[option]) {",
  ],
  ["sed line length", '"--line-length": true,', '"--line-length": false,', "const SED_OPTIONS"],
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
    'writeArguments(rest, "tS", CP_OPTIONS)',
    'writeArguments(rest, "t", CP_OPTIONS)',
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
  ["loop prefix must be literal", 'words[0]?.exp[0]?.kind === "literal" &&', "true &&"],
  ["loop prefix must be nonempty", 'words[0].exp[0].text !== "" &&', "true &&"],
  ["loop prefix must not start with dash", '!words[0].exp[0].text.startsWith("-") &&', "true &&"],
  ["cp sparse value", '"--sparse": true,', '"--sparse": false,', "const CP_OPTIONS"],
  ["cp no-preserve value", '"--no-preserve": true,', '"--no-preserve": false,', "const CP_OPTIONS"],
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
    "return { replaced: [], followed: [], inPlace: false, inferred: false };",
    "continue;",
  ],
  [
    "sed one part per unread word",
    "const others = unread.length - (maybe ? 1 : 0);",
    "const others = unread.length;",
  ],
  ["sed unread operand stands before nothing", "if (!maybe) before = true;", "before = true;"],
  ["sed split word", "splitOption = true;", "splitOption = false;"],
  [
    "sed script led by text is no option",
    'const option = !whole && text === "";',
    "const option = !whole;",
  ],
  [
    "sed unread value keeps a later --",
    "if (afterUnread) reopened = true;",
    "if (false) reopened = true;",
  ],
  [
    "sed named long word",
    "if (option === undefined && !whole) unread.push(arg);",
    "if (!whole) unread.push(arg);",
  ],
  [
    "sed unnamed long word",
    "if (option === undefined && !whole) unread.push(arg);",
    "if (false) unread.push(arg);",
  ],
  [
    "sed unread long value",
    "if (whole && !reopened) i += 1;",
    "if (true) i += 1;",
    'if (whole && !reopened && (option === "--help"',
  ],
  [
    "sed unread cluster value",
    "if (whole && !reopened) i += 1;",
    "if (true) i += 1;",
    'if ("efl".includes(letter)) {',
  ],
  ["sed unread cluster", "if (!settled && !whole) {", "if (false) {"],
  [
    "sed follow reading",
    "if (others >= needed + unsettled(follow || splitOption)) followed.push(operand.arg);",
    "if (false) followed.push(operand.arg);",
  ],
  ["shred through its link", "ACTS_ON_BOTH,", "ACTS_ON_LINK,", 'case "shred":'],
  [
    "find follows command-line roots",
    'mode !== "P" || text.endsWith("/") || text.endsWith("/.")',
    "false",
    "function checkFindRoots(",
  ],
  [
    "find L walk",
    'verdict = mode === "L" ? checkFindLinkWalk(resolved.real, ctx, site) : { ok: true };',
    "verdict = { ok: true };",
    "function checkFindRoots(",
  ],
  [
    "find L link stays in roots",
    "!inWorkspace(resolved.real, ctx.roots) && !inScratch(resolved.real, ctx.roots)",
    "false",
    "function checkFindLinkWalk(",
  ],
  ["find follow expression", 'if (word === "-follow") mode = "L";', "void word;"],
  ["find final dot", 'text.endsWith("/.")', "false", "function checkFindRoots("],
  [
    "find P glob allowance",
    'mode === "P" && !findRootMayFollow(target.exp)',
    "false",
    "function checkFindRoots(",
  ],
  [
    "find nonliteral follow refused",
    'mode === "P" && !findRootMayFollow(target.exp)',
    "true",
    "function checkFindRoots(",
  ],
  [
    "find lenient code allowance",
    "target.exp.some((piece) => piece.lenient)",
    "false",
    "function checkFindRoots(",
  ],
  [
    "find unknown suffix",
    'if (piece.kind === "unknown") return suffix === "" || suffix === "." || suffix === "/";',
    'if (piece.kind === "unknown") return false;',
    "function findRootMayFollow(",
  ],
  [
    "find glob dot suffix",
    'return suffix.endsWith("/") || suffix === "/.";',
    'return suffix.endsWith("/");',
    "function findRootMayFollow(",
  ],
  ["find predicate value", "if (isValue) continue;", "void isValue;", "function checkFind("],
  [
    "find exec argument",
    "if (index <= commandEnd) continue;",
    "void commandEnd;",
    "function checkFind(",
  ],
  [
    "find walk-limit diagnostic",
    "throw new Refusal(site.snippet, site.line, WALK_LIMIT);",
    "return { ok: false, resolution: WALK_LIMIT };",
    "function checkFindLinkWalk(",
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
  const expectedWrites = WRITE_ROWS.map((row) =>
    measureWriteRow(base, row, baseline.createPaneGuard)
  );
  const expectedFinds = FIND_MUTANT_ROWS.map((row) =>
    measureRow(row, base, baseline.createPaneGuard)
  );
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
      for (const result of expectedWrites) {
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
      for (const result of expectedFinds) {
        const measured = measureRow(result.row, base, mutant.createPaneGuard);
        if ((result.refusal === undefined) !== (measured.refusal === undefined)) {
          killed.push(result.row.name);
        }
      }
      if (name === "find walk-limit diagnostic") {
        const workspace = path.join(base, "find-budget");
        mkdirSync(workspace);
        for (let entry = 0; entry < 100_001; entry += 1) {
          writeFileSync(path.join(workspace, String(entry)), "");
        }
        const options = { workspace, scratch: base, ompPid: process.pid };
        const command = "find -L . -name absent -delete";
        const expected = baseline.createPaneGuard(options).bash(command, workspace, {});
        const actual = mutant.createPaneGuard(options).bash(command, workspace, {});
        if (!expected?.includes("walk limit")) throw new Error("find did not reach its walk limit");
        if (actual !== expected) killed.push("find walk-limit diagnostic");
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
