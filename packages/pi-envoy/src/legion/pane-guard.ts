/**
 * LEGION-121: the pane guard's filesystem and signal boundary. On 2026-09-13 a worker pane's probe
 * script ended in `rm -rf "$work" "$HOME"` and deleted most of the daemon user's home directory,
 * its SSH and commit-signing keys included, stopping every agent on the machine; the same day a
 * subagent's `pkill -x sleep` killed other agents' processes. The `tool_call` hook
 * (`extensions/legion.ts`) holds a Legion pane's `bash` commands, `eval` code, and `hub` process
 * starts to this module before they run.
 *
 * A pane may delete, move, truncate, overwrite, or recursively change the mode or owner of paths
 * under its writable roots only: its issue workspace (`LEGION_WORKSPACE`, `.jj` included) and any
 * directory below `/tmp` except `/tmp` itself, a glob over it, or the tmux and ssh socket
 * directories there. The guard cannot tell which permitted `/tmp` directory belongs to the pane. A
 * pane signals only processes descended from its own Oh My Pi process.
 *
 * Commands are parsed by `unbash`, a bash parser, and the guard follows what bash would do with
 * them: quoting, `$HOME`, `~`, variables assigned earlier in the same command (`$(mktemp -d)`
 * included), `cd`, brace expansion, command substitutions (which run), subshells, control flow,
 * functions, and the scripts a command runs (`bash <file>`, `sh -c '<text>'`, `source`, a heredoc
 * piped into a shell, a script executed by path, and python/node/bun scripts through
 * `pane-guard-code.ts`). A command the parser reports as malformed is refused, and so is a target
 * the guard cannot resolve (a variable read from input, a command's output): it never guesses.
 * A command whose walk visits more than `MAX_WALK_STEPS` nodes is refused, never allowed unread.
 *
 * A `trap` handler's body is judged once, against the state of the shell that set it after that
 * shell's last statement, where bash runs an EXIT handler; a subshell's handlers (a substitution,
 * `( … )`, a pipeline's part, a coprocess, a backgrounded command) are judged at the subshell's
 * end. The text judged is the one bash stores: a double-quoted handler's variables were expanded
 * when it was set, and a single-quoted one reads its own when it runs. So a handler set inside a
 * branch or loop body is judged with the variables that body gave it, which the merged state after
 * the construct may no longer know, except a name the shell assigns after the construct, whose new
 * value is the one it reads. A later branch that blurs the name expires the capture too, so
 * `…; fi; if [ -n "$x" ]; then t="$LEGION_WORKSPACE/b"; fi` is refused though both values are safe.
 * A handler lifted from a branch has that body's variables but not its arguments, which merge
 * as the branches leave them: `set -- a a; if true; then shift; trap 'rm -rf "$1"' EXIT; fi` is
 * refused though `$1` is safe either way, and a walk that decides the branch would not lift that.
 * A function a branch or loop body defines is every definition a path may have left: a call runs
 * each, and the command of that name where a path defined none. A handler that would be dangerous
 * only at an earlier exit (an `exit` before a later assignment makes its target safe) is not
 * caught: the guard does not model where a shell exits. A handler whose text the guard cannot read
 * is refused; one it can read that runs a command it cannot (`trap 'eval "$c"' EXIT`) is judged as
 * any such command is, the residual above.
 */
import {
  closeSync,
  existsSync,
  openSync,
  readFileSync,
  readSync,
  realpathSync,
  statSync,
} from "node:fs";
import * as path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import type {
  ArithmeticExpression,
  BraceGroup,
  Case,
  CaseItem,
  Command,
  CompoundList,
  Node,
  ParsedScript,
  Redirect,
  Statement,
  TestExpression,
  While,
  Word,
  WordPart,
} from "unbash";
import { parse } from "unbash";
import {
  evaluateTest,
  isAscii,
  removePattern,
  replacePattern,
  shellQuoted,
} from "./pane-guard-bash";
import { type CodeLanguage, scanCode, UNKNOWN_MARKER } from "./pane-guard-code";

/** One run of an expanded shell word: text the guard knows, a glob pattern, or a value it cannot
 * know until the shell runs (`why` names it for the refusal). A `$!` or a nested shell's `$$` is a
 * pid the guard knows is this pane's own descendant without knowing its number. A `lenient` value
 * came from Python or JavaScript the guard could not evaluate: code is judged only on what it can
 * evaluate (`pane-guard-code.ts`), so such a value is the documented residual, never a refusal. */
interface Piece {
  readonly kind: "literal" | "glob" | "unknown";
  readonly text: string;
  readonly why?: string;
  readonly descendantPid?: true;
  readonly lenient?: true;
}
type Expansion = readonly Piece[];

/** One argument of a command: the word as written (for the refusal) and one of its expansions
 * (brace expansion and `"$@"` give a word several). `fields` says when bash may not make it one
 * argument: `none` for an unquoted expansion that is empty, which bash drops, and `unknown` for a
 * glob or an unquoted expansion the guard cannot read, which bash may split into several. */
interface Arg {
  readonly text: string;
  readonly exp: Expansion;
  readonly fields?: "none" | "unknown";
}

interface Roots {
  /** The issue workspace's real path, or undefined when the pane names none usable. */
  readonly workspace: string | undefined;
  /** The scratch root's real path (`/tmp`). */
  readonly scratch: string;
  /** Prefixes of first path components under the scratch root that other processes own (sockets). */
  readonly protectedScratch: readonly string[];
  /** First path components under the scratch root this pane lives in, not scratch: the directory
   * holding its `HOME` or `TMUX_TMPDIR`, by exact name. */
  readonly protectedDirectories: readonly string[];
}

interface Ctx {
  readonly roots: Roots;
  readonly env: NodeJS.ProcessEnv;
  readonly ompPid: number;
  readonly parentOf: (pid: number) => number | undefined;
  readonly steps: { count: number };
}

interface State {
  vars: Map<string, Expansion>;
  exported: Set<string>;
  cwd: string | undefined;
  cwdWhy: string;
  positional: readonly Expansion[] | undefined;
  /** Files this command writes before it runs them: path to content, or null when the guard
   * cannot know what was written. Shared by every subshell of one command. */
  readonly files: Map<string, string | null>;
  backgroundStarted: boolean;
  /** Inside a shell this command starts (`bash -c`, a script): `$$` is a descendant. */
  readonly nested: boolean;
  readonly depth: number;
  /** The source the current script's node positions index, for snippets and line numbers. */
  readonly source: string;
  /** Absolute path of the file this shell is reading, when it has one. */
  readonly script: string | undefined;
  /** The `$0` a `bash -c` invocation supplies; a script otherwise names itself. */
  argv0: Expansion | undefined;
  /** Every output a function's substitution can emit. `undefined` means it emitted an unknown value. */
  output: readonly Expansion[] | undefined;
  /** Files this shell wrote with a pid it started, indexed by resolved path. */
  readonly pidFiles: Map<string, Expansion>;
  /** Per-element values of shell arrays. */
  readonly arrays: Map<string, Map<string, Expansion>>;
  /** Functions available in this shell, with their definition source for diagnostic locations. */
  functions: Map<string, Callee>;
  /** Functions whose current body walk has not returned; recursive calls add no new code to check. */
  readonly runningFunctions: Set<string>;
  /** The handlers this shell set with `trap`. Each body runs when this shell finishes, against its
   * state after its last statement; a subshell starts with none (`subshell`). */
  traps: readonly Trap[];
}

interface FunctionDefinition {
  readonly body: Node;
  readonly source: string;
  readonly file: string | undefined;
}

/** What a name runs as: each function definition a path through the command may have left, and
 * `undefined` for a path that left none, where the name runs as a command. A definition alone is
 * the ordinary case; a name a branch or loop body defines has more than one. */
type Callee = readonly (FunctionDefinition | undefined)[];

/** One definition per `Function` node, so a definition walked again (a function called in two
 * branches) is one candidate where they meet, not one per walk. */
const definitions = new WeakMap<Node, Callee>();

interface Trap {
  readonly text: string;
  readonly site: Site;
  /** The conditions it handles (`EXIT`, `INT`, …), as `trapSignal` names them. Every handler's
   * body is reachable, so each is walked; the set is what `trap - <signal>` removes. */
  readonly signals: readonly string[];
  readonly file: string;
  /** For a handler `merge` lifted out of a branch or loop body: that body's variables, and the
   * values the merge left for them. A handler reads its variables when it runs, so a body's value
   * stands only while the shell still holds the value that merge wrote (`handlerVars`). */
  readonly vars?: ReadonlyMap<string, Expansion>;
  readonly merged?: ReadonlyMap<string, Expansion>;
  /** Names bound to the pids of this shell's children a double-quoted handler expanded when it was
   * set (`trap "kill $pid" EXIT`), which its text reads back. */
  readonly pids?: ReadonlyMap<string, Expansion>;
}

/** A `trap` operand as bash names the condition: case aside, `0` is `EXIT`, and a signal may carry
 * its `SIG` prefix (`SIGINT` is `INT`). `SIGEXIT` is no condition bash knows, so it stays as
 * written and matches nothing. Signal numbers other than `0` are not translated, so a removal
 * naming one (`trap - 2`) removes nothing: the handler stays and is walked. */
function trapSignal(operand: string): string {
  const name = operand.toUpperCase();
  if (name === "0") return "EXIT";
  return name.startsWith("SIG") && name !== "SIGEXIT" ? name.slice(3) : name;
}

/** The condition a handler records for a `trap` operand the guard cannot read. `trapSignal`
 * names every readable one in capitals, and a removal leaves out the operands it cannot read, so
 * no removal names this: the handler stays and is walked. */
const UNREADABLE_CONDITION = "an unreadable condition";

/** What a refusal names: the simple command it came from, where that command sits, and the
 * rule. A refusal inside a script is rethrown by the command that ran it with the script's
 * path and line prepended to the detail. */
class Refusal extends Error {
  constructor(
    readonly snippet: string,
    readonly line: number,
    readonly detail: string,
    readonly file?: string
  ) {
    super(detail);
  }
}

function locateRefusal(error: unknown, file: string | undefined): never {
  if (!(error instanceof Refusal)) throw error;
  throw new Refusal(error.snippet, error.line, error.detail, error.file ?? file);
}

function wrapRefusal(error: unknown, site: Site, label: string): never {
  if (!(error instanceof Refusal)) throw error;
  const source = error.file ?? label;
  throw new Refusal(
    site.snippet,
    site.line,
    `line ${error.line} of ${source}, \`${error.snippet}\`: ${error.detail}`
  );
}

/** Where a check happens: the command's own text and line, for the refusal. */
interface Site {
  readonly snippet: string;
  readonly line: number;
}

const MAX_DEPTH = 8;
/** The syntax nodes one command's walk may visit before the guard refuses it as too large to judge.
 * The repository's largest tracked script, `scripts/e2e/stage4b-sandbox-tree.sh`, reaches its
 * first refusal after about 20,700, in about a third of a second through the guard on a loaded
 * devbox. What a node costs depends on what it does: 100,000 `true;` walk in about 200 ms, while a
 * budget's worth of `rm -rf a;`, each target resolved against the pane's roots, takes about
 * 490 ms. So the budget bounds a walk's size, not its time. */
const MAX_WALK_STEPS = 100_000;
const MAX_ALTERNATIVES = 64;
/** The longest value a pattern expansion is evaluated over; `//` tests every slice of it. */
const MAX_PATTERN_VALUE = 512;
/** The pattern tests one walk step stands for: a test costs about as much as 1/128 of a node, so a
 * `//` over the longest value spends about a thousand steps and the budget still bounds the time. */
const PATTERN_TESTS_PER_STEP = 128;
const MAX_SCRIPT_BYTES = 1024 * 1024;
const FRESH_TEMP_NAME = "tmp.XXXXXXXXXX";
/** First components under `/tmp` that hold other processes' sockets. */
const PROTECTED_SCRATCH = [
  "tmux-",
  "ssh-",
  "systemd-private-",
  "snap-private-tmp",
  ".X11-unix",
  ".ICE-unix",
  ".font-unix",
  ".XIM-unix",
  ".Test-unix",
];
const DEVICE_TARGETS = ["/dev/null", "/dev/stdout", "/dev/stderr", "/dev/stdin", "/dev/tty"];
// Sets, not object literals: the names come from the command, and an object would answer
// `constructor` or `toString`.
const SHELLS = new Set(["bash", "sh", "dash", "zsh", "ksh", "mksh", "ash"]);
/** Commands whose arguments name paths the guard's file rules judge, for `find -exec` and
 * `xargs`. */
const FILE_COMMANDS = new Set([
  "rm",
  "unlink",
  "mv",
  "shred",
  "truncate",
  "chmod",
  "chown",
  "chgrp",
]);
const SIGNAL_COMMANDS = new Set(["kill", "pkill", "killall", "killall5"]);

/** Commands that do not add a path to a function's stdout. */
const FUNCTION_OUTPUT_SILENT: Record<string, true> = {
  ":": true,
  break: true,
  cd: true,
  chmod: true,
  chgrp: true,
  chown: true,
  continue: true,
  declare: true,
  false: true,
  getopts: true,
  kill: true,
  killall: true,
  killall5: true,
  local: true,
  ln: true,
  mapfile: true,
  mkdir: true,
  mv: true,
  popd: true,
  pushd: true,
  read: true,
  readarray: true,
  readonly: true,
  return: true,
  rm: true,
  set: true,
  shift: true,
  shred: true,
  sleep: true,
  touch: true,
  trap: true,
  true: true,
  truncate: true,
  unlink: true,
  unset: true,
  wait: true,
};

const UNKNOWN_ARRAY_INDEX = "\u0000";

const HINT =
  "Name a path under $LEGION_WORKSPACE or under a /tmp directory of your own; the Legion pane " +
  "guard (LEGION-121) refuses destructive commands outside those roots.";

// --- Paths -----------------------------------------------------------------------------------

/** The real path of `abs`'s longest existing ancestor, with the rest appended. */
function realExisting(abs: string): string {
  const rest: string[] = [];
  let current = abs;
  for (;;) {
    try {
      return path.join(realpathSync(current), ...rest);
    } catch {
      if (current === "/") return abs;
      rest.unshift(path.basename(current));
      current = path.dirname(current);
    }
  }
}

/** `abs` with symlinks resolved; the final component is followed only when `followFinal` (rm and
 * mv act on a symlink itself, chmod -R and a redirection on what it points to). */
function realish(abs: string, followFinal: boolean): string {
  const resolved = path.resolve(abs);
  if (resolved === "/" || followFinal) return realExisting(resolved);
  return path.join(realExisting(path.dirname(resolved)), path.basename(resolved));
}

/** Whether a first component under the scratch root is one other processes own, or one holding
 * the pane's home or tmux directory. A component that is only the literal start of a glob or an
 * unknown value (`/tmp/t*`) is protected when it could still grow into such a name. */
function isProtectedScratchComponent(component: string, roots: Roots, partial = false): boolean {
  return (
    roots.protectedScratch.some(
      (name) => component.startsWith(name) || (partial && name.startsWith(component))
    ) ||
    roots.protectedDirectories.some(
      (name) => component === name || (partial && name.startsWith(component))
    )
  );
}

function inWorkspace(real: string, roots: Roots): boolean {
  const workspace = roots.workspace;
  return workspace !== undefined && (real === workspace || real.startsWith(`${workspace}/`));
}

/** Strictly under the scratch root, in a first component no other process owns. */
function inScratch(real: string, roots: Roots): boolean {
  if (!real.startsWith(`${roots.scratch}/`)) return false;
  const component = real.slice(roots.scratch.length + 1).split("/")[0] ?? "";
  return component !== "" && !isProtectedScratchComponent(component, roots);
}

function describeRoots(roots: Roots): string {
  return roots.workspace === undefined
    ? `a directory of your own under ${roots.scratch} (this pane names no usable issue workspace)`
    : `the issue workspace ${roots.workspace} and a directory of your own under ${roots.scratch}`;
}

type Verdict = { readonly ok: true } | { readonly ok: false; readonly resolution: string };

/** Whether an operation on `exp` stays inside the pane's writable roots. `overwrite` is a
 * redirection or `tee`: a device, or a file that does not exist yet, overwrites nothing. */
