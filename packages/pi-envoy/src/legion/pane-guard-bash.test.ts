import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { createPaneGuard, type PaneGuard } from "./pane-guard";
import { DESTRUCTIVE, KILL_PID, type Live, MODEL_ROWS, PID_ROWS } from "./pane-guard-model-rows";
import {
  fixtureProperties,
  measureWriteRow,
  WRITE_ROWS,
  writeRefusalMatches,
} from "./pane-guard-write-rows";

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

test("a file a branch or an unwaited command writes is never modelled as its text", () => {
  const observed: string[] = [];
  const expected: string[] = [];
  for (const [index, row] of MODEL_ROWS.entries()) {
    const dir = path.join(base, `model-${index}`);
    const rowWorkspace = path.join(dir, "ws");
    const rowHome = path.join(dir, "home");
    const rowScratch = path.join(dir, "scratch");
    rmSync(dir, { recursive: true, force: true });
    for (const made of [rowWorkspace, rowHome, rowScratch, path.join(rowWorkspace, "deep")]) {
      mkdirSync(made, { recursive: true });
    }
    // The canary. Its absence afterwards is the only evidence of destruction, so a row that runs
    // nothing reads as intact and proves nothing.
    writeFileSync(path.join(rowHome, "victim"), "do-not-delete\n");
    writeFileSync(path.join(rowWorkspace, "unread.sh"), DESTRUCTIVE);
    writeFileSync(path.join(rowWorkspace, "safe.sh"), "PRESENT\n");
    if (row.script !== undefined) writeFileSync(path.join(rowWorkspace, "gen.sh"), row.script);

    const rowEnv: NodeJS.ProcessEnv = {
      HOME: rowHome,
      LEGION_WORKSPACE: rowWorkspace,
      TMPDIR: rowScratch,
      PATH: process.env.PATH,
    };
    const rowGuard = createPaneGuard({
      workspace: rowWorkspace,
      ompPid: process.pid,
      scratch: rowScratch,
    });
    const verdict = rowGuard.bash(row.payload, rowWorkspace, rowEnv);
    // Real bash, in this row's own fixture, reaching no home but the canary.
    spawnSync("bash", ["-c", row.payload], { cwd: rowWorkspace, env: rowEnv, timeout: 20_000 });
    const live: Live =
      row.live === "racy"
        ? readFileSync(path.join(rowWorkspace, "unread.sh"), "utf8") === DESTRUCTIVE
          ? "intact"
          : "racy"
        : existsSync(path.join(rowHome, "victim"))
          ? "intact"
          : "destroyed";
    // A write this shell does not wait for may finish before or after this read. Its guard verdict
    // stays the assertion; pinning which real-Bash timing won made the comparison flaky.
    const expectedLive = row.live === "racy" ? live : row.live;
    observed.push(`${row.name}: ${verdict === undefined ? "allowed" : "refused"}, bash ${live}`);
    expected.push(`${row.name}: ${row.guard}, bash ${expectedLive}`);
  }
  expect(observed).toEqual(expected);
  // Which shapes the rule does not reach is a claim about this batch, so it is read off the
  // batch rather than written down beside it: a row that becomes refused, or a new one that
  // arrives allowed while bash destroys the canary, fails here until the list says so.
  expect(
    MODEL_ROWS.filter((row) => row.guard === "allowed" && row.live === "destroyed").map(
      (row) => row.name
    )
  ).toEqual([
    "RESIDUAL a return before the write",
    "RESIDUAL a return in a branch before the write",
    "RESIDUAL an exit before the write",
    "RESIDUAL set -e and a command that fails before the write",
    "RESIDUAL an exec before the write",
    "RESIDUAL an exit in a branch before the write",
    "RESIDUAL set -o errexit and a command that fails before the write",
    "RESIDUAL set -u and an unset name before the write",
    "RESIDUAL set -e in a script whose caller ignores its status",
  ]);
}, 120_000);

