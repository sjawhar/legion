import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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

// A file a command writes is modelled in one map the whole walk shares, and `runFile` prefers
// that model to what is on disk. So a write the shell may never perform, or may not have
// finished, may not record what the file holds: the model would carry a branch's text into the
// branch the shell took, and the guard would read a script that is not there. Each row below
// writes benign text (`echo hi`) into `unread.sh`, whose text on disk deletes the canary HOME,
// and then runs it. Real bash decides the row: a row bash leaves destroyed is one the guard must
// refuse, and a row bash leaves intact is one it must still allow.

/** The modelled write, benign. */
const WRITE = "echo 'echo hi' > unread.sh";
/** The same write as one double-quoted operand, so nesting it in `trap` adds no quoting. */
const QUOTED_WRITE = "\"echo 'echo hi' > unread.sh\"";
/** Runs what the guard modelled. */
const RUN = "bash unread.sh";
/** False under real bash: `safe.sh` holds PRESENT and never holds zzzNOPE. */
const FALSE_COND = "grep -q zzzNOPE safe.sh";
/** True under real bash. */
const TRUE_COND = "grep -q PRESENT safe.sh";
export const DESTRUCTIVE = 'rm -rf "$HOME"\n';

/** What the row does when real bash runs it: `destroyed` and `intact` are checked, and `racy` is
 * a write bash performs beside the read, whose outcome one run cannot settle — those rows are
 * held to bash having performed the write. */
type Live = "destroyed" | "intact" | "racy";

interface ModelRow {
  readonly name: string;
  readonly payload: string;
  readonly guard: "refused" | "allowed";
  readonly live: Live;
  /** Contents of `gen.sh` in the row's fixture, for a row whose handler a script sets. */
  readonly script?: string;
}

