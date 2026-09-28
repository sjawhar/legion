import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { createPaneGuard, type PaneGuard } from "./pane-guard";
import {
  DOTDOT_COMPONENT,
  measureAllPathRows,
  PATH_ROWS,
  type PathRowResult,
} from "./pane-guard-path-rows";

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
  // array), several (a value with a space), or unknown, and variables and array literals that hold
  // those; bodies that index, shift, count and test.
  const prelude = `A="$HOME/.ssh"; W="$LEGION_WORKSPACE/w"; S="a $HOME/.ssh"; arr=(); one=(""); X="\${arr[@]}"; Y=("\${arr[@]}" "$A"); Z=($S "$W")`;
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
    '"$X"',
    '"$P"',
    `"\${Y[0]}"`,
    `"\${Y[@]}"`,
    `"\${Z[1]}"`,
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
          (second) => `${prelude}; set -- ${outer}; P="$@"; f() { ${body}; }; f ${first} ${second}`
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

test("every write to a variable is bash's own, and never leaves a stale value to let a target through", () => {
  // A variable, an array, an element, a variable already outside the roots, one `readonly` and
  // one `-u` uppercases, before a write by a builtin that names it, a plain assignment, arithmetic,
  // `:=`, or a function's `local`; then a read of the name, its element 0, its elements, and
  // whether it is set. Its 4,416 cases take several seconds, past bun's default timeout.
  const befores = [
    'd="$LEGION_WORKSPACE/safe"',
    'd=("$LEGION_WORKSPACE/safe")',
    'd[0]="$LEGION_WORKSPACE/safe"',
    'd=("$LEGION_WORKSPACE/safe" "$LEGION_WORKSPACE/b")',
    'd=("$LEGION_WORKSPACE/safe" "$HOME/.ssh")',
    'd="$HOME/.ssh"',
    'd="$HOME/.ssh"; readonly d',
    "declare -u d",
  ];
  const names = ["d", "'d[0]'", "d[0]", '"d[1-1]"', '"$(echo d)"', `"$(echo 'd[0]')"`];
  const named = (name: string) => [
    `printf -v ${name} '%s' "$HOME/.ssh"`,
    `printf -v${name} '%s' "$HOME/.ssh"`,
    `read -r ${name} <<< "$HOME/.ssh"`,
    `IFS= read -r ${name} <<< "$HOME/.ssh"`,
    `read -ra ${name} <<< "$HOME/.ssh"`,
    `mapfile -t ${name} <<< "$HOME/.ssh"`,
    `getopts x: ${name} -x "$HOME/.ssh" || :`,
    `declare ${name}="$HOME/.ssh"`,
    `typeset ${name}="$HOME/.ssh"`,
    `export ${name}="$HOME/.ssh"`,
    `readonly ${name}="$HOME/.ssh"`,
    `eval ${name}='"$HOME/.ssh"'`,
    `unset ${name}`,
  ];
  const writers = [
    ...names.flatMap(named),
    'd="$LEGION_WORKSPACE/x"',
    'd=("$LEGION_WORKSPACE/x")',
    "(( d = 0 ))",
    ": $(( d++ ))",
    "let d=0",
    "for ((d = 0; d < 1; d++)); do :; done",
    "sleep 0 & wait -p d",
    `: "\${d:=$HOME/.ssh}"`,
    `unset d; : "\${d=$HOME/.ssh}"`,
    'for d in "$HOME/.ssh"; do :; done',
    'f() { local d="$LEGION_WORKSPACE/x"; }; f',
    'f() { declare d; d="$LEGION_WORKSPACE/x"; }; f',
    'f() { if [ -n "$Z" ]; then local d; fi; d="$HOME/.ssh"; }; f',
    'if [ -n "$Z" ]; then unset d; fi',
  ];
  const reads = [
    'rm -rf "$d"',
    'rm -rf "/$d"',
    `rm -rf "\${d[0]}"`,
    `rm -rf "\${d[@]}"`,
    `rm -rf "/\${d+$LEGION_WORKSPACE/w}"`,
    `rm -rf "\${d-$HOME/.ssh}"`,
  ];
  const commands = befores.flatMap((before) =>
    writers.flatMap((writer) => reads.map((read) => `${before}; ${writer}; ${read}`))
  );
  const outside = outsideCases(commands);
  expect(outside.filter((command) => guard.bash(command, workspace, env) === undefined)).toEqual(
    []
  );
  expect(outside.length).toBeGreaterThan(commands.length / 10);
}, 60_000);

