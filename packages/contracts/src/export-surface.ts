/**
 * The export surface of `@legion/contracts`, measured rather than read: every name an entry
 * exports, what kind of thing it is, and, for a zod schema, whether it refuses a key it does not
 * declare. Strictness is judged by behaviour alone - the schema is handed a value carrying a key
 * it cannot know, and the verdict is whether the answer holds an `unrecognized_keys` issue - so
 * nothing here depends on how zod spells a schema internally.
 *
 * `export-surface.test.ts` holds the surface of `main` as a committed snapshot and compares this
 * reading of the working tree against it, because the names this package exports are what a
 * branch's callers compile against: a name that was the tolerant reading of a bus frame and
 * becomes the exact one still compiles at every call site and then refuses frames it used to
 * accept. This module is test support; `index.ts` does not re-export it.
 *
 * Regenerate the snapshot with:
 *
 *     cd packages/contracts && bun run src/export-surface.ts
 */
import { mkdirSync, mkdtempSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import ts from "typescript";

/** What an exported name is: a zod schema with its reading of an undeclared key, a callable, a
 *  value that is neither, or a name with no value behind it at all. */
export type ExportKind = "schema:strict" | "schema:tolerant" | "function" | "constant" | "type";

/** One entry's names, and the entries of the package: `index.ts` is everything
 *  `@legion/contracts` re-exports, and `legion-api.ts` is the one subpath it does not. */
export type EntrySurface = Readonly<Record<string, ExportKind>>;

export type ExportSurface = Readonly<Record<string, EntrySurface>>;

export const SURFACE_ENTRIES = ["index.ts", "legion-api.ts"] as const;

/** The key handed to every schema: a name no contract declares, so an answer that names it as
 *  unrecognized is a schema that refuses what it does not know. */
const PROBE_KEY = "__export_surface_probe__";

/** One issue of a failed parse, in the shape every zod version reports: enough to tell an
 *  unrecognized key from a missing field, and to fill the missing field in and ask again. */
interface ParseIssue {
  readonly code?: unknown;
  readonly path?: readonly (string | number)[];
  readonly expected?: unknown;
  readonly values?: readonly unknown[];
  readonly errors?: readonly (readonly ParseIssue[])[];
}

interface ParseFailure {
  readonly success: false;
  readonly error: { readonly issues: readonly ParseIssue[] };
}

type ParseAnswer = { readonly success: true } | ParseFailure;

interface Parser {
  readonly safeParse: (value: unknown) => ParseAnswer;
}

/** Whether `value` parses: a zod schema is recognised by the one method this module calls on it,
 *  never by its class or its internals, so a schema from another copy of zod reads the same. */
function parserOf(value: unknown): Parser | null {
  if (typeof value !== "object" || value === null) return null;
  if (!("safeParse" in value) || typeof value.safeParse !== "function") return null;
  // Structural: the call above is the whole contract, and its answer is validated as it is read.
  return value as Parser;
}

/** What a value of the type the issue asks for looks like, or undefined when the issue says
 *  nothing a filler can be built from (a refinement, a shape nobody can guess). */
function filler(issue: ParseIssue): unknown {
  if (issue.code === "invalid_value" && issue.values !== undefined && issue.values.length > 0) {
    return issue.values[0];
  }
  switch (issue.expected) {
    case "string":
      return "x";
    case "number":
    case "int":
      return 0;
    case "bigint":
      return 0n;
    case "boolean":
      return false;
    case "array":
      return [];
    case "object":
    case "record":
      return {};
    case "null":
      return null;
    case "date":
      return new Date(0);
    case "set":
      return new Set();
    case "map":
      return new Map();
    default:
      return undefined;
  }
}

/** A union reports its branches' issues under itself; the first branch is the one a filler
 *  follows, since filling every branch at once builds a value no branch accepts. */
function flatten(issues: readonly ParseIssue[]): ParseIssue[] {
  const flat: ParseIssue[] = [];
  for (const issue of issues) {
    const branch = issue.errors?.[0];
    if (issue.code === "invalid_union" && branch !== undefined) {
      for (const nested of branch) {
        flat.push({ ...nested, path: [...(issue.path ?? []), ...(nested.path ?? [])] });
      }
      continue;
    }
    flat.push(issue);
  }
  return flat;
}

/** Writes `value` at `keys` in `probe`, building the objects and arrays on the way. False when
 *  the path is the root (which would throw the probe key away) or already holds something. */
function place(probe: Record<string, unknown>, keys: readonly (string | number)[], value: unknown) {
  if (keys.length === 0) return false;
  let cursor: Record<string | number, unknown> = probe;
  for (const key of keys.slice(0, -1)) {
    const held = cursor[key];
    // A container is written where there is none, so the step below always lands in one; the
    // compiler cannot follow that write, hence the one assertion.
    const next =
      typeof held === "object" && held !== null ? held : typeof key === "number" ? [] : {};
    cursor[key] = next;
    cursor = next as Record<string | number, unknown>;
  }
  const last = keys[keys.length - 1];
  if (cursor[last] !== undefined) return false;
  cursor[last] = value;
  return true;
}

/**
 * Whether a schema refuses a key it does not declare. The probe starts as an object holding only
 * `PROBE_KEY`; while the answer refuses it for some other reason, the fields that reason names
 * are filled in and the probe is handed over again, so a schema whose required fields hide the
 * verdict still gives it. A schema that never answers either way reads as tolerant: it is the
 * answer a reader of the bus is given.
 */
function strictness(parser: Parser): "schema:strict" | "schema:tolerant" {
  const probe: Record<string, unknown> = { [PROBE_KEY]: true };
  for (let round = 0; round < 24; round += 1) {
    const answer = parser.safeParse(probe);
    if (answer.success) return "schema:tolerant";
    const issues = answer.error.issues;
    if (issues.some((issue) => issue.code === "unrecognized_keys")) return "schema:strict";
    let filled = false;
    for (const issue of flatten(issues)) {
      const value = filler(issue);
      if (value === undefined) continue;
      if (place(probe, issue.path ?? [], value)) filled = true;
    }
    if (!filled) return "schema:tolerant";
  }
  return "schema:tolerant";
}

/** Every name an entry exports, and which of them have a value behind them: a name that is only
 *  a type has none, and no amount of importing the module will show it. */
function exportedNames(entry: string): { readonly names: string[]; readonly values: Set<string> } {
  const program = ts.createProgram([entry], {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
    strict: true,
    noEmit: true,
    skipLibCheck: true,
  });
  const checker = program.getTypeChecker();
  const source = program.getSourceFile(entry);
  if (source === undefined) throw new Error(`${entry} is not a source file of its own program`);
  const module = checker.getSymbolAtLocation(source);
  if (module === undefined) throw new Error(`${entry} exports nothing a type checker can see`);
  const names: string[] = [];
  const values = new Set<string>();
  for (const symbol of checker.getExportsOfModule(module)) {
    const resolved =
      (symbol.flags & ts.SymbolFlags.Alias) === 0 ? symbol : checker.getAliasedSymbol(symbol);
    names.push(symbol.name);
    if ((resolved.flags & ts.SymbolFlags.Value) !== 0) values.add(symbol.name);
  }
  return { names: names.sort(), values };
}

/** Reads one entry: its names from the type checker, its values from the module itself. */
async function readEntry(entry: string): Promise<EntrySurface> {
  const { names, values } = exportedNames(entry);
  // The entry is chosen at runtime - this module reads the working tree and a copy of another
  // revision in a temporary directory - so the specifier cannot be static.
  const loaded = (await import(entry)) as Record<string, unknown>;
  const surface: Record<string, ExportKind> = {};
  for (const name of names) {
    if (!values.has(name)) {
      surface[name] = "type";
      continue;
    }
    const value = loaded[name];
    const parser = parserOf(value);
    if (parser !== null) surface[name] = strictness(parser);
    else if (typeof value === "function") surface[name] = "function";
    else surface[name] = "constant";
  }
  return surface;
}

/** The surface of the package whose `src` directory is `srcDir`. */
export async function readExportSurface(srcDir: string): Promise<ExportSurface> {
  const surface: Record<string, EntrySurface> = {};
  for (const entry of SURFACE_ENTRIES) {
    surface[entry] = await readEntry(path.join(srcDir, entry));
  }
  return surface;
}

/** Runs a command that must succeed, and answers what it wrote. */
function run(command: string[], cwd: string): string {
  const answer = Bun.spawnSync(command, { cwd, stdout: "pipe", stderr: "pipe" });
  if (answer.exitCode !== 0) {
    throw new Error(`${command.join(" ")} failed: ${answer.stderr.toString().trim()}`);
  }
  return answer.stdout.toString();
}

/**
 * Copies this package's sources at `revision` into a temporary directory and reads their surface
 * there. The working tree is never checked out over: the copy borrows this one's `node_modules`,
 * so it resolves the same zod.
 */
async function surfaceAt(revision: string, repoRoot: string): Promise<ExportSurface> {
  const copy = mkdtempSync(path.join(tmpdir(), "contracts-export-surface-"));
  const srcDir = path.join(copy, "src");
  mkdirSync(srcDir);
  symlinkSync(
    path.join(repoRoot, "packages", "contracts", "node_modules"),
    path.join(copy, "node_modules")
  );
  const listed = run(
    ["jj", "--repository", repoRoot, "file", "list", "-r", revision, "packages/contracts/src"],
    repoRoot
  );
  for (const file of listed.split("\n")) {
    if (!file.endsWith(".ts") || file.endsWith(".test.ts")) continue;
    const shown = run(
      ["jj", "--repository", repoRoot, "file", "show", "-r", revision, file],
      repoRoot
    );
    writeFileSync(path.join(srcDir, path.basename(file)), shown);
  }
  return await readExportSurface(srcDir);
}

if (import.meta.main) {
  const argv = Bun.argv.slice(2);
  const read = (flag: string, fallback: string): string => {
    const at = argv.indexOf(flag);
    return at < 0 ? fallback : (argv[at + 1] ?? fallback);
  };
  const revision = read("--revision", "main@origin");
  const repoRoot = path.resolve(import.meta.dir, "..", "..", "..");
  const out = path.resolve(read("--out", path.join(import.meta.dir, "export-surface.main.json")));
  const surface = await surfaceAt(revision, repoRoot);
  await Bun.write(out, `${JSON.stringify(surface, null, 2)}\n`);
  const counted = Object.entries(surface)
    .map(([entry, names]) => `${entry}: ${Object.keys(names).length} names`)
    .join(", ");
  process.stdout.write(`${path.relative(repoRoot, out)} holds ${revision} (${counted})\n`);
}