function judgePath(
  exp: Expansion,
  st: State,
  ctx: Ctx,
  options: { readonly follow: boolean; readonly overwrite: boolean }
): Verdict {
  const { roots } = ctx;
  const text = exp.map((piece) => piece.text).join("");
  const open = exp.findIndex((piece) => piece.kind !== "literal");
  if (open === -1) {
    if (text === "") return { ok: true };
    if (
      options.overwrite &&
      (DEVICE_TARGETS.includes(text) ||
        text.startsWith("/dev/fd/") ||
        text.startsWith("/proc/self/fd/"))
    ) {
      return { ok: true };
    }
    if (!text.startsWith("/") && st.cwd === undefined) {
      return {
        ok: false,
        resolution: `relative to a working directory unknown after ${st.cwdWhy}`,
      };
    }
    const abs = path.resolve(st.cwd ?? "/", text);
    const real = realish(abs, options.follow || text.endsWith("/"));
    if (inWorkspace(real, roots) || inScratch(real, roots)) return { ok: true };
    if (options.overwrite && !existsSync(real)) return { ok: true };
    return { ok: false, resolution: real === abs ? real : `${abs}, which resolves to ${real}` };
  }
  const piece = exp[open] as Piece;
  const prefix = exp
    .slice(0, open)
    .map((p) => p.text)
    .join("");
  const slash = prefix.lastIndexOf("/");
  if (exp.some((p) => p.lenient) && prefix === "") return { ok: true };
  if (piece.kind === "unknown" && prefix === "") {
    return { ok: false, resolution: piece.why ?? "a value the guard cannot know" };
  }
  if (!prefix.startsWith("/") && st.cwd === undefined) {
    return { ok: false, resolution: `relative to a working directory unknown after ${st.cwdWhy}` };
  }
  const component = prefix.slice(slash + 1);
  let stem = path.resolve(
    st.cwd ?? "/",
    slash === -1 ? "." : slash === 0 ? "/" : prefix.slice(0, slash)
  );
  // A glob component that starts with a dot can match `..`.
  if (piece.kind === "glob" && (component === "" ? piece.text : component).startsWith(".")) {
    stem = path.dirname(stem);
  }
  const real = realish(stem, true);
  const allowed =
    inWorkspace(real, roots) ||
    inScratch(real, roots) ||
    (real === roots.scratch &&
      component !== "" &&
      !isProtectedScratchComponent(component, roots, true));
  if (allowed) return { ok: true };
  const what =
    piece.kind === "glob"
      ? `a glob over ${real}`
      : `${piece.why ?? "a value the guard cannot know"}, under ${real}`;
  return {
    ok: false,
    resolution:
      real === roots.scratch
        ? `${what}, which holds other sessions' directories and sockets`
        : what,
  };
}

// --- Word expansion --------------------------------------------------------------------------

const literal = (text: string): Piece => ({ kind: "literal", text });
const unknown = (why: string, descendantPid?: true): Piece =>
  descendantPid
    ? { kind: "unknown", text: "", why, descendantPid }
    : { kind: "unknown", text: "", why };

/** Unquoted literal source text: backslash escapes, glob characters, and (at the start of the
 * word, or after an assignment's `=`) tilde expansion. */
function unquotedLiteral(
  source: string,
  tildeAt: number | undefined,
  st: State,
  ctx: Ctx
): Piece[] {
  const pieces: Piece[] = [];
  let text = source;
  if (tildeAt !== undefined && text.charAt(tildeAt) === "~") {
    const head = text.slice(0, tildeAt);
    if (head !== "") pieces.push(literal(head));
    const slash = text.indexOf("/", tildeAt);
    const user = text.slice(tildeAt + 1, slash === -1 ? text.length : slash);
    if (user === "") {
      const home = lookup("HOME", st, ctx);
      pieces.push(...(home ?? [unknown("`~` (HOME is not set)")]));
    } else if (user === "+" && st.cwd !== undefined) pieces.push(literal(st.cwd));
    else pieces.push(unknown(`\`~${user}\``));
    text = slash === -1 ? "" : text.slice(slash);
  }
  let run = "";
  for (let i = 0; i < text.length; i += 1) {
    const char = text.charAt(i);
    if (char === "\\" && i + 1 < text.length) {
      i += 1;
      run += text.charAt(i);
    } else if (char === "*" || char === "?" || char === "[") {
      if (run !== "") pieces.push(literal(run));
      run = "";
      pieces.push({ kind: "glob", text: char });
    } else run += char;
  }
  if (run !== "") pieces.push(literal(run));
  return pieces;
}

/** A variable's value: this command's assignments, then the pane's environment. */
function lookup(name: string, st: State, ctx: Ctx): Expansion | undefined {
  const own = st.vars.get(name);
  if (own !== undefined) return own;
  if (name === "PWD" && st.cwd !== undefined) return [literal(st.cwd)];
  const value = ctx.env[name];
  return value === undefined ? undefined : [literal(value)];
}

function resolveArrayIndex(index: string | undefined, st: State, ctx: Ctx): string | undefined {
  if (index === undefined) return undefined;
  if (/^[0-9]+$/.test(index)) return index;
  const match = /^\$([A-Za-z_][A-Za-z0-9_]*)$/.exec(index);
  return match === null ? undefined : literalText(lookup(match[1] as string, st, ctx));
}

/** Every alternative of `$name` / `${name}`; `quoted` keeps an unquoted value from being taken
 * as one word when bash would split or glob it. */
function parameter(name: string, quoted: boolean, st: State, ctx: Ctx): Piece[][] {
  if (name === "@" || name === "*") {
    if (st.positional === undefined) return [[unknown(`\`$${name}\` (the positional parameters)`)]];
    return st.positional.length === 0 ? [[literal("")]] : st.positional.map((p) => [...p]);
  }
  if (/^[0-9]+$/.test(name)) {
    if (name === "0") return [[...(st.argv0 ?? [literal(st.script ?? "bash")])]];
    const value = st.positional?.[Number(name) - 1];
    if (value !== undefined) return [[...value]];
    // A shell whose arguments the guard knows has none past the last: that parameter is empty.
    if (st.positional !== undefined) return [[literal("")]];
    return [[unknown(`\`$${name}\` (a positional parameter)`)]];
  }
  if (name === "$") {
    return [st.nested ? [unknown("`$$`", true)] : [literal(String(ctx.ompPid))]];
  }
  if (name === "!") {
    return [[unknown("`$!` (the last background job)", st.backgroundStarted ? true : undefined)]];
  }
  if (name === "#" && st.positional !== undefined) return [[literal(String(st.positional.length))]];
  if (["?", "#", "-"].includes(name)) return [[unknown(`\`$${name}\``)]];
  const value = lookup(name, st, ctx);
  if (value === undefined) {
    return [[unknown(`\`$${name}\`, which is not set in this command or the pane's environment`)]];
  }
  if (quoted) return [[...value]];
  // Unquoted: bash splits a value with whitespace into several words and globs each.
  const pieces: Piece[] = [];
  for (const piece of value) {
    if (piece.kind !== "literal") pieces.push(piece);
    else if (/\s/.test(piece.text)) {
      pieces.push(unknown(`unquoted \`$${name}\`, which splits into several words`));
    } else pieces.push(...unquotedLiteral(piece.text.replaceAll("\\", "\\\\"), undefined, st, ctx));
  }
  return [pieces];
}

/** A parameter's value as an operator (`${1:-x}`) tests it: a special parameter from what the
 * shell holds (`$1` from its arguments, unset past the last), any other name from `lookup`. */
function operatorValue(name: string, st: State, ctx: Ctx): Expansion | undefined {
  if (!/^([0-9]+|[@*#?$!-])$/.test(name)) return lookup(name, st, ctx);
  if (/^[1-9][0-9]*$/.test(name) && st.positional !== undefined) {
    return st.positional[Number(name) - 1];
  }
  const alternatives = parameter(name, true, st, ctx);
  return alternatives.length === 1 ? alternatives[0] : [unknown(`\`$${name}\``)];
}

function parameterExpansion(
  part: Extract<WordPart, { type: "ParameterExpansion" }>,
  quoted: boolean,
  st: State,
  ctx: Ctx
): Piece[][] {
  const name = part.parameter;
  if (name === "BASH_SOURCE" && part.index === "0" && st.script !== undefined) {
    return [[literal(st.script)]];
  }
  const array = st.arrays.get(name);
  if (part.index === "@" || part.index === "*") {
    if (array !== undefined) {
      const unknownElement = array.get(UNKNOWN_ARRAY_INDEX);
      if (unknownElement !== undefined) return [[...unknownElement]];
      const values = [...array.values()];
      return values.length === 0 ? [[literal("")]] : values.map((value) => [...value]);
    }
    return parameter(name, quoted, st, ctx);
  }
  if (part.index !== undefined) {
    const index = resolveArrayIndex(part.index, st, ctx);
    if (array !== undefined && index !== undefined) {
      return [[...(array.get(index) ?? array.get(UNKNOWN_ARRAY_INDEX) ?? [literal("")])]];
    }
    return [[unknown(`\`${part.text}\``)]];
  }
  if (part.length || part.indirect || part.slice) {
    return [[unknown(`\`${part.text}\``)]];
  }
  if (part.replace !== undefined || ["#", "##", "%", "%%"].includes(part.operator ?? "")) {
    return patternExpansion(part, quoted, st, ctx);
  }
  const operator = part.operator;
  if (operator === undefined) return parameter(name, quoted, st, ctx);
  const value = operatorValue(name, st, ctx);
  const set = value !== undefined;
  const nonEmpty = set && value.map((p) => p.text).join("") !== "";
  const known = !set || value.every((p) => p.kind === "literal");
  // A value holding known text or a pid this shell started is non-empty whatever else it holds.
  const surelyNonEmpty =
    set && value.some((p) => p.descendantPid === true || (p.kind === "literal" && p.text !== ""));
  const operand = (): Piece[][] =>
    part.operand === undefined ? [[literal("")]] : expandWord(part.operand, st, ctx);
  switch (operator) {
    case ":-":
    case ":=":
      if (surelyNonEmpty) return parameter(name, quoted, st, ctx);
      if (!known) return [[unknown(`\`${part.text}\``)]];
      return nonEmpty ? parameter(name, quoted, st, ctx) : operand();
    case "-":
    case "=":
      // Only whether the parameter is set decides, and that the guard knows.
      return set ? parameter(name, quoted, st, ctx) : operand();
    case ":+":
      if (surelyNonEmpty) return operand();
      if (!known) return [[unknown(`\`${part.text}\``)]];
      return nonEmpty ? operand() : [[literal("")]];
    case "+":
      return set ? operand() : [[literal("")]];
    case ":?":
    case "?":
      return parameter(name, quoted, st, ctx);
    default:
      return [[unknown(`\`${part.text}\``)]];
  }
}

/** `${name#p}`, `##`, `%` and `%%`, and `${name/p/r}`, `//`, `/#` and `/%`, evaluated as bash does
 * when the value, the pattern and the replacement are all known; unknown otherwise, and for a
 * replacement holding `&` or `\`, which bash 5.2's `patsub_replacement` may rewrite. The parser's
 * split must account for the expansion's own text, and after `/` or `//` the pattern must be
 * non-empty: bash reads a `/` there as the pattern's first character (`${v////x}` replaces every
 * `/`), where the parser reads an empty pattern, and a value left as it was is the one the caller
 * already trusts. */
function patternExpansion(
  part: Extract<WordPart, { type: "ParameterExpansion" }>,
  quoted: boolean,
  st: State,
  ctx: Ctx
): Piece[][] {
  const unknownResult = [[unknown(`\`${part.text}\``)]];
  const operator = part.operator ?? "";
  const head = `\${${part.parameter}${operator}`;
  const accounted =
    part.replace === undefined
      ? part.text === `${head}${part.operand?.text ?? ""}}`
      : part.text === `${head}${part.replace.pattern.text}/${part.replace.replacement.text}}` ||
        (part.replace.replacement.text === "" &&
          part.text === `${head}${part.replace.pattern.text}}`);
  const slashPattern = operator === "/" || operator === "//";
  if (!accounted || (slashPattern && part.replace?.pattern.text === "")) return unknownResult;
  // An argument past the last one this shell knows it was given is unset, which bash expands empty.
  const pastLast = /^[1-9][0-9]*$/.test(part.parameter) && st.positional !== undefined;
  const raw = operatorValue(part.parameter, st, ctx);
  const value = raw === undefined && pastLast ? "" : literalText(raw);
  if (value === undefined || value.length > MAX_PATTERN_VALUE || !isAscii(value)) {
    return unknownResult;
  }
  const patternWord = part.replace?.pattern ?? part.operand;
  const patterns = patternWord === undefined ? [[literal("")]] : expandWord(patternWord, st, ctx);
  const source = patterns.length === 1 ? patternSource(patterns[0] as Expansion) : undefined;
  if (source === undefined || (slashPattern && source === "")) return unknownResult;
  const n = value.length;
  const tests = slashPattern ? (n * (n + 1)) / 2 + 1 : n + 1;
  ctx.steps.count += Math.ceil(tests / PATTERN_TESTS_PER_STEP);
  // Past the budget the value is not computed; the walk's next node refuses the command.
  if (ctx.steps.count > MAX_WALK_STEPS) return unknownResult;
  const pattern = new RegExp(`^${source}$`);
  let result: string;
  if (part.replace === undefined) {
    result = removePattern(value, operator, pattern);
  } else {
    const replacements = expandWord(part.replace.replacement, st, ctx);
    const pieces = replacements.length === 1 ? (replacements[0] as Expansion) : undefined;
    if (pieces === undefined || pieces.some((piece) => piece.kind === "unknown")) {
      return unknownResult;
    }
    const replacement = pieces.map((piece) => piece.text).join("");
    if (/[&\\]/.test(replacement)) return unknownResult;
    result = replacePattern(value, operator, pattern, replacement);
  }
  if (quoted) return [[literal(result)]];
  if (/\s/.test(result)) {
    return [[unknown(`unquoted \`${part.text}\`, which splits into several words`)]];
  }
  return [unquotedLiteral(result.replaceAll("\\", "\\\\"), undefined, st, ctx)];
}

/** A command substitution's value: `$(mktemp ...)` is a fresh path in its directory and
 * `$(pwd)` the working directory; script-location helpers resolve while the guard reads a script.
 * Anything else is a command's output, unknown. */
function substitution(
  script: ParsedScript | undefined,
  text: string,
  st: State,
  ctx: Ctx
): Piece[][] {
  const pidRead = /^\$\(<(.+)\)$/.exec(text);
  const variableRead = /^\$\(<"?\$([A-Za-z_][A-Za-z0-9_]*)"?\)$/.exec(text);
  const directFile = pidRead?.[1]?.includes("$") === true ? undefined : pidRead?.[1];
  const file =
    directFile ??
    (variableRead === null ? undefined : literalText(lookup(variableRead[1] as string, st, ctx)));
  if (file !== undefined && st.cwd !== undefined) {
    const pid = st.pidFiles.get(path.resolve(st.cwd, file));
    if (pid !== undefined) return [[...pid]];
  }
  const only = script?.commands.length === 1 ? script.commands[0]?.command : undefined;
  const invocation = substitutionInvocation(only, st, ctx);
  const callee = invocation === undefined ? undefined : st.functions.get(invocation.base);
  if (callee !== undefined && invocation !== undefined) {
    const alternatives: Piece[][] = [];
    let external = false;
    for (const definition of callee) {
      const output =
        definition === undefined
          ? undefined
          : functionOutput(invocation.base, definition, invocation.rest, st, ctx);
      if (output !== undefined) alternatives.push(...output.map((value) => [...value]));
      else external = true;
    }
    return external
      ? [...alternatives, ...commandOutput(only, invocation, text, st, ctx)]
      : alternatives;
  }
  return commandOutput(only, invocation, text, st, ctx);
}

/** The value of a substitution that runs a command rather than one of this shell's functions. */
function commandOutput(
  only: Node | undefined,
  invocation: ReturnType<typeof substitutionInvocation>,
  text: string,
  st: State,
  ctx: Ctx
): Piece[][] {
  if (invocation !== undefined) {
    if (invocation.base === "pwd" && invocation.rest.length === 0 && st.cwd !== undefined) {
      return [[literal(st.cwd)]];
    }
    if (invocation.base === "mktemp") return [mktempPath(invocation.command, st, ctx)];
    const resolved = substitutionPath(invocation.base, invocation.rest, st);
    if (resolved !== undefined) return [[literal(resolved)]];
  }
  if (only?.type === "AndOr" && only.operators.length === 1 && only.operators[0] === "&&") {
    const [left, right] = only.commands;
    const cd = substitutionInvocation(left, st, ctx);
    const pwd = substitutionInvocation(right, st, ctx);
    if (cd?.base === "cd" && pwd?.base === "pwd" && pwd.rest.length === 0 && cd.rest.length === 1) {
      const target = literalText(cd.rest[0]?.exp);
      if (target !== undefined && st.cwd !== undefined) {
        return [[literal(path.resolve(st.cwd, target))]];
      }
    }
  }
  return [[unknown(`\`${text}\` (a command's output)`)]];
}

function substitutionInvocation(
  node: Node | undefined,
  st: State,
  ctx: Ctx
): { readonly base: string; readonly command: Command; readonly rest: readonly Arg[] } | undefined {
  if (node?.type !== "Command" || node.name === undefined || node.prefix.length !== 0)
    return undefined;
  const argv = args([node.name, ...node.suffix], st, ctx);
  const name = literalText(argv[0]?.exp);
  if (name === undefined) return undefined;
  return { base: path.basename(name), command: node, rest: argv.slice(1) };
}

function substitutionPath(base: string, list: readonly Arg[], st: State): string | undefined {
  if (!["dirname", "basename", "realpath", "readlink"].includes(base)) return undefined;
  const target = literalText(operands(list, "").operands.at(-1)?.exp);
  if (target === undefined || st.cwd === undefined) return undefined;
  // An empty operand names no path: dirname prints `.`, basename nothing, and realpath and
  // readlink fail and print nothing.
  if (target === "") return base === "dirname" ? "." : "";
  const abs = path.resolve(st.cwd, target);
  if (base === "dirname") return path.dirname(abs);
  if (base === "basename") return path.basename(abs);
  if (base === "readlink" && !list.some((arg) => literalText(arg.exp)?.includes("f")))
    return undefined;
  return realExisting(abs);
}

function mktempPath(command: Command, st: State, ctx: Ctx): Piece[] {
  const words = command.suffix;
  let directory: Piece[] | undefined;
  for (let i = 0; i < words.length; i += 1) {
    const word = words[i] as Word;
    const value = word.value;
    if (value === "-p" || value === "--tmpdir") {
      const next = words[i + 1];
      if (value === "-p" && next !== undefined) {
        directory = expandWord(next, st, ctx)[0] ?? [];
        i += 1;
      }
    } else if (value.startsWith("--tmpdir=")) directory = [literal(value.slice(9))];
    else if (value.startsWith("-p") && value.length > 2) directory = [literal(value.slice(2))];
    else if (!value.startsWith("-") && value.includes("/")) {
      directory = [literal(path.resolve(st.cwd ?? "/", path.dirname(value)))];
    }
  }
  const base = directory ?? [literal(ctx.env.TMPDIR ?? "/tmp")];
  return [...base, literal(`/${FRESH_TEMP_NAME}`)];
}

/** Brace expansion of a `{a,b,c}` part into its alternatives; anything else (a sequence, a nested
 * expansion) is a glob, since the guard cannot list what it produces. */
function braceAlternatives(part: Extract<WordPart, { type: "BraceExpansion" }>): Piece[][] {
  const inner = part.text.slice(1, -1);
  const simple =
    part.text.startsWith("{") &&
    part.text.endsWith("}") &&
    !/[{}$`"'\\*?[]/.test(inner) &&
    inner.includes(",");
  if (!simple) return [[{ kind: "glob", text: part.text }]];
  return inner.split(",").map((alternative) => [literal(alternative)]);
}

function expandPart(part: WordPart, first: boolean, st: State, ctx: Ctx): Piece[][] {
  switch (part.type) {
    case "Literal":
      return [unquotedLiteral(part.text, first ? 0 : undefined, st, ctx)];
    case "SingleQuoted":
    case "AnsiCQuoted":
      return [[literal(part.value)]];
    case "DoubleQuoted":
    case "LocaleString": {
      let alternatives: Piece[][] = [[]];
      for (const child of part.parts) {
        const options: Piece[][] =
          child.type === "Literal"
            ? [[literal(child.value)]]
            : child.type === "SimpleExpansion"
              ? parameter(child.text.replace(/^\$\{?|\}$/g, ""), true, st, ctx)
              : child.type === "ParameterExpansion"
                ? parameterExpansion(child, true, st, ctx)
                : child.type === "CommandExpansion"
                  ? substitution(child.script, child.text, st, ctx)
                  : [[unknown(`\`${child.text}\``)]];
        alternatives = product(alternatives, options);
      }
      return alternatives;
    }
    case "SimpleExpansion":
      return parameter(part.text.replace(/^\$\{?|\}$/g, ""), false, st, ctx);
    case "ParameterExpansion":
      return parameterExpansion(part, false, st, ctx);
    case "CommandExpansion":
      return substitution(part.script, part.text, st, ctx);
    case "ArithmeticExpansion":
      return [[unknown(`\`${part.text}\` (arithmetic)`)]];
    case "ProcessSubstitution":
      return [[literal("/dev/fd/63")]];
    case "ExtendedGlob":
      return [[{ kind: "glob", text: part.text }]];
    case "BraceExpansion":
      return braceAlternatives(part);
  }
}