// Where the guard's path and the kernel's part company is a component the guard cannot RESOLVE,
// not a `..`: `path.resolve` — and `realpathSync` on this runtime — strip a `..` before the
// symlink in front of it is read, which is the shape that makes the divergence visible, but a
// directory the guard may not search diverges with no `..` anywhere. These rows are judged by
// what real bash did to a canary HOME, never by their names (LEGION-355).
//
// Two residuals, both allowed here and both with their own controls in the batch:
//
// - `cp`, `dd`, `install` and `ln` are not path-matched at all, so their `..` rows are allowed —
//   and so is their own no-`..` control, which is what says the `..` is not what lets them
//   through. That is #1551's create-the-target residual (LEGION-357).
// - The command changing, during its own run, the namespace the guard resolved against: it
//   retargets a link the guard already followed, or it creates the component that decides where
//   the path lands (`ln -s .. sub/made; echo <payload> >> sub/made/unread.sh`, which carries no
//   `..` in the written path at all — the `..` is the link's target). Resolving harder cannot
//   reach either: the first reading was right when it was taken, and in the second there was
//   nothing on disk to read. The `prelink.*` rows are the discriminator — the same write through
//   a link ALREADY on disk is refused here and allowed at base.
const PATH_ROW_RESIDUAL = [
  "cp.dotdot.symlink",
  "cp.symlink.nodotdot",
  "dd.dotdot.symlink",
  "install.dotdot.symlink",
  "ln.sf.dotdot.symlink",
  "retarget.realdir.dotdot",
  "retarget.script",
  "madelink.append",
  "madelink.group",
  "madelink.tee",
  "retarget.symlink.dotdot",
  "retarget.symlink.nodotdot",
  "retarget.truncate",
];

// The copy family is the one place a control does not fire, and that is the finding rather than a
// gap: its no-`..` must-refuse leaks identically to its `..` rows.
const PATH_ROW_CONTROL_EXEMPT = ["cp.symlink.nodotdot"];

test("the path battery's rows measure the property they name", () => {
  // `path.join` normalises a `..` away, so a row whose property is the `..` and whose fixture was
  // built with it would pass while measuring nothing. This is the assertion that catches that.
  expect(
    PATH_ROWS.filter((row) => row.dotdot && !DOTDOT_COMPONENT.test(row.command)).map((r) => r.name)
  ).toEqual([]);

  // Every operation class carries a control in both directions, so no class rests on probes alone.
  const families = [...new Set(PATH_ROWS.map((row) => row.family))].sort();
  expect(
    families.filter(
      (family) =>
        !PATH_ROWS.some((row) => row.family === family && row.role === "must-refuse") ||
        !PATH_ROWS.some((row) => row.family === family && row.role === "must-allow")
    )
  ).toEqual([]);
  expect(families.length).toBeGreaterThan(10);
});

test("a target is judged as the kernel resolves it, not as the text reads", () => {
  const root = mkdtempSync(path.join(os.tmpdir(), "legion-pane-guard-paths-"));
  try {
    const results = measureAllPathRows(root);
    const live = results.filter((result) => result.live);
    const named = (subset: readonly PathRowResult[]): string[] =>
      subset.map((result) => result.row.name).sort();

    // Positive controls: a harness whose bash did nothing, or whose controls stopped
    // discriminating, must fail rather than report a clean zero.
    expect(live.length).toBeGreaterThan(20);
    expect(
      named(results.filter((r) => r.row.role === "must-refuse" && r.refusal === undefined))
    ).toEqual([...PATH_ROW_CONTROL_EXEMPT].sort());

    // The claim: every command real bash used to damage the canary is refused.
    expect(named(live.filter((result) => result.refusal === undefined))).toEqual(
      [...PATH_ROW_RESIDUAL].sort()
    );

    // The other direction, at equal standing: nothing ordinary is refused for it.
    expect(
      named(results.filter((r) => r.row.role === "must-allow" && r.refusal !== undefined))
    ).toEqual([]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}, 180_000);
