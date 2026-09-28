import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { createPaneGuard, type PaneGuard } from "./pane-guard";

// The guard evaluates `${v#…}`, `${v%…}` and `${v/…/…}` itself (pane-guard-bash.ts), decides
// `${v-…}` and `${v+…}` by whether a parameter is set, and binds a function's arguments. Held to
// real bash here: a target the guard judges is either unknown or the path bash computes, and a
// target bash puts outside the pane's roots is never allowed.

let base = "";
let workspace = "";
let scratch = "";
let guard: PaneGuard;
let env: NodeJS.ProcessEnv;

beforeAll(() => {
  base = mkdtempSync(path.join(os.tmpdir(), "legion-pane-guard-bash-"));
  workspace = path.join(base, "ws");
  scratch = path.join(base, "tmp");
  mkdirSync(workspace, { recursive: true });
  mkdirSync(scratch, { recursive: true });
  guard = createPaneGuard({ workspace, ompPid: process.pid, scratch });
  env = {
    HOME: path.join(base, "home"),
    LEGION_WORKSPACE: workspace,
    TMPDIR: scratch,
    E: "",
    PANEVAR: path.join(workspace, "p"),
    PATH: process.env.PATH,
  };
});

afterAll(() => {
  rmSync(base, { recursive: true, force: true });
});

function insideRoots(target: string): boolean {
  return (
    target === workspace || target.startsWith(`${workspace}/`) || target.startsWith(`${scratch}/`)
  );
}

/** The cases bash puts a target outside the roots in, each run by one bash as a subshell whose
 * `rm` is a function that prints its operands. */
function outsideCases(commands: readonly string[]): string[] {
  const script = commands
    .map(
      (command) => `(rm() { shift; printf '%s\\001' "$@"; }; ${command}) 2>/dev/null; printf '\\0'`
    )
    .join("\n");
  const run = spawnSync("bash", [], { input: script, encoding: "utf8", env, maxBuffer: 1 << 26 });
  expect(run.status).toBe(0);
  const printed = run.stdout.split("\0").slice(0, commands.length);
  expect(printed).toHaveLength(commands.length);
  return commands.filter((_, index) =>
    (printed[index] as string)
      .split("\u0001")
      .some((target) => target !== "" && !insideRoots(path.resolve(workspace, target)))
  );
}

const OPERATORS = ["/", "#", "##", "%", "%%"];
// Written after the operator: `/`-leading operands (which the parser splits differently from
// bash), escaped and quoted slashes, empty and expanding-to-empty patterns, globs, anchors.
const OPERANDS = [
  "/",
  "//",
  "///",
  "////home/v",
  "//home/v",
  "/home/v",
  "\\//_",
  "\\//\\/home\\/u",
  "a/b",
  "*/x",
  "#/P",
  "#//P",
  "%/S",
  "%//S",
  "#a/X",
  "%c/X",
  "/$E/x",
  '"/"/x',
  "'/'/x",
  "*",
  "?/Q",
  "b*/Y",
  "/x/",
  "..//home",
  "*//",
  "*//etc/ssh",
  "a/&",
  "/tmp//etc",
  "/tmp//home/op/.ssh",
];

test("every pattern expansion is unknown or bash's own value, and never allows a target outside the roots", () => {
  const values = [
    `${workspace}/build`,
    `${workspace}/a/b`,
    "a/b/c.tar.gz",
    "aaa",
    "",
    "x:y:z",
    "/a//b/",
    "...",
    "a*b",
    `${scratch}/q/../w`,
  ];
  const cases = values.flatMap((value) =>
    OPERATORS.flatMap((operator) =>
      OPERANDS.map((operand) => ({ value, expression: `\${v${operator}${operand}}` }))
    )
  );
  // One bash computes every case; a case bash rejects prints a marker instead.
  const script = cases
    .map(
      ({ value, expression }) =>
        `(v='${value}'; printf '%s\\0' "${expression}") 2>/dev/null || printf '\\1\\0'`
    )
    .join("\n");
  const run = spawnSync("bash", ["-c", `E=; ${script}`], { encoding: "utf8" });
  expect(run.status).toBe(0);
  const results = run.stdout.split("\0").slice(0, cases.length);
  expect(results).toHaveLength(cases.length);

  const underRefused: string[] = [];
  const wrong: string[] = [];
  let resolved = 0;
  cases.forEach(({ value, expression }, index) => {
    const printed = results[index] as string;
    if (printed === "\u0001") return;
    const target = path.resolve(workspace, printed);
    const inside = insideRoots(target);
    const reason = guard.bash(`v='${value}'; rm -rf "${expression}"`, workspace, env);
    if (reason === undefined) {
      if (!inside) underRefused.push(`${value} ${expression} -> ${target}`);
      else resolved += 1;
      return;
    }
    const named = /rm would delete `[^`]*` \((\/[^)`]*)\)/.exec(reason)?.[1];
    if (named === undefined) return;
    resolved += 1;
    if (named !== target) wrong.push(`${value} ${expression}: bash ${target}, guard ${named}`);
  });
  expect(underRefused).toEqual([]);
  expect(wrong).toEqual([]);
  // The harness itself: most cases resolve, so an empty comparison is not a pass by default.
  expect(resolved).toBeGreaterThan(cases.length / 2);
});