function product(left: Piece[][], right: Piece[][]): Piece[][] {
  const out: Piece[][] = [];
  for (const a of left) {
    for (const b of right) {
      if (out.length >= MAX_ALTERNATIVES) {
        return [[unknown(`a brace expansion with more than ${MAX_ALTERNATIVES} alternatives`)]];
      }
      out.push([...a, ...b]);
    }
  }
  return out;
}

/** Every expansion of `word` bash could produce, as piece lists. Literal text holding the
 * unknown marker came from a part an outer command builds at run time (`runtimeText`), so it is
 * unknown here too, whatever quoting it now sits in. */
function expandWord(word: Word, st: State, ctx: Ctx): Piece[][] {
  let alternatives: Piece[][] = [[]];
  if (word.parts === undefined) alternatives = [unquotedLiteral(word.text, 0, st, ctx)];
  else {
    word.parts.forEach((part, index) => {
      alternatives = product(alternatives, expandPart(part, index === 0, st, ctx));
    });
  }
  return alternatives.map((pieces) =>
    pieces.map((piece) =>
      piece.kind === "literal" && piece.text.includes(UNKNOWN_MARKER)
        ? unknown("a part of the text its outer command builds at run time")
        : piece
    )
  );
}

/** Text a command hands to a shell (`-c`, `eval`) or an interpreter (`-c`, `-e`): what the
 * guard knows of it, with each part it cannot know replaced by the unknown marker, which reads
 * back as unknown wherever the inner script uses it. */
function runtimeText(exp: Expansion): string {
  return exp.map((piece) => (piece.kind === "unknown" ? UNKNOWN_MARKER : piece.text)).join("");
}

/** Word parts bash splits into fields when they stand unquoted. */
const SPLIT_PARTS = new Set([
  "SimpleExpansion",
  "ParameterExpansion",
  "CommandExpansion",
  "ArithmeticExpansion",
]);

function args(words: readonly Word[], st: State, ctx: Ctx): Arg[] {
  return words.flatMap((word) => {
    const parts = word.parts ?? [];
    const unquoted = parts.some((part) => SPLIT_PARTS.has(part.type));
    const onlyExpansions = parts.length > 0 && parts.every((part) => SPLIT_PARTS.has(part.type));
    return expandWord(word, st, ctx).map((exp): Arg => {
      if (exp.some((piece) => piece.kind === "glob")) {
        return { text: word.text, exp, fields: "unknown" };
      }
      // Unquoted, an expansion bash splits on whitespace (`$*`, `$(…)`) or that the guard cannot read
      // may be several arguments.
      if (unquoted && exp.some((piece) => piece.kind === "unknown" || /[ \t\n]/.test(piece.text))) {
        return { text: word.text, exp, fields: "unknown" };
      }
      if (onlyExpansions && exp.every((piece) => piece.text === "")) {
        return { text: word.text, exp, fields: "none" };
      }
      return { text: word.text, exp };
    });
  });
}

/** The positional parameters a list of arguments gives a script, function or `set --`: undefined
 * when bash may make a different number of them (`fields`), so no parameter past the ones the
 * guard saw is read as empty while bash holds a value there. */
function positionalOf(list: readonly Arg[]): Expansion[] | undefined {
  if (list.some((arg) => arg.fields === "unknown")) return undefined;
  return list.filter((arg) => arg.fields !== "none").map((arg) => arg.exp);
}

function literalText(exp: Expansion | undefined): string | undefined {
  if (exp === undefined || exp.some((piece) => piece.kind !== "literal")) return undefined;
  return exp.map((piece) => piece.text).join("");
}

// --- Nested scripts in words -------------------------------------------------------------------

/** Walks the scripts a word runs while it expands: command and process substitutions, and those
 * nested in parameter operands, brace expansions, and arithmetic. */
function visitParts(parts: readonly WordPart[] | undefined, st: State, ctx: Ctx): void {
  for (const part of parts ?? []) {
    switch (part.type) {
      case "DoubleQuoted":
      case "LocaleString":
        visitParts(part.parts, st, ctx);
        break;
      case "CommandExpansion":
      case "ProcessSubstitution":
        walkSubstitution(part.script, part.inner, part.text, st, ctx);
        break;
      case "ArithmeticExpansion":
        visitArithmetic(part.expression, st, ctx);
        break;
      case "ParameterExpansion":
        visitParts(part.indexParts, st, ctx);
        if (part.operand !== undefined) visitParts(part.operand.parts, st, ctx);
        if (part.slice !== undefined) {
          visitParts(part.slice.offset.parts, st, ctx);
          visitParts(part.slice.length?.parts, st, ctx);
        }
        if (part.replace !== undefined) {
          visitParts(part.replace.pattern.parts, st, ctx);
          visitParts(part.replace.replacement.parts, st, ctx);
        }
        break;
      case "BraceExpansion":
      case "ExtendedGlob":
        visitParts(part.parts, st, ctx);
        break;
      default:
        break;
    }
  }
}

function visitWord(word: Word | undefined, st: State, ctx: Ctx): void {
  if (word !== undefined) visitParts(word.parts, st, ctx);
}

function visitArithmetic(expression: ArithmeticExpression | undefined, st: State, ctx: Ctx): void {
  if (expression === undefined) return;
  switch (expression.type) {
    case "ArithmeticBinary":
      visitArithmetic(expression.left, st, ctx);
      visitArithmetic(expression.right, st, ctx);
      break;
    case "ArithmeticUnary":
      visitArithmetic(expression.operand, st, ctx);
      break;
    case "ArithmeticTernary":
      visitArithmetic(expression.test, st, ctx);
      visitArithmetic(expression.consequent, st, ctx);
      visitArithmetic(expression.alternate, st, ctx);
      break;
    case "ArithmeticGroup":
      visitArithmetic(expression.expression, st, ctx);
      break;
    case "ArithmeticWord":
      visitParts(expression.parts, st, ctx);
      break;
    case "ArithmeticCommandExpansion":
      walkSubstitution(expression.script, expression.inner, expression.text, st, ctx);
      break;
  }
}

function visitTest(expression: TestExpression, st: State, ctx: Ctx): void {
  switch (expression.type) {
    case "TestUnary":
      visitWord(expression.operand, st, ctx);
      break;
    case "TestBinary":
      visitWord(expression.left, st, ctx);
      visitWord(expression.right, st, ctx);
      break;
    case "TestLogical":
      visitTest(expression.left, st, ctx);
      visitTest(expression.right, st, ctx);
      break;
    case "TestNot":
      visitTest(expression.operand, st, ctx);
      break;
    case "TestGroup":
      visitTest(expression.expression, st, ctx);
      break;
  }
}

function walkSubstitution(
  script: ParsedScript | undefined,
  inner: string | undefined,
  text: string,
  st: State,
  ctx: Ctx
): void {
  const parsed = script ?? (inner === undefined ? undefined : parse(inner));
  if (parsed === undefined) {
    throw new Refusal(text, 1, `the guard could not read the command in \`${text}\``);
  }
  const source = parsed.source ?? (script === undefined ? (inner ?? "") : st.source);
  walkScript(parsed, subshell(st, { source }), ctx);
}

// --- Statements --------------------------------------------------------------------------------

function clone(st: State): State {
  return {
    ...st,
    vars: new Map(st.vars),
    exported: new Set(st.exported),
    arrays: new Map([...st.arrays].map(([name, elements]) => [name, new Map(elements)] as const)),
    functions: new Map(st.functions),
    traps: [...st.traps],
    runningFunctions: new Set(st.runningFunctions),
    pidFiles: st.pidFiles,
  };
}

/** Anything a copy-back site does not carry out of a child is a candidate hole. Three sites carry
 * a child's state back into this shell, `merge`, `runFunction` and `runFile`'s sourced branch, and
 * each lists `State`'s fields by hand. A field added to `State` belongs in all three; or in the
 * list of what each leaves out and why; or, like `nested`, `depth`, `source`, `script` and
 * `argv0`, it says where the walk is rather than what the shell holds, and crosses no site at all.
 *
 * After branches that may or may not run: everything a branch leaves in this shell, since any of
 * them may be the one that ran. A variable, the positional parameters (`shift`, `set --`) or the
 * working directory the branches leave differently is unknown, a function name holds every
 * definition they leave (`Callee`), and a handler any of them sets stays. A variable every branch
 * leaves empty or a pid this shell started (`$!` in a retry loop, empty had it not run) stays a pid
 * that is safe to signal. Not merged: `files` and `pidFiles`, which every branch shares; `output`,
 * which each caller compares itself (`outputChanged`); and `runningFunctions`, which only a
 * function's own walk changes. */
function merge(target: State, branches: readonly State[]): void {
  const names = new Set<string>();
  for (const branch of branches) for (const name of branch.vars.keys()) names.add(name);
  for (const name of names) {
    const values = branches.map((branch) => JSON.stringify(branch.vars.get(name) ?? null));
    const first = branches[0]?.vars.get(name);
    if (first !== undefined && values.every((value) => value === values[0])) {
      target.vars.set(name, first);
    } else {
      const signalSafe = branches.every((branch) => {
        const value = branch.vars.get(name);
        return (
          value !== undefined &&
          (literalText(value) === "" ||
            (value.length > 0 && value.every((piece) => piece.descendantPid === true)))
        );
      });
      target.vars.set(name, [
        unknown(`\`$${name}\`, which a branch sets differently`, signalSafe ? true : undefined),
      ]);
    }
  }
  const arrayNames = new Set<string>();
  for (const branch of branches) for (const name of branch.arrays.keys()) arrayNames.add(name);
  target.arrays.clear();
  for (const name of arrayNames) {
    const values = branches.map((branch) => JSON.stringify([...(branch.arrays.get(name) ?? [])]));
    const first = branches[0]?.arrays.get(name);
    if (first !== undefined && values.every((value) => value === values[0])) {
      target.arrays.set(name, new Map(first));
    } else {
      const unknownElement = [unknown(`\`$${name}\`, which a branch sets differently`)];
      target.arrays.set(name, new Map([[UNKNOWN_ARRAY_INDEX, unknownElement]]));
    }
  }
  const positionals = branches.map((branch) => JSON.stringify(branch.positional ?? null));
  target.positional = positionals.every((value) => value === positionals[0])
    ? branches[0]?.positional
    : undefined;
  const cwds = new Set(branches.map((branch) => branch.cwd));
  if (cwds.size > 1) {
    target.cwd = undefined;
    target.cwdWhy = "a `cd` inside a branch or loop";
  } else {
    target.cwd = branches[0]?.cwd;
    target.cwdWhy = branches[0]?.cwdWhy ?? target.cwdWhy;
  }
  for (const branch of branches) {
    for (const name of branch.exported) target.exported.add(name);
    target.backgroundStarted ||= branch.backgroundStarted;
  }
  const functionNames = new Set<string>();
  for (const branch of branches)
    for (const name of branch.functions.keys()) functionNames.add(name);
  const functions = new Map<string, Callee>();
  for (const name of functionNames) {
    const callees = branches.map((branch) => branch.functions.get(name));
    const first = callees[0];
    if (first !== undefined && callees.every((callee) => callee === first)) {
      functions.set(name, first);
      continue;
    }
    const candidates = new Set<FunctionDefinition | undefined>();
    for (const callee of callees)
      for (const definition of callee ?? [undefined]) candidates.add(definition);
    functions.set(name, [...candidates]);
  }
  target.functions = functions;
  // A handler a branch or loop body sets stays set after it, as bash keeps it. It carries the
  // variables that body left, since the merged state may already have blurred the ones it reads
  // (`t=$(mktemp); trap 'rm -f "$t"' EXIT` in an `if`), beside the values this merge wrote for
  // them. A removal inside a branch that may not run resets nothing here.
  const lifted: Trap[] = [];
  let merged: ReadonlyMap<string, Expansion> | undefined;
  for (const branch of branches) {
    for (const trap of branch.traps) {
      if (target.traps.includes(trap)) continue;
      merged ??= new Map(target.vars);
      lifted.push({ ...trap, vars: handlerVars(trap, branch.vars), merged });
    }
  }
  target.traps = [...target.traps, ...lifted];
}

