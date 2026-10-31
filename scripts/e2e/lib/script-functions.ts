import { readFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";

/**
 * Reads the shell script at `path` once and returns `fn`, which gives the definition of the
 * function `name` exactly as the script writes it at the start of a line: `name() { … }` when it
 * is one line, else from `name() {` to the first line that is `}` alone. `fn` throws, naming the
 * script by its path under scripts/e2e, when the script defines no such function.
 */
export function scriptFunctions(path: string): (name: string) => string {
  const script = readFileSync(path, "utf8");
  const shown = relative(dirname(import.meta.dir), path);
  return (name) => {
    const found = new RegExp(`^${name}\\(\\) \\{(?:.*\\}$|[\\s\\S]*?\\n\\}$)`, "m").exec(script);
    if (found === null) throw new Error(`${shown} defines no ${name}()`);
    return found[0];
  };
}

// Every function a script defines at the start of a line, one-line definitions included.
const definition = /^([A-Za-z_][A-Za-z0-9_]*)\(\) \{/gm;
// A library of scripts/e2e/lib a script sources, as every stage script and library writes it.
const sourced = /^\s*\. ".*\/lib\/([A-Za-z0-9_.-]+\.sh)"\s*$/gm;

/**
 * The shell a harness puts above the functions it extracts from `paths`, so that a helper one of
 * them calls that the harness neither extracts nor stubs is named rather than left as a bare
 * `exit 127` the caller must trace: bash's command_not_found_handle prints
 * `harness: no definition for <name>` for a name one of those scripts defines, and leaves every
 * other missing command (a binary the machine lacks) as it was, still exit 127. The line goes to
 * the harness's own stderr, saved in a file descriptor when the guard is set up, so a call the
 * extracted function silences (`helper >/dev/null 2>&1`) is named too. Every library under
 * scripts/e2e/lib a script among `paths` sources (`. "…/lib/<name>"`) counts as one of them, and
 * the libraries it sources in turn, so a harness passes only the scripts it extracts from.
 * `missingHelpers` reads those lines out of a run's output for the harness to fail on.
 */
export function nameMissingHelpers(...paths: string[]): string {
  const names = new Set<string>();
  const read = new Set<string>();
  const pending = [...paths];
  for (let path = pending.pop(); path !== undefined; path = pending.pop()) {
    if (read.has(path)) continue;
    read.add(path);
    const text = readFileSync(path, "utf8");
    for (const found of text.matchAll(definition)) names.add(found[1]);
    for (const found of text.matchAll(sourced)) pending.push(join(import.meta.dir, found[1]));
  }
  return `exec {harness_stderr}>&2
command_not_found_handle() {
  case " ${[...names].join(" ")} " in
    *" $1 "*) printf 'harness: no definition for %s\\n' "$1" >&"$harness_stderr" ;;
  esac
  return 127
}`;
}

/**
 * Throws, naming each one, when a run's output shows the harness left a helper of the extracted
 * functions undefined. A harness calls it on every stream a run's shell writes.
 */
export function expectNoMissingHelpers(...outputs: string[]): void {
  const named = outputs.flatMap((out) =>
    [...out.matchAll(/harness: no definition for (\S+)/g)].map((found) => found[1])
  );
  const missing = [...new Set(named)];
  if (missing.length > 0) {
    throw new Error(
      `the harness neither extracts nor stubs ${missing.join(", ")}, which the functions it ` +
        "extracts call: add each one beside them"
    );
  }
}