export const MODEL_ROWS: readonly ModelRow[] = [
  // Straight-line writes, both directions: the model is the only thing that can allow a row, and
  // the guard still reads what a command really writes.
  {
    name: "straight-line benign write",
    payload: `${WRITE}; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "straight-line destructive write",
    payload: `echo 'rm -rf "$HOME"' > unread.sh; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  { name: "no modelled write at all", payload: RUN, guard: "refused", live: "destroyed" },

  // Conditional and loop bodies: the shell may never enter them.
  {
    name: "&& rhs",
    payload: `${FALSE_COND} && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "&& rhs after a test",
    payload: `[ -f nosuch.conf ] && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "&& rhs after a mkdir",
    payload: `mkdir deep 2>/dev/null && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  { name: "|| rhs", payload: `true || ${WRITE}; ${RUN}`, guard: "refused", live: "destroyed" },
  {
    name: "if then-body",
    payload: `if ${FALSE_COND}; then ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "if else-body",
    payload: `if ${TRUE_COND}; then :; else ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "elif body",
    payload: `if ${TRUE_COND}; then :; elif ${FALSE_COND}; then ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "case arm the guard cannot decide",
    payload: `case "$(uname)" in NOSUCHOS) ${WRITE} ;; esac; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "while body",
    payload: `while ${FALSE_COND}; do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "until body",
    payload: `until ${TRUE_COND}; do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "for body over an unknown wordlist",
    payload: `for n in $(grep -o zzzNOPE safe.sh); do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "arithmetic for body",
    payload: `for ((i=0;i<0;i++)); do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "select body",
    payload: `select n in a; do ${WRITE}; break; done < /dev/null; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "handler for a signal that never arrives",
    payload: `( trap ${QUOTED_WRITE} USR1; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "definition only a branch that did not run leaves",
    payload: `if ${FALSE_COND}; then f() { ${WRITE}; }; fi; f 2>/dev/null; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },

  // Every site that records what a command writes, inside a body the shell may not enter.
  {
    name: "tee and a here-document in a branch",
    payload: `if ${FALSE_COND}; then tee unread.sh >/dev/null <<'EOF'\necho hi\nEOF\nfi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a rendered brace group in a branch",
    payload: `if ${FALSE_COND}; then { echo 'echo hi'; } > unread.sh; fi; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "cat and a here-document in a branch",
    payload: `${FALSE_COND} && cat > unread.sh <<'EOF'\necho hi\nEOF\n${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "printf in a branch",
    payload: `${FALSE_COND} && printf 'echo hi\\n' > unread.sh; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an append in a branch",
    payload: `${FALSE_COND} && echo 'echo hi' >> unread.sh; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },

  // Commands this shell does not wait for: the write happens, but maybe not before the read.
  { name: "a background command", payload: `${WRITE} & ${RUN}`, guard: "refused", live: "racy" },
  {
    name: "an earlier part of the same pipeline",
    payload: `${WRITE} | ${RUN}`,
    guard: "refused",
    live: "racy",
  },
  {
    name: "a coprocess",
    payload: `coproc C { ${WRITE}; }; ${RUN}`,
    guard: "refused",
    live: "racy",
  },

  // The straight-line path keeps its model: these must not be refused.
  {
    name: "an if clause, which always runs",
    payload: `if ${WRITE}; then :; fi; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  { name: "a subshell", payload: `( ${WRITE} ); ${RUN}`, guard: "allowed", live: "intact" },
  { name: "a brace group", payload: `{ ${WRITE}; }; ${RUN}`, guard: "allowed", live: "intact" },
  {
    name: "a command substitution",
    payload: `x=$( ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "a here-document's command substitution",
    payload: `cat > /dev/null <<EOF\n$( ${WRITE} )\nEOF\n${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "a function this shell called",
    payload: `f() { ${WRITE}; }; f; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "an EXIT handler",
    payload: `( trap ${QUOTED_WRITE} EXIT; : ); ${RUN}`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "a straight-line write after a branch wrote the same file",
    payload: `if ${FALSE_COND}; then echo 'rm -rf "$HOME"' > unread.sh; fi; ${WRITE}; ${RUN}`,
    guard: "allowed",
    live: "intact",
  },

  // The cost: a conditional generate-then-run whose condition really holds is refused with the
  // rest, since nothing tells the two apart before the shell runs. Its refusal names the remedy.
  {
    name: "a legitimate conditional generate-then-run (&&)",
    payload: `${TRUE_COND} && ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a legitimate conditional generate-then-run (if)",
    payload: `if ${TRUE_COND}; then ${WRITE}; fi; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a || rhs that does run",
    payload: `${FALSE_COND} || ${WRITE}; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a for body over known words",
    payload: `for n in a b; do ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "an earlier part of a pipeline the reader is not in",
    payload: `${WRITE} | cat; ${RUN}`,
    guard: "refused",
    live: "intact",
  },
  {
    name: "a definition the branch that ran left",
    payload: `if ${TRUE_COND}; then f() { ${WRITE}; }; fi; f 2>/dev/null; ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // A handler is set, not run, by the body it sits in. A branch the shell never enters sets no
  // handler at all, so even an `EXIT` one may never run — and a subshell's handlers and a
  // script's run mid-command, in front of a later read.
  {
    name: "an EXIT handler a branch that did not run set",
    payload: `( if ${FALSE_COND}; then trap ${QUOTED_WRITE} EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an EXIT handler an && right-hand side that did not run set",
    payload: `( ${FALSE_COND} && trap ${QUOTED_WRITE} EXIT; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an EXIT handler a script set in a branch that did not run",
    payload: `bash gen.sh; ${RUN}`,
    script: `if ${FALSE_COND}; then trap ${QUOTED_WRITE} EXIT; fi\n`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an EXIT handler a script set on its straight line",
    payload: `bash gen.sh; ${RUN}`,
    script: `trap ${QUOTED_WRITE} EXIT\n`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "an EXIT handler a branch that DID run set",
    payload: `( if ${TRUE_COND}; then trap ${QUOTED_WRITE} EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // Process substitution: bash waits for neither `>(…)` nor `<(…)`.
  {
    name: "an output process substitution",
    payload: `echo z > >( ${WRITE} ); ${RUN}`,
    guard: "refused",
    live: "racy",
  },
  {
    name: "an input process substitution",
    payload: `cat <( ${WRITE} ) >/dev/null; ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // An `&&` right-hand side the shell never runs leaves more behind than a file. The control for
  // each is the same command in an `if`, which the walk has always merged.
  {
    name: "a cd on an && right-hand side that did not run",
    payload: `cd "$HOME" && ${FALSE_COND} && cd "$LEGION_WORKSPACE"; rm -rf victim`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "the same cd in an if",
    payload: `cd "$HOME"; if ${FALSE_COND}; then cd "$LEGION_WORKSPACE"; fi; rm -rf victim`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an assignment on an && right-hand side that did not run",
    payload: `d="$HOME"; ${FALSE_COND} && d="$LEGION_WORKSPACE"; rm -rf "$d/victim"`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "the same assignment in an if",
    payload: `d="$HOME"; if ${FALSE_COND}; then d="$LEGION_WORKSPACE"; fi; rm -rf "$d/victim"`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a redefinition on an && right-hand side that did not run",
    payload: `f() { rm -rf "$HOME/victim"; }; ${FALSE_COND} && f() { :; }; f`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an && right-hand side whose value nothing reads",
    payload: `${FALSE_COND} && d="$LEGION_WORKSPACE"; rm -rf "$LEGION_WORKSPACE/x"`,
    guard: "allowed",
    live: "intact",
  },
  {
    name: "an && right-hand side that redirects a later rm",
    payload: `d="$HOME"; ${FALSE_COND} && d="$LEGION_WORKSPACE/junk"; rm -rf "$d"`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "an && right-hand side that cds, and does run",
    payload: `[ -d deep ] && cd deep; rm -rf junk`,
    guard: "refused",
    live: "intact",
  },

  // The mirror of a handler a branch registered: one this shell certainly registered, whose
  // removal sits in a body. The removal really happening is what makes the modelled write not
  // happen, so these conditions hold rather than fail.
  {
    name: "a removal of an EXIT handler on the straight line",
    payload: `( trap ${QUOTED_WRITE} EXIT; trap - EXIT; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in an if body that did run",
    payload: `( trap ${QUOTED_WRITE} EXIT; if ${TRUE_COND}; then trap - EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler on an && right-hand side that did run",
    payload: `( trap ${QUOTED_WRITE} EXIT; ${TRUE_COND} && trap - EXIT; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in a case arm",
    payload: `( trap ${QUOTED_WRITE} EXIT; case "$(uname)" in *) trap - EXIT ;; esac; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in a called function",
    payload: `( trap ${QUOTED_WRITE} EXIT; f() { trap - EXIT; }; f; : ); ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a removal of an EXIT handler in an if body that did not run",
    payload: `( trap ${QUOTED_WRITE} EXIT; if ${FALSE_COND}; then trap - EXIT; fi; : ); ${RUN}`,
    guard: "refused",
    live: "intact",
  },

  // Leaving a body early. `break` and `continue` are closed, because the walk reaches their
  // bodies through a body wrap; the rest are the residual this rule does not reach, and are
  // pinned as they behave so that closing one is a visible change rather than a silent one.
  {
    name: "a break before the write",
    payload: `for n in a; do break; ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a break before the write in a while body",
    payload: `while true; do break; ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "a continue before the write",
    payload: `for n in a b; do continue; ${WRITE}; done; ${RUN}`,
    guard: "refused",
    live: "destroyed",
  },
  {
    name: "RESIDUAL a return before the write",
    payload: `f() { return 0; ${WRITE}; }; f; ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL a return in a branch before the write",
    payload: `f() { [ -f nosuch.conf ] || return 0; ${WRITE}; }; f; ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL an exit before the write",
    payload: `( exit 0; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL set -e and a command that fails before the write",
    payload: `( set -e; ${FALSE_COND}; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
  {
    name: "RESIDUAL an exec before the write",
    payload: `( exec true; ${WRITE} ); ${RUN}`,
    guard: "allowed",
    live: "destroyed",
  },
];

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
    observed.push(`${row.name}: ${verdict === undefined ? "allowed" : "refused"}, bash ${live}`);
    expected.push(`${row.name}: ${row.guard}, bash ${row.live}`);
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
  ]);
}, 120_000);

// The other model a branch writes into: the pid a file holds, which `kill "$(<pid)"` reads. A
// branch that may rewrite it must not leave the straight line's pid standing as fact — and must
// not throw it away either, since when both what the branch writes and what was there before are
// pids this shell started, signalling the file is safe whichever ran. Real bash decides here too:
// each row reports what it actually put in the file and whether that pid is one of this shell's
// own children, so "this pane's own descendant" is bash's answer rather than an assumption.
export const START_PID = "sleep 5 & echo $! > pid";
export const KILL_PID = 'kill "$(<pid)"';
export const PID_ROWS = [
  {
    name: "a straight-line pid",
    payload: `${START_PID}; ${KILL_PID}`,
    guard: "allowed",
    ownChild: true,
  },
  {
    name: "a branch may rewrite it with another pid of this shell",
    payload: `${START_PID}; ${FALSE_COND} && echo $! > pid; ${KILL_PID}`,
    guard: "allowed",
    ownChild: true,
  },
  {
    name: "a branch may rewrite it with something that is not a pid",
    payload: `${START_PID}; ${FALSE_COND} && echo 1 > pid; ${KILL_PID}`,
    guard: "refused",
    ownChild: true,
  },
  {
    name: "only a branch ever wrote it",
    payload: `sleep 5 & ${FALSE_COND} && echo $! > pid; ${KILL_PID}`,
    guard: "refused",
    ownChild: false,
  },
];

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