/** The variables a handler runs with in a shell whose variables are `vars`. For a handler lifted
 * out of a branch, a name the shell still holds as the merge that lifted it wrote it takes the
 * value that branch left, since the handler runs only on a path where that branch ran; a name
 * assigned since holds its new value, which is the one the handler reads when it runs. This capture
 * exists because a variable keeps one value after a merge, a blur where the branches differ. Giving
 * variables every candidate a path may have left, as a function name has (`Callee`), would replace
 * it, and is the direction a further change to the merge model takes. */
function handlerVars(
  trap: Trap,
  vars: ReadonlyMap<string, Expansion>
): ReadonlyMap<string, Expansion> {
  const { vars: branch, merged } = trap;
  if (branch === undefined || merged === undefined) return vars;
  const out = new Map(vars);
  for (const [name, value] of branch) if (vars.get(name) === merged.get(name)) out.set(name, value);
  return out;
}

function lineOf(source: string, pos: number): number {
  let line = 1;
  for (let i = 0; i < pos && i < source.length; i += 1) if (source.charAt(i) === "\n") line += 1;
  return line;
}

/** The snippet now; the line only when a refusal reads it, since counting lines for every
 * command of a long script would make walking it quadratic. */
function siteOf(node: { readonly pos: number; readonly end: number }, st: State): Site {
  const source = st.source;
  return {
    snippet: source.slice(node.pos, node.end).trim(),
    get line() {
      return lineOf(source, node.pos);
    },
  };
}

function walkScript(script: ParsedScript, st: State, ctx: Ctx, runTraps = true): void {
  const [error] = script.errors ?? [];
  if (error !== undefined) {
    throw new Refusal(
      st.source.trim(),
      lineOf(st.source, error.pos),
      `the guard cannot parse this command confidently (${error.message} at offset ${error.pos}), ` +
        "and it never guesses what a malformed command runs. Rewrite it in plain bash, or put the " +
        "work in a script file"
    );
  }
  for (const statement of script.commands) walkNode(statement, st, ctx, false);
  if (runTraps) runSetTraps(st, ctx);
}

/** A subshell of this shell: `( … )`, each part of a pipeline, a coprocess, a command or process
 * substitution. Bash resets the parent's traps in one, so a subshell runs only the handlers it sets
 * itself, at its own end (`runSetTraps`), and the parent's run once, at the parent's end. */
function subshell(st: State, patch?: Partial<State>): State {
  return { ...clone(st), traps: [], ...patch };
}

/** Runs the body of every handler this shell set, at its end: each against the state after the
 * shell's last statement, with the variables of the branch that set it where the shell still holds
 * what the merge wrote for them (`handlerVars`), and the pids it expanded (`Trap.pids`) over it. */
function runSetTraps(st: State, ctx: Ctx): void {
  for (const trap of st.traps) {
    const vars = new Map([...handlerVars(trap, st.vars), ...(trap.pids ?? [])]);
    try {
      runText(
        trap.text,
        "the EXIT trap",
        subshell(st, { source: trap.file, vars }),
        ctx,
        trap.site
      );
    } catch (error) {
      locateRefusal(error, trap.file);
    }
  }
}

function walkList(statements: readonly Statement[], st: State, ctx: Ctx): void {
  for (const statement of statements) walkNode(statement, st, ctx, false);
}

/** The passes of a loop the guard can decide (an argument loop runs one per option) that it walks
 * one at a time before it walks the rest as a loop that may run any number of times. */
const MAX_DECIDED_PASSES = 64;

/** A `while` or `until` loop that may run any number of times: its condition and body walked once
 * on a copy, merged with not running at all. */
function walkLoopAnyPasses(node: While, st: State, ctx: Ctx): void {
  const before = st.output;
  const body = clone(st);
  walkList(node.clause.commands, body, ctx);
  walkList(node.body.commands, body, ctx);
  merge(st, [clone(st), body]);
  if (outputChanged(before, body.output)) st.output = undefined;
}

/** A `while` or `until` loop: walked pass by pass as bash runs it while the guard can decide its
 * condition (an argument loop over known arguments, `while [ $# -gt 0 ]; do case "$1" in ...`),
 * and from the first condition it cannot decide on as a loop that may run any number of times. A
 * body holding `break` or `continue` is never decided, since the passes would not end where the
 * condition says. */
function walkLoop(node: While, st: State, ctx: Ctx): void {
  if (!/\b(break|continue)\b/.test(st.source.slice(node.body.pos, node.body.end))) {
    for (let pass = 0; pass < MAX_DECIDED_PASSES; pass += 1) {
      const holds = decideCondition(node.clause, st, ctx);
      if (holds === undefined) break;
      walkList(node.clause.commands, st, ctx);
      if (holds === (node.kind === "until")) return;
      walkList(node.body.commands, st, ctx);
    }
  }
  walkLoopAnyPasses(node, st, ctx);
}

/** A condition's truth when it is one `[` or `test` command over words the guard knows, else
 * undefined. A word that expands to nothing is undecided, since bash would drop it. */
function decideCondition(clause: CompoundList, st: State, ctx: Ctx): boolean | undefined {
  const [statement, ...others] = clause.commands;
  if (statement === undefined || others.length > 0 || statement.background) return undefined;
  const command = statement.command;
  if (
    statement.redirects.length > 0 ||
    command.type !== "Command" ||
    command.name === undefined ||
    command.prefix.length > 0 ||
    command.redirects.length > 0
  ) {
    return undefined;
  }
  // The name as written: `[` would expand as the start of a bracket glob.
  const name = command.name.text;
  if (name !== "[" && name !== "test") return undefined;
  const argv = args(command.suffix, st, ctx);
  if (argv.length !== command.suffix.length) return undefined;
  const words = argv.map((arg) => literalText(arg.exp));
  if (words.some((word) => word === undefined || word === "")) return undefined;
  const operands = words as string[];
  if (name === "[")
    return operands.at(-1) === "]" ? evaluateTest(operands.slice(0, -1)) : undefined;
  return evaluateTest(operands);
}

/** The one item a `case` runs when the guard knows its word: the first whose pattern matches.
 * Undefined when it cannot tell, and then every branch is walked: a word or a pattern before the
 * match it cannot know, a bracket expression or an extended pattern, an item that falls through,
 * and no item matching at all, so a match the guard missed never leaves every branch unwalked. */
function decideCase(node: Case, st: State, ctx: Ctx): CaseItem | undefined {
  const words = expandWord(node.word, st, ctx);
  const value = words.length === 1 ? literalText(words[0]) : undefined;
  if (value === undefined) return undefined;
  for (const item of node.items) {
    for (const word of item.pattern) {
      const patterns = expandWord(word, st, ctx);
      const matched =
        patterns.length === 1 ? matchesPattern(patterns[0] as Expansion, value) : undefined;
      if (matched === undefined) return undefined;
      if (matched)
        return item.terminator === undefined || item.terminator === ";;" ? item : undefined;
    }
  }
  return undefined;
}

/** Whether a `case` pattern matches `value` (`patternSource`); undefined when the guard cannot
 * match it exactly, a value outside ASCII included. */
function matchesPattern(pattern: Expansion, value: string): boolean | undefined {
  const source = patternSource(pattern);
  if (source === undefined || !isAscii(value)) return undefined;
  return new RegExp(`^${source}$`).test(value);
}

/** A bash pattern as a regular expression's source: literal ASCII text exactly, unquoted `*` and
 * `?` as wildcards. Undefined for what the guard cannot match exactly: an unknown part, a bracket
 * expression, text that may be an extended pattern (`@(a|b)`), and text outside ASCII. */
function patternSource(pattern: Expansion): string | undefined {
  let source = "";
  for (const piece of pattern) {
    if (piece.kind === "unknown" || !isAscii(piece.text)) return undefined;
    if (piece.kind === "glob") {
      if (piece.text === "*") source += "[\\s\\S]*";
      else if (piece.text === "?") source += "[\\s\\S]";
      else return undefined;
    } else if (/[()|]/.test(piece.text)) return undefined;
    else source += piece.text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }
  return source;
}

function outputChanged(
  before: readonly Expansion[] | undefined,
  after: readonly Expansion[] | undefined
): boolean {
  return (
    before !== undefined &&
    (after === undefined || JSON.stringify(before) !== JSON.stringify(after))
  );
}

function walkNode(node: Node, st: State, ctx: Ctx, pipeIn: boolean): void {
  if (++ctx.steps.count > MAX_WALK_STEPS) {
    const site = siteOf(node, st);
    throw new Refusal(
      site.snippet,
      site.line,
      `the guard reached its walk limit of ${MAX_WALK_STEPS} nodes; run a smaller script`
    );
  }
  switch (node.type) {
    case "Statement": {
      checkRedirects(node.redirects, undefined, siteOf(node, st), st, ctx);
      if (node.background) {
        // `command &` runs in a subshell: nothing it changes (a handler, a variable, the working
        // directory, the arguments) reaches this shell, and the handlers it sets run at its end.
        const before = st.output;
        const child = subshell(st);
        walkNode(node.command, child, ctx, pipeIn);
        runSetTraps(child, ctx);
        if (outputChanged(before, child.output)) st.output = undefined;
        st.backgroundStarted = true;
        return;
      }
      if (node.command.type === "BraceGroup") {
        const written = groupOutputFile(node, st, ctx);
        if (written !== undefined) {
          st.files.set(written, walkRenderingGroup(node.command, st, ctx));
          return;
        }
      }
      walkNode(node.command, st, ctx, pipeIn);
      return;
    }
    case "Command":
      handleCommand(node, st, ctx, pipeIn);
      return;
    case "Pipeline": {
      // `! command`, a pipeline of one, runs in this shell; bash runs every part of a longer
      // pipeline in a subshell of its own.
      if (node.commands.length === 1) {
        for (const command of node.commands) walkNode(command, st, ctx, pipeIn);
        return;
      }
      const before = st.output;
      const parts: State[] = [];
      for (const [index, command] of node.commands.entries()) {
        const part = subshell(st);
        walkNode(command, part, ctx, index > 0);
        runSetTraps(part, ctx);
        parts.push(part);
      }
      if (parts.some((part) => outputChanged(before, part.output))) st.output = undefined;
      return;
    }
    case "AndOr":
      for (const command of node.commands) walkNode(command, st, ctx, pipeIn);
      return;
    case "CompoundList":
      walkList(node.commands, st, ctx);
      return;
    case "BraceGroup":
      walkList(node.body.commands, st, ctx);
      return;
    case "Subshell": {
      const before = st.output;
      const child = subshell(st);
      walkList(node.body.commands, child, ctx);
      runSetTraps(child, ctx);
      if (outputChanged(before, child.output)) st.output = undefined;
      return;
    }
    case "If": {
      const before = st.output;
      walkList(node.clause.commands, st, ctx);
      const clauseOutput = st.output;
      const then = clone(st);
      walkList(node.then.commands, then, ctx);
      const otherwise = clone(st);
      if (node.else !== undefined) walkNode(node.else, otherwise, ctx, pipeIn);
      merge(st, [then, otherwise]);
      if (
        outputChanged(before, clauseOutput) ||
        outputChanged(clauseOutput, then.output) ||
        outputChanged(clauseOutput, otherwise.output)
      ) {
        st.output = undefined;
      }
      return;
    }
    case "For":
    case "Select": {
      const before = st.output;
      const words = args(node.wordlist, st, ctx);
      for (const word of node.wordlist) visitWord(word, st, ctx);
      const name = node.name.value;
      const known = words.map((word) => literalText(word.exp));
      const branches: State[] = [clone(st)];
      if (node.type === "For" && known.every((w) => w !== undefined) && known.length <= 16) {
        for (const value of known) {
          const body = clone(st);
          body.vars.set(name, [literal(value as string)]);
          walkList(node.body.commands, body, ctx);
          branches.push(body);
        }
      } else {
        const body = clone(st);
        const signalSafe = words.every((word) => {
          const text = literalText(word.exp);
          return (
            text === "" || (word.exp.length > 0 && word.exp.every((piece) => piece.descendantPid))
          );
        });
        body.vars.set(
          name,
          signalSafe
            ? [unknown(`\`$${name}\`, a loop variable`, true)]
            : [unknown(`\`$${name}\`, a loop variable`)]
        );
        walkList(node.body.commands, body, ctx);
        branches.push(body);
      }
      merge(st, branches);
      if (branches.some((branch) => outputChanged(before, branch.output))) st.output = undefined;
      return;
    }
    case "ArithmeticFor": {
      const before = st.output;
      visitArithmetic(node.initialize, st, ctx);
      visitArithmetic(node.test, st, ctx);
      visitArithmetic(node.update, st, ctx);
      const body = clone(st);
      walkList(node.body.commands, body, ctx);
      merge(st, [clone(st), body]);
      if (outputChanged(before, body.output)) st.output = undefined;
      return;
    }
    case "While": {
      walkLoop(node, st, ctx);
      return;
    }
    case "Case": {
      const chosen = decideCase(node, st, ctx);
      if (chosen !== undefined) {
        visitWord(node.word, st, ctx);
        for (const item of node.items) for (const word of item.pattern) visitWord(word, st, ctx);
        walkList(chosen.body.commands, st, ctx);
        return;
      }
      const before = st.output;
      visitWord(node.word, st, ctx);
      const branches: State[] = [clone(st)];
      for (const item of node.items) {
        for (const word of item.pattern) visitWord(word, st, ctx);
        const body = clone(st);
        walkList(item.body.commands, body, ctx);
        branches.push(body);
      }
      merge(st, branches);
      if (branches.some((branch) => outputChanged(before, branch.output))) st.output = undefined;
      return;
    }
    case "Function": {
      let callee = definitions.get(node);
      if (callee === undefined) {
        callee = [{ body: node.body, source: st.source, file: st.script }];
        definitions.set(node, callee);
      }
      st.functions.set(node.name.value, callee);
      checkRedirects(node.redirects, undefined, siteOf(node, st), st, ctx);
      return;
    }
    case "Coproc": {
      const before = st.output;
      const child = subshell(st);
      walkNode(node.body, child, ctx, false);
      runSetTraps(child, ctx);
      if (outputChanged(before, child.output)) st.output = undefined;
      checkRedirects(node.redirects, undefined, siteOf(node, st), st, ctx);
      return;
    }
    case "TestCommand":
      visitTest(node.expression, st, ctx);
      return;
    case "ArithmeticCommand":
      visitArithmetic(node.expression, st, ctx);
      return;
  }
}

// --- Redirections ------------------------------------------------------------------------------

/** Checks each output redirection for an overwrite outside the roots, and records what a
 * command writes to a file it may run later in the same command (`cat > f <<EOF`). */