test("an operator on whether a parameter is set never allows a target bash puts outside the roots", () => {
  // Set, empty, unset, unset after a value, left to the pane's environment or removed from it, and
  // never mentioned; a positional parameter given, empty, absent, and in a shell whose arguments
  // the guard does not know.
  const setups = [
    ":",
    "unset X",
    "X=",
    'X="$HOME/.ssh"',
    'X="$LEGION_WORKSPACE/x"',
    "X=y; unset X",
    "unset PANEVAR",
    "env -u X true",
    "set --",
    'set -- "$HOME/.ssh"',
    'set -- ""',
  ];
  const operators = ["-", ":-", "+", ":+", "=", ":=", "?", ":?"];
  const commands = setups.flatMap((setup) =>
    ["X", "PANEVAR", "1", "NOPE"].flatMap((name) =>
      operators
        .filter((operator) => name !== "1" || !operator.endsWith("="))
        .flatMap((operator) =>
          ["$HOME/.ssh", "$LEGION_WORKSPACE/w"].flatMap((operand) => {
            const expression = `\${${name}${operator}${operand}}`;
            return [
              `${setup}; rm -rf "${expression}"`,
              `${setup}; rm -rf "/${expression}"`,
              `${setup}; f() { rm -rf "${expression}"; }; f`,
            ];
          })
        )
    )
  );
  const outside = outsideCases(commands);
  expect(outside.filter((command) => guard.bash(command, workspace, env) === undefined)).toEqual(
    []
  );
  // The harness itself: bash deletes outside the roots in many cases, so none allowed is a result.
  expect(outside.length).toBeGreaterThan(commands.length / 10);
});

test("a function's arguments are bash's own, and never let a target outside the roots through", () => {
  // Words that are one argument, none (an empty unquoted value, `"$@"` of no arguments, an empty
  // array), several (a value with a space), or unknown; bodies that index, shift, count and test.
  const prelude = 'A="$HOME/.ssh"; W="$LEGION_WORKSPACE/w"; S="a $HOME/.ssh"; arr=(); one=("")';
  const words = [
    '"$A"',
    "$A",
    '"$W"',
    '"$E"',
    "$E",
    "$S",
    '"$S"',
    '"$@"',
    "$@",
    '"$*"',
    "$*",
    `"\${arr[@]}"`,
    `"\${one[@]}"`,
    "$NOPE",
  ];
  const bodies = [
    'rm -rf "$1"',
    'rm -rf "$2"',
    'rm -rf "$3"',
    'rm -rf "$@"',
    'shift 2; rm -rf "$1"',
    'shift; rm -rf "$1"',
    `rm -rf "\${2-$A}"`,
    `rm -rf "\${3:-$W}"`,
    `rm -rf "\${2+$A}"`,
    '[ $# -gt 2 ] || rm -rf "$2"',
    'rm -rf "$#$1"',
  ];
  const commands = ["", '"$A"', '"$E" "$A"', '"$W"'].flatMap((outer) =>
    bodies.flatMap((body) =>
      words.flatMap((first) =>
        ['"$A"', "$E", '"$@"', "$S"].map(
          (second) => `${prelude}; set -- ${outer}; f() { ${body}; }; f ${first} ${second}`
        )
      )
    )
  );
  const outside = outsideCases(commands);
  expect(outside.filter((command) => guard.bash(command, workspace, env) === undefined)).toEqual(
    []
  );
  expect(outside.length).toBeGreaterThan(commands.length / 10);
});