// The other model a branch writes into: the pid a file holds, which `kill "$(<pid)"` reads. A
// branch that may rewrite it must not leave the straight line's pid standing as fact — and must
// not throw it away either, since when both what the branch writes and what was there before are
// pids this shell started, signalling the file is safe whichever ran. Real bash decides here too:
// each row reports what it actually put in the file and whether that pid is one of this shell's
// own children, so "this pane's own descendant" is bash's answer rather than an assumption.
test("a pid file a branch may rewrite is neither trusted nor forgotten", () => {
  const observed: string[] = [];
  const expected: string[] = [];
  for (const [index, row] of PID_ROWS.entries()) {
    const dir = path.join(base, `pid-${index}`);
    const rowWorkspace = path.join(dir, "ws");
    rmSync(dir, { recursive: true, force: true });
    mkdirSync(rowWorkspace, { recursive: true });
    writeFileSync(path.join(rowWorkspace, "safe.sh"), "PRESENT\n");
    const rowEnv: NodeJS.ProcessEnv = {
      HOME: path.join(dir, "home"),
      LEGION_WORKSPACE: rowWorkspace,
      PATH: process.env.PATH,
    };
    const rowGuard = createPaneGuard({
      workspace: rowWorkspace,
      ompPid: process.pid,
      scratch: path.join(dir, "scratch"),
    });
    const verdict = rowGuard.bash(row.payload, rowWorkspace, rowEnv);
    // The same payload with the `kill` replaced by a report of what it would have signalled and
    // of every child this shell holds, so the signal is never actually sent from a test.
    const probe = row.payload.replace(
      KILL_PID,
      'printf "%s|%s" "$(<pid)" "$(jobs -p | tr "\\n" ",")"'
    );
    const seen = spawnSync("bash", ["-c", probe], {
      cwd: rowWorkspace,
      env: rowEnv,
      encoding: "utf8",
    });
    const [wrote = "", children = ""] = seen.stdout.split("|");
    const ownChild = wrote !== "" && children.split(",").includes(wrote);
    observed.push(
      `${row.name}: ${verdict === undefined ? "allowed" : "refused"}, own child ${ownChild}`
    );
    expected.push(`${row.name}: ${row.guard}, own child ${row.ownChild}`);
  }
  expect(observed).toEqual(expected);
}, 60_000);

// The verbs that write a path they name (LEGION-357): `cp`, `dd of=`, `install`, `ln` and
// `sed -i`, each through the operand its own grammar makes the destination. Every row is measured
// twice — what the guard returns, and whether real bash changed anything under a canary HOME in
// the row's own fixture — so no row carries a written-down verdict for the dangerous direction.
// What bash did is the expectation, and a row that stops destroying the canary stops demanding a
// refusal rather than passing quietly — it fails the probe-is-live check below instead, so a row
// that has quietly stopped measuring anything is reported rather than counted as a pass.
test("a command that writes a path it names is judged, whatever grammar names it", () => {
  const results = WRITE_ROWS.map((row) => measureWriteRow(base, row, createPaneGuard));

  // The fixture, as booleans: no row means anything if its links point elsewhere, and a digest
  // over an empty home would make every row look intact.
  expect(Object.entries(fixtureProperties(base)).filter(([, held]) => !held)).toEqual([]);
  // The measurement discriminates: most rows really do change something outside the roots.
  expect(results.filter((result) => result.live).length).toBeGreaterThan(WRITE_ROWS.length / 2);
  expect(results.filter((r) => r.row.role === "probe" && !r.live).map((r) => r.row.name)).toEqual(
    []
  );
  expect(
    results.filter((r) => r.error !== undefined).map((r) => `${r.row.name}: ${r.error}`)
  ).toEqual([]);
  expect(
    results
      .filter((r) => r.row.role === "unreadable" && r.refusal === undefined)
      .map((r) => r.row.name)
  ).toEqual([]);
  expect(results.filter((r) => !writeRefusalMatches(r)).map((r) => r.row.name)).toEqual([]);

  // Every row the guard allows while bash changed the canary, by name. The list is the boundary
  // `docs/deployment.md` documents, not a tolerance: a path that does not exist yet overwrites nothing (`judgePath`'s
  // `overwrite`), and a link created and written through in the same command is not yet on disk
  // when the guard reads it. Existing links are covered by the separate-call copy probes.
  // A new name here is a leak; a name that leaves is a boundary someone moved on purpose.
  expect(
    results.filter((result) => result.live && result.refusal === undefined).map((r) => r.row.name)
  ).toEqual(WRITE_ROWS.filter((row) => row.role === "residual").map((row) => row.name));

  // The cost side, which nothing derived from bash can supply: a command the pane is meant to be
  // able to run, still allowed.
  expect(
    results
      .filter((result) => result.row.role === "must-allow" && result.refusal !== undefined)
      .map((result) => `${result.row.name}: ${result.refusal}`)
  ).toEqual([]);
}, 180_000);

// `dd` keeps its destination inside an operand word, so its refusal names that word whole, as
// `tar -C`'s does. That is why it is not in `pane-guard.test.ts`'s family matrix, whose contract
// is that a refusal names the target as written.
test("dd's refusal names the operand word that carries the path", () => {
  const home = path.join(base, "dd-home");
  const workspace = path.join(base, "dd-ws");
  mkdirSync(home, { recursive: true });
  mkdirSync(workspace, { recursive: true });
  writeFileSync(path.join(home, ".bashrc"), "profile\n");
  const ddGuard = createPaneGuard({ workspace, ompPid: process.pid, scratch });
  const reason = ddGuard.bash('dd if=/dev/zero of="$HOME/.bashrc"', workspace, {
    ...env,
    HOME: home,
    LEGION_WORKSPACE: workspace,
  });
  expect(reason).toContain('dd would write `of="$HOME/.bashrc"`');
  expect(reason).toContain(path.join(home, ".bashrc"));
});