function checkRedirects(
  redirects: readonly Redirect[],
  command: { readonly name: string | undefined; readonly args: readonly Arg[] } | undefined,
  site: Site,
  st: State,
  ctx: Ctx
): void {
  const heredoc = redirects.find(
    (redirect) => redirect.operator === "<<" || redirect.operator === "<<-"
  );
  const herestring = redirects.find((redirect) => redirect.operator === "<<<");
  for (const redirect of redirects) {
    visitWord(redirect.target, st, ctx);
    visitWord(redirect.body, st, ctx);
    const operator = redirect.operator;
    const writes = operator === ">" || operator === ">|" || operator === "&>";
    const appends = operator === ">>" || operator === "&>>";
    const duplicate =
      operator === ">&" &&
      redirect.target !== undefined &&
      /^(?:[0-9]+|-)$/.test(redirect.target.value);
    if (
      !(writes || appends || (operator === ">&" && !duplicate)) ||
      redirect.target === undefined
    ) {
      continue;
    }
    for (const exp of expandWord(redirect.target, st, ctx)) {
      if (writes || operator === ">&") {
        const verdict = judgePath(exp, st, ctx, { follow: true, overwrite: true });
        if (!verdict.ok) {
          throw refusal(
            site,
            `the redirection \`${operator}\` would overwrite \`${redirect.target.text}\` (${verdict.resolution})`,
            ctx
          );
        }
      }
      const target = literalText(exp);
      if (target === undefined || (st.cwd === undefined && !target.startsWith("/"))) continue;
      const file = path.resolve(st.cwd ?? "/", target);
      let content: string | null = null;
      if (command?.name === "cat" && command.args.length === 0) {
        content = heredoc?.content ?? (herestring ? herestringText(herestring, st, ctx) : null);
      } else if (command?.name === "echo") {
        const words = command.args.map((arg) => literalText(arg.exp));
        content = words.every((word) => word !== undefined) ? `${words.join(" ")}\n` : null;
      } else if (command?.name === "printf") {
        content = printfText(command.args) ?? null;
      }
      st.pidFiles.delete(file);
      const before = appends ? st.files.get(file) : "";
      st.files.set(file, content === null || before === null ? null : `${before ?? ""}${content}`);
      if (
        writes &&
        !appends &&
        command?.name === "echo" &&
        command.args.length === 1 &&
        command.args[0]?.exp.length > 0 &&
        command.args[0]?.exp.every((piece) => piece.descendantPid)
      ) {
        st.pidFiles.set(file, command.args[0].exp);
      }
    }
  }
}

function herestringText(redirect: Redirect, st: State, ctx: Ctx): string | null {
  if (redirect.target === undefined) return null;
  return literalText(expandWord(redirect.target, st, ctx)[0]) ?? null;
}

/** The file a `{ ...; } > file` statement writes its output to, when that is its one redirection
 * and the guard knows the path. */
function groupOutputFile(statement: Statement, st: State, ctx: Ctx): string | undefined {
  const [redirect, ...others] = statement.redirects;
  if (redirect === undefined || others.length > 0 || redirect.fileDescriptor !== undefined) {
    return undefined;
  }
  if ((redirect.operator !== ">" && redirect.operator !== ">|") || redirect.target === undefined) {
    return undefined;
  }
  const targets = expandWord(redirect.target, st, ctx);
  const target = targets.length === 1 ? literalText(targets[0]) : undefined;
  if (target === undefined || (st.cwd === undefined && !target.startsWith("/"))) return undefined;
  return path.resolve(st.cwd ?? "/", target);
}

/** Walks a brace group whose output goes to a file, and returns what it writes there: the
 * concatenated output of each statement, with each part the guard cannot know as the unknown
 * marker, or null once a statement prints something it cannot render. Each statement's output is
 * read before the statement is walked, so it sees the variables the statements before it set. */
function walkRenderingGroup(group: BraceGroup, st: State, ctx: Ctx): string | null {
  let content: string | null = "";
  for (const statement of group.body.commands) {
    if (content !== null) {
      const text = statementOutput(statement, st, ctx);
      content = text === undefined ? null : content + text;
    }
    walkNode(statement, st, ctx, false);
  }
  return content;
}

/** What one statement of a rendered group prints: nothing for an assignment, a quoted here-document
 * fed to `cat`, `printf`, or `echo`. Undefined for anything else. */
function statementOutput(statement: Statement, st: State, ctx: Ctx): string | undefined {
  const command = statement.command;
  if (statement.background || statement.redirects.length > 0 || command.type !== "Command") {
    return undefined;
  }
  if (command.name === undefined) return command.redirects.length === 0 ? "" : undefined;
  const name = literalText(expandWord(command.name, st, ctx)[0]);
  const rest = args(command.suffix, st, ctx);
  if (name === "cat" && rest.length === 0) {
    const [heredoc, ...others] = command.redirects;
    if (heredoc === undefined || others.length > 0) return undefined;
    if (heredoc.operator !== "<<" && heredoc.operator !== "<<-") return undefined;
    // An unquoted here-document expands its text as bash writes it; only one with nothing to
    // expand reads the same.
    const text = heredoc.content;
    if (text === undefined || (!heredoc.heredocQuoted && /[$`\\]/.test(text))) return undefined;
    return text;
  }
  if (command.redirects.length > 0) return undefined;
  if (name === "printf") return printfText(rest);
  if (name === "echo") {
    if (literalText(rest[0]?.exp)?.startsWith("-")) return undefined;
    return `${rest.map((arg) => runtimeText(arg.exp)).join(" ")}\n`;
  }
  return undefined;
}

/** What `printf FORMAT ARG...` prints, with each part the guard cannot know as the unknown marker:
 * `%s`, `%q` (quoted for a shell to read back), `%d`, `%%`, and the escapes `\n`, `\t` and `\\`,
 * the format reused while arguments remain. Undefined for a format it does not render: an unknown
 * one, `-v`, a width or precision, or another conversion. */
function printfText(list: readonly Arg[]): string | undefined {
  const [formatArg, ...values] = list;
  const format = literalText(formatArg?.exp);
  if (format === undefined || format.startsWith("-")) return undefined;
  const parts: ({ readonly text: string } | { readonly conversion: "s" | "q" | "d" })[] = [];
  for (let i = 0; i < format.length; i += 1) {
    const char = format.charAt(i);
    const next = format.charAt(i + 1);
    if (char === "%") {
      if (next === "%") parts.push({ text: "%" });
      else if (next === "s" || next === "q" || next === "d") parts.push({ conversion: next });
      else return undefined;
      i += 1;
    } else if (char === "\\") {
      const text = next === "n" ? "\n" : next === "t" ? "\t" : next === "\\" ? "\\" : undefined;
      if (text === undefined) return undefined;
      parts.push({ text });
      i += 1;
    } else parts.push({ text: char });
  }
  const conversions = parts.filter((part) => "conversion" in part).length;
  let out = "";
  let index = 0;
  do {
    for (const part of parts) {
      if ("text" in part) {
        out += part.text;
        continue;
      }
      const value = values[index];
      index += 1;
      const text = value === undefined ? "" : runtimeText(value.exp);
      if (part.conversion === "s") out += text;
      else if (part.conversion === "d")
        out += /^-?[0-9]+$/.test(text) ? text : value === undefined ? "0" : UNKNOWN_MARKER;
      else out += shellQuoted(text);
    }
  } while (conversions > 0 && index < values.length);
  return out;
}

/** A path refusal: the rule, the pane's roots, and what to do instead. */
function refusal(site: Site, detail: string, ctx: Ctx): Refusal {
  return new Refusal(
    site.snippet,
    site.line,
    `${detail}, outside ${describeRoots(ctx.roots)}. ${HINT}`
  );
}

// --- Commands ----------------------------------------------------------------------------------

interface Invocation {
  readonly args: readonly Arg[];
  readonly site: Site;
  /** Assignments prefixed to the command: the environment of a program it starts. */
  readonly overlay: ReadonlyMap<string, Expansion>;
  readonly redirects: readonly Redirect[];
  readonly pipeIn: boolean;
}

function handleCommand(command: Command, st: State, ctx: Ctx, pipeIn: boolean): void {
  const site = siteOf(command, st);
  const overlay = new Map<string, Expansion>();
  for (const assignment of command.prefix) {
    visitWord(assignment.value, st, ctx);
    visitParts(assignment.indexParts, st, ctx);
    for (const word of assignment.array ?? []) visitWord(word, st, ctx);
    if (assignment.name === undefined) continue;
    const expanded =
      assignment.value === undefined
        ? [literal("")]
        : (expandWord(assignment.value, st, ctx)[0] ?? []);
    const arrayAssignment =
      assignment.array !== undefined || assignment.append || assignment.index !== undefined;
    const descendantPid = expanded.length > 0 && expanded.every((piece) => piece.descendantPid);
    const value = arrayAssignment
      ? [unknown(`\`$${assignment.name}\`, an array or appended value`, descendantPid || undefined)]
      : expanded;
    overlay.set(assignment.name, value);
    // `a=1 b=$a` alone assigns left to right in this shell.
    if (command.name === undefined) {
      if (arrayAssignment) {
        const elements =
          assignment.array !== undefined && !assignment.append
            ? new Map<string, Expansion>()
            : new Map(st.arrays.get(assignment.name) ?? []);
        if (assignment.array !== undefined) {
          let index = assignment.append
            ? Math.max(
                0,
                ...[...elements.keys()]
                  .filter((key) => /^[0-9]+$/.test(key))
                  .map((key) => Number(key) + 1)
              )
            : 0;
          for (const word of assignment.array) {
            elements.set(String(index), expandWord(word, st, ctx)[0] ?? []);
            index += 1;
          }
        } else if (assignment.append) {
          const index = Math.max(
            0,
            ...[...elements.keys()]
              .filter((key) => /^[0-9]+$/.test(key))
              .map((key) => Number(key) + 1)
          );
          elements.set(String(index), expanded);
        } else {
          const index = resolveArrayIndex(assignment.index, st, ctx);
          if (index === undefined) {
            elements.clear();
            elements.set(UNKNOWN_ARRAY_INDEX, [
              unknown(`\`$${assignment.name}\`, an array element with an unresolved index`),
            ]);
          } else {
            elements.set(index, expanded);
          }
        }
        st.arrays.set(assignment.name, elements);
      } else {
        st.arrays.delete(assignment.name);
      }
      st.vars.set(assignment.name, value);
    }
  }
  const words = command.name === undefined ? command.suffix : [command.name, ...command.suffix];
  for (const word of words) visitWord(word, st, ctx);
  if (command.name === undefined) {
    checkRedirects(command.redirects, undefined, site, st, ctx);
    return;
  }
  const argv = args(words, st, ctx);
  checkRedirects(
    command.redirects,
    { name: literalText(argv[0]?.exp), args: argv.slice(1) },
    site,
    st,
    ctx
  );
  dispatch({ args: argv, site, overlay, redirects: command.redirects, pipeIn }, st, ctx);
}

/** Options of a command: the operands after them, honouring `--` and the options that take a
 * value. An argument the guard cannot know is an operand. */
function operands(
  list: readonly Arg[],
  valued: string,
  longValued: readonly string[] = []
): { options: string[]; operands: Arg[] } {
  const options: string[] = [];
  const found: Arg[] = [];
  let rest = false;
  for (let i = 0; i < list.length; i += 1) {
    const arg = list[i] as Arg;
    const text = literalText(arg.exp);
    if (rest || text === undefined || !text.startsWith("-") || text === "-") {
      found.push(arg);
      continue;
    }
    if (text === "--") {
      rest = true;
      continue;
    }
    options.push(text);
    if (text.startsWith("--")) {
      if (!text.includes("=") && longValued.includes(text)) i += 1;
      continue;
    }
    const last = text.charAt(text.length - 1);
    const valueAt = [...text.slice(1)].findIndex((flag) => valued.includes(flag));
    if (valueAt !== -1 && valueAt === text.length - 2 && valued.includes(last)) i += 1;
  }
  return { options, operands: found };
}

/** Strips the programs that run another program unchanged: `sudo rm` is `rm`. */
function unwrap(
  argv: readonly Arg[],
  st: State,
  ctx: Ctx,
  site: Site
): { argv: readonly Arg[]; st: State } {
  let list = argv;
  let state = st;
  for (let guard = 0; guard < 16; guard += 1) {
    const name = literalText(list[0]?.exp);
    if (name === undefined) break;
    const base = path.basename(name);
    const rest = list.slice(1);
    const skip = (valued: string, longValued: readonly string[] = []): readonly Arg[] => {
      let i = 0;
      for (; i < rest.length; i += 1) {
        const text = literalText(rest[i]?.exp);
        if (text === "--") return rest.slice(i + 1);
        if (text === undefined || !text.startsWith("-") || text === "-") break;
        if (text.startsWith("--")) {
          if (!text.includes("=") && longValued.includes(text)) i += 1;
        } else {
          const flags = text.slice(1);
          const takesValue = [...flags].findIndex((flag) => valued.includes(flag));
          if (takesValue === flags.length - 1) i += 1;
        }
      }
      return rest.slice(i);
    };
    if ((base === "busybox" || base === "toybox") && rest[0] !== undefined) list = rest;
    if (base === "sudo" || base === "doas") list = skip("ughpCDrtUT", ["--user", "--group"]);
    else if (base === "nice") list = skip("n", ["--adjustment"]);
    else if (["nohup", "setsid", "builtin", "time"].includes(base)) list = skip("");
    else if (base === "exec") list = skip("a");
    else if (base === "command") {
      const flags = rest.map((arg) => literalText(arg.exp));
      if (flags[0] === "-v" || flags[0] === "-V") return { argv: [], st: state };
      list = skip("");
    } else if (base === "stdbuf") list = skip("ioe", ["--input", "--output", "--error"]);
    else if (base === "ionice") list = skip("cnt", ["--class", "--classdata"]);
    else if (base === "timeout") {
      const after = skip("sk", ["--signal", "--kill-after"]);
      list = after.slice(1);
    } else if (base === "env") {
      let i = 0;
      const overlay = new Map<string, Expansion>();
      const unset: string[] = [];
      let cwd = state.cwd;
      for (; i < rest.length; i += 1) {
        const arg = rest[i] as Arg;
        const text = literalText(arg.exp);
        if (text === "-u" || text === "--unset") {
          const name = literalText(rest[i + 1]?.exp);
          if (name !== undefined) unset.push(name);
          i += 1;
        } else if (text === "-C" || text === "--chdir") {
          const dir = literalText(rest[i + 1]?.exp);
          cwd =
            dir === undefined || state.cwd === undefined ? undefined : path.resolve(state.cwd, dir);
          i += 1;
        } else if (text?.startsWith("--chdir=")) {
          cwd = state.cwd === undefined ? undefined : path.resolve(state.cwd, text.slice(8));
        } else if (text === "-S" || text === "--split-string") {
          const split = literalText(rest[i + 1]?.exp);
          const tail = rest.slice(i + 2);
          if (split === undefined) {
            throw new Refusal(
              site.snippet,
              site.line,
              "the guard cannot read the text `env -S` splits"
            );
          }
          const reparsed = args(
            parse(split).commands.flatMap((s) =>
              s.command.type === "Command" && s.command.name !== undefined
                ? [s.command.name, ...s.command.suffix]
                : []
            ),
            state,
            ctx
          );
          list = [...reparsed, ...tail];
          i = -1;
          break;
        } else if (text?.startsWith("-")) continue;
        else if (text !== undefined && /^[A-Za-z_][A-Za-z0-9_]*=/.test(text)) {
          overlay.set(text.slice(0, text.indexOf("=")), [
            literal(text.slice(text.indexOf("=") + 1)),
          ]);
        } else if (
          arg.exp[0]?.kind === "literal" &&
          /^[A-Za-z_][A-Za-z0-9_]*=/.test(arg.exp[0].text)
        ) {
          overlay.set(arg.exp[0].text.slice(0, arg.exp[0].text.indexOf("=")), [
            unknown("an `env` assignment"),
          ]);
        } else break;
      }
      if (i !== -1) list = rest.slice(i);
      state = { ...clone(state), cwd, cwdWhy: "`env -C`" };
      for (const name of unset) {
        state.vars.set(name, [literal("")]);
        state.exported.delete(name);
      }
      for (const [name, value] of overlay) {
        state.vars.set(name, value);
        state.exported.add(name);
      }
    } else break;
  }
  return { argv: list, st: state };
}

function runFunction(
  name: string,
  definition: FunctionDefinition,
  args: readonly Arg[],
  outer: State,
  ctx: Ctx
): void {
  if (outer.runningFunctions.has(name)) return;
  const child: State = {
    ...clone(outer),
    positional: positionalOf(args),
    source: definition.source,
    script: definition.file ?? outer.script,
  };
  child.runningFunctions.add(name);
  try {
    walkNode(definition.body, child, ctx, false);
  } catch (error) {
    locateRefusal(error, definition.file);
  }
  // A call runs in this shell, so everything the body leaves stays: the copy-back rule on `merge`
  // holds here too. Not carried: the positional parameters, which bash restores to the caller's
  // when a function returns; `files` and `pidFiles` (shared); and `runningFunctions`.
  outer.vars = child.vars;
  outer.exported = child.exported;
  outer.arrays.clear();
  for (const [name, elements] of child.arrays) outer.arrays.set(name, new Map(elements));
  outer.cwd = child.cwd;
  outer.cwdWhy = child.cwdWhy;
  outer.functions = child.functions;
  outer.traps = child.traps;
  outer.output = child.output;
  outer.backgroundStarted ||= child.backgroundStarted;
}

function functionOutput(
  name: string,
  definition: FunctionDefinition,
  args: readonly Arg[],
  outer: State,
  ctx: Ctx
): readonly Expansion[] | undefined {
  if (outer.runningFunctions.has(name)) return undefined;
  const child: State = {
    ...clone(outer),
    positional: positionalOf(args),
    source: definition.source,
    script: definition.file ?? outer.script,
    output: [],
  };
  child.runningFunctions.add(name);
  try {
    walkNode(definition.body, child, ctx, false);
  } catch (error) {
    locateRefusal(error, definition.file);
  }
  return child.output?.length === 0 ? undefined : child.output;
}

function dispatch(invocation: Invocation, outer: State, ctx: Ctx): void {
  const unwrapped = unwrap(invocation.args, outer, ctx, invocation.site);
  const argv = unwrapped.argv;
  const st = unwrapped.st;
  const name = literalText(argv[0]?.exp);
  // A command whose name is itself a variable or a substitution runs a program the guard cannot
  // name (the documented residual risk).
  if (name === undefined) return;
  const base = path.basename(name);
  const rest = argv.slice(1);
  const site = invocation.site;
  const callee = outer.functions.get(base);
  if (callee !== undefined) {
    const [only] = callee;
    if (callee.length === 1 && only !== undefined) {
      runFunction(base, only, rest, outer, ctx);
      return;
    }
    // Each definition a branch may have left, and the command where a path left none, runs as a
    // branch of its own.
    const runs = callee.map((definition) => {
      const run = clone(outer);
      if (definition !== undefined) runFunction(base, definition, rest, run, ctx);
      else {
        run.functions.delete(base);
        dispatch(invocation, run, ctx);
      }
      return run;
    });
    merge(outer, runs);
    return;
  }
  const stdoutRedirected = invocation.redirects.some(
    (redirect) =>
      (redirect.fileDescriptor === undefined || redirect.fileDescriptor === 1) &&
      [">", ">>", ">|", "&>", "&>>", ">&"].includes(redirect.operator)
  );
  // `umask` prints the mask (`-S`, `-p`) only when it is given no mode to set.
  const umaskWords = base === "umask" ? rest.map((arg) => literalText(arg.exp)) : [];
  const silentUmask =
    umaskWords.every((word) => word !== undefined) &&
    umaskWords.some((word) => !word?.startsWith("-"));
  if (
    outer.output !== undefined &&
    !stdoutRedirected &&
    base !== "echo" &&
    base !== "printf" &&
    !silentUmask &&
    FUNCTION_OUTPUT_SILENT[base] !== true
  ) {
    outer.output = undefined;
  }
  switch (base) {
    case "echo":
      if (outer.output !== undefined && !stdoutRedirected) {
        outer.output =
          rest.length === 1 && rest[0] !== undefined ? [...outer.output, rest[0].exp] : undefined;
      }
      return;
    case "cd":
    case "pushd": {
      const target = operands(rest, "").operands[0];
      changeDirectory(target, outer, ctx);
      return;
    }
    case "popd":
      outer.cwd = undefined;
      outer.cwdWhy = "`popd`";
      return;
    case "export":
    case "declare":
    case "typeset":
    case "local":
    case "readonly":
      declaration(base, rest, outer, ctx);
      return;
    case "unset":
      for (const arg of operands(rest, "").operands) {
        const variable = literalText(arg.exp);
        if (variable !== undefined) {
          outer.vars.set(variable, [literal("")]);
          outer.arrays.delete(variable);
        }
      }
      return;
    case "read":
    case "mapfile":
    case "readarray":
    case "getopts": {
      const found = operands(rest, base === "read" || base === "getopts" ? "adnNptui" : "");
      const names = base === "getopts" ? found.operands.slice(1, 2) : found.operands;
      const arrayAt = base === "read" ? rest.findIndex((arg) => literalText(arg.exp) === "-a") : -1;
      const readsArray = base === "mapfile" || base === "readarray" || arrayAt !== -1;
      const destinations =
        arrayAt === -1 || rest[arrayAt + 1] === undefined ? names : [rest[arrayAt + 1] as Arg];
      for (const arg of destinations) {
        const variable = literalText(arg.exp);
        if (variable !== undefined) {
          outer.vars.set(variable, [unknown(`\`$${variable}\`, read from input`)]);
          if (readsArray) {
            const unknownArray = [unknown(`\`$${variable}\`, read from input`)];
            outer.arrays.set(variable, new Map([[UNKNOWN_ARRAY_INDEX, unknownArray]]));
          }
        }
      }
      return;
    }
    case "set": {
      const list = rest.map((arg) => literalText(arg.exp));
      const dashDash = list.indexOf("--");
      if (dashDash !== -1) outer.positional = positionalOf(rest.slice(dashDash + 1));
      else if (list.length > 0 && !list[0]?.startsWith("-") && !list[0]?.startsWith("+")) {
        outer.positional = positionalOf(rest);
      }
      return;
    }
    case "shift": {
      // Bash shifts nothing when the count is negative, not a number, or exceeds `$#`; a count the
      // guard cannot read leaves the arguments unknown.
      const text = rest.length === 0 ? "1" : literalText(rest[0]?.exp);
      if (text === undefined) outer.positional = undefined;
      else if (/^[0-9]+$/.test(text) && outer.positional !== undefined) {
        const count = Number(text);
        if (count <= outer.positional.length) outer.positional = outer.positional.slice(count);
      }
      return;
    }
    case "printf": {
      // `-v NAME` or `-vNAME`, printf's one option, comes first.
      const first = literalText(rest[0]?.exp);
      const joined = first?.startsWith("-v") === true && first.length > 2;
      const variable =
        first === "-v" ? literalText(rest[1]?.exp) : joined ? first.slice(2) : undefined;
      if (variable !== undefined) {
        // `printf -v NAME '%s' VALUE` assigns VALUE as it is, a pid of this shell's included;
        // another format assigns what printf renders, known only when every part of it is.
        const printed = rest.slice(joined ? 1 : 2);
        const [format, value, ...others] = printed;
        if (literalText(format?.exp) === "%s" && value !== undefined && others.length === 0) {
          outer.vars.set(variable, value.exp);
        } else {
          const text = printfText(printed);
          outer.vars.set(
            variable,
            text === undefined || text.includes(UNKNOWN_MARKER)
              ? [unknown(`\`$${variable}\` (printf -v)`)]
              : [literal(text)]
          );
        }
        return;
      }
      if (outer.output !== undefined && !stdoutRedirected) {
        outer.output =
          literalText(rest[0]?.exp) === "%s\\n" && rest.length === 2 && rest[1] !== undefined
            ? [...outer.output, (rest[1] as Arg).exp]
            : undefined;
      }
      return;
    }
    case "eval": {
      runText(
        rest.map((arg) => runtimeText(arg.exp)).join(" "),
        "the `eval` text",
        outer,
        ctx,
        site
      );
      return;
    }
    case "trap": {
      const [handler, ...conditions] = operands(rest, "").operands;
      if (handler === undefined) return;
      const text = literalText(handler.exp);
      // A condition the guard cannot read could be any condition. A removal resets only the
      // conditions it can read, so `trap - "$x"` resets nothing; a handler set for one records
      // UNREADABLE_CONDITION, which no removal names, so it stays and is walked.
      const readable = conditions.map((arg) => literalText(arg.exp));
      const signals = readable.map((condition) =>
        condition === undefined ? UNREADABLE_CONDITION : trapSignal(condition)
      );
      const handled = signals.length === 0 ? ["EXIT"] : signals;
      const file = outer.script ?? outer.source;
      if (text === "-") {
        // `trap - INT` resets INT and leaves every other handler, the EXIT one included; `trap -`
        // naming nothing is a usage error that resets nothing.
        const named = readable.flatMap((condition) =>
          condition === undefined ? [] : [trapSignal(condition)]
        );
        outer.traps = outer.traps.flatMap((trap) => {
          const signals = trap.signals.filter((signal) => !named.includes(signal));
          return signals.length === 0 ? [] : [{ ...trap, signals }];
        });
      } else if (
        text === undefined &&
        handler.exp.every((piece) => piece.kind === "literal" || piece.descendantPid)
      ) {
        // A double-quoted handler that expanded only this shell's children's pids when it was set
        // (`trap "kill $pid" EXIT`): its text reads each pid back from a name bound to it.
        const pids = new Map<string, Expansion>();
        const handlerText = handler.exp
          .map((piece) => {
            if (piece.kind === "literal") return piece.text;
            const name = `__legion_guard_trap_pid_${pids.size}`;
            pids.set(name, [piece]);
            return `\${${name}}`;
          })
          .join("");
        outer.traps = [...outer.traps, { text: handlerText, site, signals: handled, file, pids }];
      } else if (text === undefined) {
        throw new Refusal(
          site.snippet,
          site.line,
          `the guard cannot read the handler this \`trap\` sets, \`${handler.text}\` (${handler.exp.find((piece) => piece.kind !== "literal")?.why ?? "a value the guard cannot know"}), so it cannot check what runs when this shell exits; write the handler out, or put it in a function`
        );
      } else if (!text.startsWith("-")) {
        outer.traps = [...outer.traps, { text, site, signals: handled, file }];
      }
      return;
    }
    case "source":
    case ".": {
      const file = rest[0];
      if (file !== undefined) {
        runFile(file, rest.slice(1), outer, ctx, site, "shell", true);
      }
      return;
    }
    case "xargs":
      checkXargs(rest, st, ctx, site);
      return;
    case "rm":
    case "unlink":
      checkTargets(base, "delete", operands(rest, "").operands, false, st, ctx, site);
      return;
    case "mv": {
      const found = operands(rest, "tS", ["--target-directory", "--suffix"]);
      const targets = [...found.operands];
      const at = rest.findIndex((arg) => {
        const text = literalText(arg.exp);
        return text === "-t" || text === "--target-directory";
      });
      if (at !== -1 && rest[at + 1] !== undefined) targets.push(rest[at + 1] as Arg);
      for (const arg of rest) {
        const text = literalText(arg.exp);
        if (text?.startsWith("--target-directory=")) {
          targets.push({ text: arg.text, exp: [literal(text.slice(19))] });
        }
      }
      checkTargets("mv", "move", targets, false, st, ctx, site);
      return;
    }

    case "rsync": {
      const destination = operands(rest, "efTBM", [
        "--backup-dir",
        "--block-size",
        "--bwlimit",
        "--chmod",
        "--chown",
        "--exclude",
        "--exclude-from",
        "--files-from",
        "--filter",
        "--include",
        "--include-from",
        "--log-file",
        "--out-format",
        "--password-file",
        "--remote-option",
        "--rsync-path",
        "--temp-dir",
      ]).operands.at(-1);
      if (destination !== undefined) {
        checkTargets("rsync", "synchronize and delete into", [destination], true, st, ctx, site);
      }
      return;
    }
    case "tar":
    case "unzip": {
      const extracting =
        base === "unzip" ||
        rest.some((arg, index) => {
          const text = literalText(arg.exp);
          return (
            text === "--extract" ||
            text === "--get" ||
            (text?.startsWith("-") === true && !text.startsWith("--") && text.includes("x")) ||
            (index === 0 && /^[A-Za-z]*x[A-Za-z]*$/.test(text ?? ""))
          );
        });
      if (!extracting) return;
      const targetDirectories: Arg[] = [];
      const short = base === "tar" ? "-C" : "-d";
      const long = base === "tar" ? "--directory" : "--destination";
      for (const [index, arg] of rest.entries()) {
        const text = literalText(arg.exp);
        if (text === short || text === long) {
          const target = rest[index + 1];
          if (target !== undefined) targetDirectories.push(target);
        } else if (text?.startsWith(`${long}=`)) {
          targetDirectories.push({ text: arg.text, exp: [literal(text.slice(long.length + 1))] });
        }
      }
      checkTargets(base, "extract and overwrite in", targetDirectories, true, st, ctx, site);
      return;
    }
    case "shred":
      checkTargets(
        "shred",
        "shred",
        operands(rest, "ns", ["--iterations", "--size", "--random-source"]).operands,
        false,
        st,
        ctx,
        site
      );
      return;
    case "truncate":
      checkTargets(
        "truncate",
        "truncate",
        operands(rest, "sr", ["--size", "--reference"]).operands,
        true,
        st,
        ctx,
        site
      );
      return;
    case "chmod":
    case "chown":
    case "chgrp":
      checkRecursiveMode(base, rest, st, ctx, site);
      return;
    case "find":
      checkFind(rest, st, ctx, site);
      return;
    case "tee": {
      const found = operands(rest, "", ["--output-error"]);
      const append = found.options.some(
        (option) => option === "--append" || /^-[^-]*a/.test(option)
      );
      const heredoc = invocation.redirects.find((r) => r.operator === "<<" || r.operator === "<<-");
      for (const arg of found.operands) {
        if (!append) {
          const verdict = judgePath(arg.exp, st, ctx, { follow: true, overwrite: true });
          if (!verdict.ok) {
            throw refusal(site, `tee would overwrite \`${arg.text}\` (${verdict.resolution})`, ctx);
          }
        }
        const target = literalText(arg.exp);
        if (target !== undefined && st.cwd !== undefined) {
          const file = path.resolve(st.cwd, target);
          st.pidFiles.delete(file);
          st.files.set(file, append ? null : (heredoc?.content ?? null));
        }
      }
      return;
    }
    case "kill":
      checkKill(rest, ctx, site);
      return;
    case "pkill":
    case "killall":
    case "killall5":
      throw new Refusal(
        site.snippet,
        site.line,
        `${base} selects processes by name or pattern, which reaches other agents' processes on a ` +
          "shared machine; a pane signals only processes it started. Stop a process you started " +
          'with the hub tool (`op: "stop"`), or `kill` the pid of a child you started from this pane'
      );
    case "fuser":
      if (rest.some((arg) => /^-[a-zA-Z]*k/.test(literalText(arg.exp) ?? ""))) {
        throw new Refusal(
          site.snippet,
          site.line,
          "`fuser -k` kills every process using a file, other agents' included; a pane signals only " +
            "processes it started"
        );
      }
      return;
    case "tmux": {
      let socket: Arg | undefined;
      let socketIsPath = false;
      for (let i = 0; i < rest.length; i += 1) {
        const text = literalText(rest[i]?.exp);
        if (text === "-L" || text === "-S" || text === "--socket") {
          socket = rest[i + 1];
          socketIsPath = text !== "-L";
          i += 1;
        } else if (text?.startsWith("--socket=")) {
          socket = { text, exp: [literal(text.slice(9))] };
          socketIsPath = true;
        }
      }
      const killCommands = ["kill-server", "kill-session", "kill-window", "kill-pane"];
      const respawnCommands = ["respawn-pane", "respawn-window"];
      const respawning =
        rest.some((arg) => {
          const text = literalText(arg.exp);
          if (text === undefined) return false;
          const compact = text.replace("-", "");
          return (
            respawnCommands.filter((command) => command.replace("-", "").startsWith(compact))
              .length === 1
          );
        }) && rest.some((arg) => literalText(arg.exp) === "-k");
      const killing = rest.find((arg) => {
        const text = literalText(arg.exp);
        if (text === undefined) return false;
        const compact = text.replace("-", "");
        return (
          killCommands.filter((command) => command.replace("-", "").startsWith(compact)).length ===
            1 || respawning
        );
      });
      if (killing === undefined) return;
      let resolvedSocket: Expansion | undefined;
      if (socketIsPath) {
        resolvedSocket = socket?.exp;
      } else {
        const tmux = invocation.overlay.get("TMUX") ?? lookup("TMUX", st, ctx);
        const tmuxText = tmux === undefined ? undefined : literalText(tmux);
        if (socket === undefined && tmuxText !== undefined && tmuxText !== "") {
          resolvedSocket = [literal(tmuxText.split(",")[0] ?? "")];
        } else if (socket !== undefined || tmux === undefined || tmuxText === "") {
          const tmpdir = invocation.overlay.get("TMUX_TMPDIR") ?? lookup("TMUX_TMPDIR", st, ctx);
          const root = tmpdir ?? [literal("/tmp")];
          const name = socket === undefined ? [literal("default")] : socket.exp;
          const uid = process.getuid?.();
          if (uid !== undefined) {
            resolvedSocket = [...root, literal(`/tmux-${uid}/`), ...name];
          }
        }
      }
      if (
        resolvedSocket !== undefined &&
        judgePath(resolvedSocket, st, ctx, { follow: true, overwrite: false }).ok
      ) {
        return;
      }
      throw new Refusal(
        site.snippet,
        site.line,
        `\`tmux ${literalText(killing.exp)}\` needs a socket path inside this pane's roots; the ` +
          "resolved socket can end panes and processes this pane did not start"
      );
    }
    default:
      break;
  }
  if (SHELLS.has(base) || (base === "busybox" && SHELLS.has(literalText(rest[0]?.exp) ?? ""))) {
    runShell(base === "busybox" ? rest.slice(1) : rest, invocation, st, ctx);
    return;
  }
  const interpreter = interpreterLanguage(base);
  if (interpreter !== undefined) {
    runInterpreter(base, interpreter, rest, invocation, st, ctx);
    return;
  }
  if (name.includes("/")) {
    runFile(argv[0] as Arg, rest, childState(st, invocation.overlay), ctx, site, "auto");
  }
}

function changeDirectory(target: Arg | undefined, st: State, ctx: Ctx): void {
  if (target === undefined) {
    const home = literalText(lookup("HOME", st, ctx));
    st.cwd = home;
    st.cwdWhy = "`cd` with HOME unset";
    return;
  }
  const text = literalText(target.exp);
  if (text === "-" || text === undefined || st.cwd === undefined) {
    st.cwd = undefined;
    st.cwdWhy = `\`cd ${target.text}\``;
    return;
  }
  st.cwd = path.resolve(st.cwd, text);
}

function declaration(builtin: string, list: readonly Arg[], st: State, ctx: Ctx): void {
  const array = list.some((arg) => /^-[a-zA-Z]*[aA]/.test(literalText(arg.exp) ?? ""));
  for (const arg of list) {
    const first = arg.exp[0];
    const equals = arg.text.indexOf("=");
    const variable = equals === -1 ? literalText(arg.exp) : arg.text.slice(0, equals);
    if (variable === undefined || variable.startsWith("-")) continue;
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(variable)) continue;
    if (builtin === "export") st.exported.add(variable);
    if (equals === -1) continue;
    let value =
      first?.kind === "literal" && first.text.startsWith(`${variable}=`)
        ? [
            ...(first.text.length === equals + 1 ? [] : [literal(first.text.slice(equals + 1))]),
            ...arg.exp.slice(1),
          ]
        : [...arg.exp];
    // Tilde expands after an assignment's `=` in a declaration's argument too.
    if (arg.text.charAt(equals + 1) === "~" && arg.text.startsWith(`${variable}=~`)) {
      value = [...unquotedLiteral(arg.text.slice(equals + 1), 0, st, ctx), ...arg.exp.slice(1)];
    }
    st.vars.set(variable, array ? [unknown(`\`$${variable}\`, an array`)] : value);
    if (array) {
      if (arg.text.slice(equals + 1) === "()") {
        st.arrays.set(variable, new Map());
      } else {
        const unknownArray = [unknown(`\`$${variable}\`, an array assignment`)];
        st.arrays.set(variable, new Map([[UNKNOWN_ARRAY_INDEX, unknownArray]]));
      }
    }
  }
}

function checkTargets(
  program: string,
  verb: string,
  targets: readonly Arg[],
  follow: boolean,
  st: State,
  ctx: Ctx,
  site: Site
): void {
  for (const target of targets) {
    const verdict = judgePath(target.exp, st, ctx, { follow, overwrite: false });
    if (!verdict.ok) {
      throw refusal(
        site,
        `${program} would ${verb} \`${target.text}\` (${verdict.resolution})`,
        ctx
      );
    }
    const text = literalText(target.exp);
    if (text !== undefined && (text.startsWith("/") || st.cwd !== undefined)) {
      st.pidFiles.delete(path.resolve(st.cwd ?? "/", text));
    }
  }
}

function checkRecursiveMode(
  program: string,
  list: readonly Arg[],
  st: State,
  ctx: Ctx,
  site: Site
): void {
  const flags = program === "chmod" ? "cfvR" : "cfvRhHLP";
  const found: Arg[] = [];
  let recursive = false;
  let reference = false;
  let rest = false;
  for (const arg of list) {
    const text = literalText(arg.exp);
    if (!rest && text === "--") {
      rest = true;
      continue;
    }
    if (!rest && text !== undefined && text.startsWith("--")) {
      if (text === "--recursive") recursive = true;
      if (text.startsWith("--reference=")) reference = true;
      continue;
    }
    // chmod takes a mode like `-w` or `-rwx` where an option could stand: only letters of its
    // own options make an option.
    if (
      !rest &&
      text !== undefined &&
      /^-[A-Za-z]+$/.test(text) &&
      [...text.slice(1)].every((c) => flags.includes(c))
    ) {
      if (text.includes("R")) recursive = true;
      continue;
    }
    found.push(arg);
  }
  if (!recursive) return;
  const targets = reference ? found : found.slice(1);
  const verb =
    program === "chmod" ? "recursively change the mode of" : "recursively change the owner of";
  checkTargets(`${program} -R`, verb, targets, true, st, ctx, site);
}

function checkFind(list: readonly Arg[], st: State, ctx: Ctx, site: Site): void {
  let i = 0;
  // Leading options: -H, -L, -P, -D <debug>, -O<level>.
  for (; i < list.length; i += 1) {
    const text = literalText(list[i]?.exp);
    if (text === "-H" || text === "-L" || text === "-P" || text?.startsWith("-O")) continue;
    if (text === "-D") {
      i += 1;
      continue;
    }
    break;
  }
  const starts: Arg[] = [];
  for (; i < list.length; i += 1) {
    const text = literalText(list[i]?.exp);
    if (
      text !== undefined &&
      (text.startsWith("-") || text === "(" || text === "!" || text === ")")
    )
      break;
    starts.push(list[i] as Arg);
  }
  const targets = starts.length > 0 ? starts : [{ text: ".", exp: [literal(".")] }];
  const expression = list.slice(i).map((arg) => literalText(arg.exp));
  let action: string | undefined;
  let shell: readonly Arg[] | undefined;
  for (const [index, word] of expression.entries()) {
    if (word === "-delete") action = "find -delete";
    if (word !== "-exec" && word !== "-execdir" && word !== "-ok" && word !== "-okdir") continue;
    const end = expression.findIndex(
      (value, offset) => offset > index && (value === ";" || value === "\\;" || value === "+")
    );
    if (end === -1) continue;
    const inner = unwrap(list.slice(i + index + 1, i + end), st, ctx, site);
    const program = path.basename(literalText(inner.argv[0]?.exp) ?? "");
    if (FILE_COMMANDS.has(program) || program === "xargs") action = `find ${word} ${program}`;
    if (SHELLS.has(program)) {
      action = `find ${word} ${program}`;
      shell = inner.argv;
    }
  }
  if (action === undefined) return;
  checkTargets(action, "delete or change what it finds under", targets, false, st, ctx, site);
  if (shell === undefined) return;
  const found = targets.length === 1 ? (targets[0]?.exp ?? []) : [unknown("a path `find` found")];
  const argv = shell.map((arg) =>
    literalText(arg.exp) === "{}" ? { text: arg.text, exp: found } : arg
  );
  runShell(
    argv.slice(1),
    { args: argv, site, overlay: new Map(), redirects: [], pipeIn: false },
    st,
    ctx
  );
}
function checkXargs(list: readonly Arg[], st: State, ctx: Ctx, site: Site): void {
  const found = operands(list, "nLIPdsEa", [
    "--max-args",
    "--max-procs",
    "--delimiter",
    "--max-chars",
    "--arg-file",
  ]);
  let replacement: string | undefined;
  for (const [index, arg] of list.entries()) {
    const text = literalText(arg.exp);
    if (text === "-i" || text === "--replace") replacement = "{}";
    else if (text === "-I") replacement = literalText(list[index + 1]?.exp);
    else if (text?.startsWith("-I")) replacement = text.slice(2);
    else if (text?.startsWith("--replace=")) replacement = text.slice(10);
  }
  const first = found.operands[0];
  const inner = unwrap(
    first === undefined ? [] : list.slice(list.indexOf(first)),
    st,
    ctx,
    site
  ).argv;
  const replacementName = "LEGION_GUARD_XARGS_REPLACEMENT";
  const argv =
    replacement === undefined
      ? inner
      : inner.map((arg) => {
          const text = literalText(arg.exp);
          return text === undefined || !text.includes(replacement)
            ? arg
            : {
                text: arg.text,
                exp: [literal(text.replaceAll(replacement, `"$${replacementName}"`))],
              };
        });
  const program = path.basename(literalText(argv[0]?.exp) ?? "");
  if (FILE_COMMANDS.has(program) || SIGNAL_COMMANDS.has(program)) {
    throw new Refusal(
      site.snippet,
      site.line,
      `\`xargs ${program}\` takes its targets from standard input, which the guard cannot see; name ` +
        `the paths or pids on the command line (or use \`find <dir> -delete\`) so it can check them`
    );
  }
  const overlay = new Map<string, Expansion>();
  if (replacement !== undefined) {
    overlay.set(replacementName, [unknown("an xargs replacement")]);
  }
  const invocation: Invocation = {
    args: argv,
    site,
    overlay,
    redirects: [],
    pipeIn: false,
  };
  if (SHELLS.has(program)) {
    runShell(argv.slice(1), invocation, st, ctx);
    return;
  }
  const interpreter = interpreterLanguage(program);
  if (interpreter !== undefined) {
    const codeArgv =
      replacement === undefined
        ? argv
        : inner.map((arg) => {
            const text = literalText(arg.exp);
            return text === undefined || !text.includes(replacement)
              ? arg
              : {
                  text: arg.text,
                  exp: [literal(text.replaceAll(replacement, `\\"$${UNKNOWN_MARKER}\\"`))],
                };
          });
    runInterpreter(
      program,
      interpreter,
      codeArgv.slice(1),
      { ...invocation, args: codeArgv },
      st,
      ctx
    );
  }
}
function checkKill(list: readonly Arg[], ctx: Ctx, site: Site): void {
  const words = list.map((arg) => literalText(arg.exp));
  if (
    words.some((word) => word === "-l" || word === "-L" || word === "--list" || word === "--table")
  ) {
    return;
  }
  let i = 0;
  let signal: string | undefined;
  if (words[0] === "-s" || words[0] === "-n" || words[0] === "--signal") {
    signal = words[1];
    i = 2;
  } else if (words[0]?.startsWith("-") && words[0] !== "--") {
    signal = words[0].slice(1);
    i = 1;
  }
  if (words[i] === "--") i += 1;
  // Signal 0 only asks whether the process exists.
  if (signal === "0") return;
  for (const arg of list.slice(i)) {
    const text = literalText(arg.exp);
    if (text === "") continue;
    if (arg.exp.some((p) => p.lenient)) continue;
    if (arg.exp.length > 0 && arg.exp.every((piece) => piece.descendantPid)) continue;
    if (text?.startsWith("%")) continue;
    if (text === undefined || !/^-?[0-9]+$/.test(text)) {
      throw new Refusal(
        site.snippet,
        site.line,
        `the guard cannot resolve the pid \`${arg.text}\` (${arg.exp.find((p) => p.kind !== "literal")?.why ?? "not a number"}), so it cannot check that the process descends from this pane's Oh My Pi process; name the pid of a child you started`
      );
    }
    const pid = Math.abs(Number(text));
    const verdict = descendant(pid, ctx);
    if (verdict !== true) {
      const target = text.startsWith("-") ? `process group ${pid}` : `pid ${pid}`;
      throw new Refusal(
        site.snippet,
        site.line,
        `${target} ${verdict}, and a pane signals only processes it started. Stop a process you ` +
          'started with the hub tool (`op: "stop"`), or `kill` the pid of a child you started from this pane'
      );
    }
  }
}

/** True when `pid` descends from (and is not) this pane's Oh My Pi process, else why not. A pid
 * that no longer exists is fine: nothing receives the signal. */
function descendant(pid: number, ctx: Ctx): true | string {
  if (pid === 0 || pid === 1) {
    return pid === 0
      ? "is the caller's whole process group, which holds this pane's Oh My Pi process"
      : "is init, or with a minus sign every process the user owns";
  }
  if (pid === ctx.ompPid) return "is this pane's own Oh My Pi process";
  let current: number | undefined = pid;
  for (let hops = 0; hops < 512 && current !== undefined; hops += 1) {
    let parent: number | undefined;
    try {
      parent = ctx.parentOf(current);
    } catch (error) {
      return `could not be traced to this pane's Oh My Pi process (${messageFor(error)})`;
    }
    if (parent === undefined)
      return hops === 0
        ? true
        : `is not a descendant of this pane's Oh My Pi process (pid ${ctx.ompPid})`;
    if (parent === ctx.ompPid) return true;
    if (parent <= 1) break;
    current = parent;
  }
  return `is not a descendant of this pane's Oh My Pi process (pid ${ctx.ompPid})`;
}

// --- Scripts a command runs ------------------------------------------------------------------

/** The state of a shell a command starts: exported variables and the command's own assignment
 * prefix, the same working directory, and `$$` a descendant. */
function childState(st: State, overlay: ReadonlyMap<string, Expansion>): State {
  const vars = new Map<string, Expansion>();
  for (const name of st.exported) {
    const value = st.vars.get(name);
    if (value !== undefined) vars.set(name, value);
  }
  for (const [name, value] of overlay) vars.set(name, value);
  return {
    vars,
    exported: new Set(vars.keys()),
    cwd: st.cwd,
    cwdWhy: st.cwdWhy,
    positional: [],
    files: st.files,
    backgroundStarted: false,
    nested: true,
    depth: st.depth + 1,
    source: "",
    script: undefined,
    argv0: undefined,
    output: undefined,
    pidFiles: st.pidFiles,
    arrays: new Map(),
    functions: new Map(),
    traps: [],
    runningFunctions: new Set(),
  };
}

function runText(text: string, label: string, st: State, ctx: Ctx, site: Site): void {
  if (st.depth >= MAX_DEPTH) {
    throw new Refusal(
      site.snippet,
      site.line,
      "the guard stops following scripts this deep; run the inner script directly"
    );
  }
  try {
    walkScript(parse(text), { ...st, source: text, depth: st.depth + 1 }, ctx);
  } catch (error) {
    wrapRefusal(error, site, label);
  }
}

function runShell(list: readonly Arg[], invocation: Invocation, st: State, ctx: Ctx): void {
  const site = invocation.site;
  let command = false;
  let i = 0;
  for (; i < list.length; i += 1) {
    const text = literalText(list[i]?.exp);
    if (text === undefined) break;
    if (text === "--") {
      i += 1;
      break;
    }
    if (["-o", "+o", "-O", "+O", "--rcfile", "--init-file"].includes(text)) {
      i += 1;
      continue;
    }
    if (/^[-+][A-Za-z]+$/.test(text)) {
      // `bash -n <file>` reads the script for syntax and runs none of it.
      if (text.startsWith("-") && text.includes("n")) return;
      if (text.startsWith("-") && text.includes("c")) command = true;
      // `-euo pipefail`: a cluster ending in `o` or `O` takes the next word as its value.
      if (/[oO]$/.test(text)) i += 1;
      continue;
    }
    if (text.startsWith("--")) continue;
    break;
  }
  const operand = list.slice(i);
  const child = childState(st, invocation.overlay);
  child.argv0 = operand[1]?.exp;
  if (command) {
    const text = operand[0] === undefined ? "" : runtimeText(operand[0].exp);
    child.positional = positionalOf(operand.slice(2));
    runText(text, "its `-c` text", child, ctx, site);
    return;
  }
  const file = operand[0];
  if (file !== undefined) {
    child.positional = positionalOf(operand.slice(1));
    runFile(file, operand.slice(1), child, ctx, site, "shell");
    return;
  }
  // No -c and no file: the shell reads its script from standard input.
  const heredoc = invocation.redirects.find((r) => r.operator === "<<" || r.operator === "<<-");
  if (heredoc?.content !== undefined) {
    runText(heredoc.content, "the script it reads from the heredoc", child, ctx, site);
    return;
  }
  const herestring = invocation.redirects.find((r) => r.operator === "<<<");
  if (herestring !== undefined) {
    const text = herestringText(herestring, st, ctx);
    if (text === null) {
      throw new Refusal(
        site.snippet,
        site.line,
        "the guard cannot read the script this shell reads from its here-string"
      );
    }
    runText(text, "the script it reads from the here-string", child, ctx, site);
    return;
  }
  const input = invocation.redirects.find((r) => r.operator === "<");
  if (input?.target !== undefined) {
    const [exp] = expandWord(input.target, st, ctx);
    runFile({ text: input.target.text, exp: exp ?? [] }, [], child, ctx, site, "shell");
    return;
  }
  if (invocation.pipeIn) {
    throw new Refusal(
      site.snippet,
      site.line,
      "a shell reading its script from a pipe runs text the guard cannot see; write the script to a file and run that"
    );
  }
}

function interpreterLanguage(base: string): CodeLanguage | undefined {
  if (/^python[0-9.]*$/.test(base)) return "py";
  if (base === "node" || base === "bun" || base === "deno" || base === "tsx") return "js";
  return undefined;
}

function runInterpreter(
  base: string,
  language: CodeLanguage,
  list: readonly Arg[],
  invocation: Invocation,
  st: State,
  ctx: Ctx
): void {
  const site = invocation.site;
  const words = list.map((arg) => literalText(arg.exp));
  const codeFlags = language === "py" ? ["-c"] : ["-e", "--eval", "-p", "--print"];
  const valued =
    language === "py" ? ["-W", "-X", "-m"] : ["-r", "--require", "--import", "--loader", "-C"];
  let i = 0;
  if (base === "bun" && (words[0] === "run" || words[0] === "x")) i = 1;
  if (base === "deno" && (words[0] === "run" || words[0] === "eval")) {
    if (words[0] === "eval") {
      runCode(language, list[1], invocation, st, ctx, site);
      return;
    }
    i = 1;
  }
  for (; i < words.length; i += 1) {
    const word = words[i];
    if (word === undefined) return;
    if (codeFlags.includes(word)) {
      runCode(language, list[i + 1], invocation, st, ctx, site);
      return;
    }
    if (language === "py" && word === "-m") return;
    // `python3 - <<'PY'`: the program is standard input.
    if (word === "-") {
      const heredoc = invocation.redirects.find((r) => r.operator === "<<" || r.operator === "<<-");
      if (heredoc?.content !== undefined) {
        runCode(
          language,
          { text: "<heredoc>", exp: [literal(heredoc.content)] },
          invocation,
          st,
          ctx,
          site
        );
      }
      return;
    }
    if (valued.includes(word)) {
      i += 1;
      continue;
    }
    if (word.startsWith("-")) continue;
    // `bun test`, `bun install` and other subcommands are not script files.
    const file = list[i] as Arg;
    if (base === "bun" && !/\.[cm]?[jt]sx?$/.test(word)) return;
    runFile(file, list.slice(i + 1), childState(st, invocation.overlay), ctx, site, language);
    return;
  }
}

function runCode(
  language: CodeLanguage,
  code: Arg | undefined,
  invocation: Invocation,
  st: State,
  ctx: Ctx,
  site: Site
): void {
  if (code === undefined) return;
  try {
    checkCode(language, runtimeText(code.exp), childState(st, invocation.overlay), ctx);
  } catch (error) {
    wrapRefusal(error, site, "its code");
  }
}

/** Reads and checks the script at `file`: a shell script, or Python/JavaScript by extension or
 * shebang (`auto`). A file written earlier in this same command is read from what the command
 * writes; a file that does not exist runs nothing. */
function runFile(
  file: Arg,
  positional: readonly Arg[],
  st: State,
  ctx: Ctx,
  site: Site,
  kind: "shell" | "auto" | CodeLanguage,
  source = false
): void {
  const text = literalText(file.exp);
  if (text === undefined || (!text.startsWith("/") && st.cwd === undefined)) {
    throw new Refusal(
      site.snippet,
      site.line,
      `the guard cannot resolve the script \`${file.text}\` (${file.exp.find((p) => p.kind !== "literal")?.why ?? `a working directory unknown after ${st.cwdWhy}`}), so it cannot read what it runs`
    );
  }
  const abs = path.resolve(st.cwd ?? "/", text);
  let content: string | undefined;
  if (st.files.has(abs)) {
    const written = st.files.get(abs);
    if (written === null || written === undefined) {
      throw new Refusal(
        site.snippet,
        site.line,
        `this command writes ${abs} in a way the guard cannot read before running it; write the script with the write tool first`
      );
    }
    content = written;
  } else {
    let size: number;
    try {
      const info = statSync(abs);
      if (!info.isFile()) return;
      size = info.size;
    } catch {
      return;
    }
    try {
      // A program run by path is a script only when its shebang or extension says so; a compiled
      // one is the documented residual.
      if (kind === "auto" && scriptKind(abs, fileHead(abs)) === undefined) return;
      if (size > MAX_SCRIPT_BYTES) {
        throw new Refusal(
          site.snippet,
          site.line,
          `the script ${abs} is larger than the guard reads (${MAX_SCRIPT_BYTES} bytes)`
        );
      }
      content = readFileSync(abs, "utf8");
    } catch (error) {
      if (error instanceof Refusal) throw error;
      throw new Refusal(
        site.snippet,
        site.line,
        `the guard cannot read the script ${abs} (${messageFor(error)})`
      );
    }
  }
  const language = kind === "auto" ? scriptKind(abs, content) : kind;
  if (language === undefined) return;
  const label = abs;
  try {
    if (language === "shell") {
      if (st.depth >= MAX_DEPTH) {
        throw new Refusal(
          site.snippet,
          site.line,
          "the guard stops following scripts this deep; run the inner script directly"
        );
      }
      const child: State = { ...clone(st), source: content, script: abs, depth: st.depth + 1 };
      const operands = positionalOf(positional);
      // `. file` with no operands (none left once empty unquoted ones drop) runs the file with this
      // shell's own arguments.
      const noOperands = operands !== undefined && operands.length === 0;
      child.positional = source && noOperands ? st.positional : operands;
      walkScript(parse(content), child, ctx, !source);
      if (source) {
        // A sourced file runs in this shell, so everything it leaves stays: the copy-back rule on
        // `merge` holds here too. Not carried here: `files` and `pidFiles` (shared), and
        // `runningFunctions`. The arguments: with no operands the file changed this shell's own,
        // which stay changed; with operands bash restores this shell's afterwards unless the file
        // set new ones with `set`, which the guard does not tell apart from a `shift` bash undoes,
        // so a list the file touched is unknown. Touched, not changed: `shift` and `set --` each
        // leave a new list, so identity tells a file that never touched them from one that ran
        // `set -- "$@"`, which bash keeps.
        st.positional = noOperands
          ? child.positional
          : child.positional === operands
            ? st.positional
            : undefined;
        st.vars = child.vars;
        st.exported = child.exported;
        st.arrays.clear();
        for (const [name, elements] of child.arrays) st.arrays.set(name, new Map(elements));
        st.cwd = child.cwd;
        st.cwdWhy = child.cwdWhy;
        st.functions = child.functions;
        st.traps = child.traps;
        st.output = child.output;
        st.backgroundStarted ||= child.backgroundStarted;
      }
    } else checkCode(language, content, st, ctx);
  } catch (error) {
    wrapRefusal(error, site, label);
  }
}

/** The first line's worth of a file, enough for its shebang. */
function fileHead(file: string): string {
  const descriptor = openSync(file, "r");
  try {
    const buffer = Buffer.alloc(256);
    const read = readSync(descriptor, buffer, 0, buffer.length, 0);
    return buffer.subarray(0, read).toString("utf8");
  } finally {
    closeSync(descriptor);
  }
}

function scriptKind(file: string, content: string): "shell" | CodeLanguage | undefined {
  const shebang = content.startsWith("#!") ? (content.split("\n", 1)[0] ?? "") : "";
  if (/\b(?:ba|da|z|k|mk)?sh\b/.test(shebang)) return "shell";
  if (/\bpython[0-9.]*\b/.test(shebang)) return "py";
  if (/\b(?:node|bun|deno|tsx)\b/.test(shebang)) return "js";
  if (shebang !== "") return undefined;
  if (/\.(?:sh|bash)$/.test(file)) return "shell";
  if (/\.py$/.test(file)) return "py";
  if (/\.[cm]?[jt]sx?$/.test(file)) return "js";
  return undefined;
}

function checkCode(
  language: CodeLanguage,
  code: string,
  st: State,
  ctx: Ctx,
  ipython = false
): void {
  const cwd = st.cwd;
  const env = { ...ctx.env };
  for (const [name, value] of st.vars) {
    const text = literalText(value);
    if (text !== undefined) env[name] = text;
  }
  scanCode(
    language,
    code,
    { env, cwd: cwd ?? "/", tmpdir: env.TMPDIR ?? "/tmp", ipython },
    {
      path: (call, verb, target) => {
        const pieces: Piece[] = [];
        let cursor = 0;
        for (const name of target.unknown) {
          const marker = `"$${name}"`;
          const at = target.text.indexOf(marker, cursor);
          if (at === -1) continue;
          if (at > cursor) pieces.push(literal(target.text.slice(cursor, at)));
          pieces.push({ ...unknown(`a \`${call}\` interpolation`), lenient: true });
          cursor = at + marker.length;
        }
        if (cursor < target.text.length) pieces.push(literal(target.text.slice(cursor)));
        if (pieces.length === 0) pieces.push(literal(target.text));
        const verdict = judgePath(pieces, { ...st, cwd: cwd ?? "/" }, ctx, {
          follow: verb !== "delete" && verb !== "move",
          overwrite: verb === "overwrite",
        });
        if (!verdict.ok) {
          throw refusal(
            { snippet: call, line: 1 },
            `${call} would ${verb} ${verdict.resolution}`,
            ctx
          );
        }
      },
      signal: (call, pid, group) => {
        if (!/^-?[0-9]+$/.test(pid)) return;
        const verdict = descendant(Math.abs(Number(pid)), ctx);
        if (verdict !== true) {
          throw new Refusal(
            call,
            1,
            `${group ? "process group" : "pid"} ${pid} ${verdict}, and a pane signals only processes it started`
          );
        }
      },
      shell: (call, text, unknownNames) => {
        const child = clone(st);
        for (const name of unknownNames) {
          child.vars.set(name, [{ ...unknown(`a \`${call}\` interpolation`), lenient: true }]);
        }
        try {
          walkScript(
            parse(text),
            { ...child, source: text, nested: true, depth: st.depth + 1 },
            ctx
          );
        } catch (error) {
          wrapRefusal(error, { snippet: `${call}(...)`, line: 1 }, "its shell text");
        }
      },
      argv: (call, list) => {
        const argv: Arg[] = list.map((item) => ({
          text: item ?? "<unknown>",
          exp: [
            item === undefined
              ? { ...unknown(`an argument of \`${call}\``), lenient: true as const }
              : literal(item),
          ],
        }));
        const site = { snippet: `${call}(...)`, line: 1 };
        dispatch(
          { args: argv, site, overlay: new Map(), redirects: [], pipeIn: false },
          clone(st),
          ctx
        );
      },
    }
  );
}

// --- Entry points ----------------------------------------------------------------------------

/** The pane's parent-pid lookup from `/proc`: undefined when the process no longer exists. */
function procParent(pid: number): number | undefined {
  let stat: string;
  try {
    stat = readFileSync(`/proc/${pid}/stat`, "utf8");
  } catch (error) {
    if (!existsSync("/proc/self/stat")) throw new Error("/proc is not available");
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }
  // The command name is parenthesised and may itself hold spaces or parentheses.
  const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
  return Number(fields[1]);
}

export interface PaneGuardOptions {
  /** The pane's issue workspace (`LEGION_WORKSPACE`); undefined leaves only the scratch root. */
  readonly workspace: string | undefined;
  /** This pane's Oh My Pi process: the ancestor every signalled process must descend from. */
  readonly ompPid: number;
  /** The scratch root; `/tmp` in a pane, a temporary directory in a test. */
  readonly scratch?: string;
  /** A process's parent pid, or undefined when it no longer exists; `/proc` in a pane. */
  readonly parentOf?: (pid: number) => number | undefined;
}

export interface PaneGuard {
  /** The refusal for a `bash` command, or undefined when it may run. */
  readonly bash: (command: string, cwd: string, env: NodeJS.ProcessEnv) => string | undefined;
  /** The refusal for a program started without a shell (a `hub` process start). */
  readonly argv: (
    argv: readonly string[],
    cwd: string,
    env: NodeJS.ProcessEnv
  ) => string | undefined;
  /** The refusal for `eval` tool code. */
  readonly code: (
    language: CodeLanguage,
    code: string,
    cwd: string,
    env: NodeJS.ProcessEnv
  ) => string | undefined;
}

export function createPaneGuard(options: PaneGuardOptions): PaneGuard {
  const scratch = realish(options.scratch ?? "/tmp", true);
  const roots = (env: NodeJS.ProcessEnv): Roots => {
    let workspace =
      options.workspace === undefined || options.workspace === ""
        ? undefined
        : realish(options.workspace, true);
    const home = env.HOME === undefined ? undefined : realish(env.HOME, true);
    // A workspace that is the filesystem root or holds the home directory protects nothing.
    if (
      workspace === "/" ||
      (home !== undefined && (home === workspace || home.startsWith(`${workspace}/`)))
    ) {
      workspace = undefined;
    }
    // A home or tmux directory under the scratch root is not the pane's scratch: the /tmp
    // directory holding it is protected whole, as a rig's run directory holds its OMP home.
    const tmuxDir = env.TMUX_TMPDIR === undefined ? undefined : realish(env.TMUX_TMPDIR, true);
    const protectedDirectories: string[] = [];
    for (const owned of [tmuxDir, home]) {
      const component = owned?.startsWith(`${scratch}/`)
        ? owned.slice(scratch.length + 1).split("/")[0]
        : undefined;
      if (component !== undefined && component !== "") protectedDirectories.push(component);
    }
    return { workspace, scratch, protectedScratch: PROTECTED_SCRATCH, protectedDirectories };
  };
  const context = (env: NodeJS.ProcessEnv): Ctx => ({
    roots: roots(env),
    env,
    ompPid: options.ompPid,
    parentOf: options.parentOf ?? procParent,
    steps: { count: 0 },
  });
  const initial = (cwd: string, source: string): State => ({
    vars: new Map(),
    exported: new Set(),
    cwd,
    cwdWhy: "",
    positional: undefined,
    files: new Map(),
    backgroundStarted: false,
    nested: false,
    depth: 0,
    source,
    runningFunctions: new Set(),
    script: undefined,
    argv0: undefined,
    output: undefined,
    pidFiles: new Map(),
    arrays: new Map(),
    functions: new Map(),
    traps: [],
  });
  const run = (attempt: () => void, fallback: string): string | undefined => {
    try {
      attempt();
      return undefined;
    } catch (error) {
      if (!(error instanceof Refusal)) throw error;
      return `refused \`${error.snippet || fallback}\`: ${error.detail}`;
    }
  };
  return {
    bash: (command, cwd, env) => {
      const ctx = context(env);
      return run(() => walkScript(parse(command), initial(cwd, command), ctx), command.trim());
    },
    argv: (argv, cwd, env) => {
      const ctx = context(env);
      const list: Arg[] = argv.map((item) => ({ text: item, exp: [literal(item)] }));
      const snippet = argv.join(" ");
      return run(
        () =>
          dispatch(
            {
              args: list,
              site: { snippet, line: 1 },
              overlay: new Map(),
              redirects: [],
              pipeIn: false,
            },
            initial(cwd, snippet),
            ctx
          ),
        snippet
      );
    },
    code: (language, code, cwd, env) => {
      const ctx = context(env);
      return run(
        () => checkCode(language, code, initial(cwd, code), ctx, true),
        `${language} code`
      );
    },
  };
}
