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
 * directory below `/tmp` except `/tmp` itself, a glob over it, the tmux and ssh socket directories
 * there, or the one holding the pane's `HOME` or `TMUX_TMPDIR`. The guard cannot tell which other
 * permitted `/tmp` directory belongs to the pane. A pane signals only processes descended from its
 * own Oh My Pi process.
 *
 * Commands are parsed by `unbash`, a bash parser, and the guard follows what bash would do with
 * them: quoting, `$HOME`, `~`, variables assigned earlier in the same command (`$(mktemp -d)`
 * included), `cd`, brace expansion, command substitutions (which run), subshells, control flow,
 * functions, and the scripts a command runs (`bash <file>`, `sh -c '<text>'`, `source`, a heredoc
 * piped into a shell, a script executed by path, and python/node/bun scripts through
 * `pane-guard-code.ts`). A command the parser reports as malformed is refused, and so is a target
 * the guard cannot resolve (a variable read from input, a command's output): it never guesses.
 * A command whose walk visits more than `MAX_WALK_STEPS` nodes is refused, never allowed unread, and
 * no value the guard builds is longer than `MAX_VALUE_LENGTH`: past it (a replacement of a
 * replacement, `x="$x$x"` repeated) a value is unknown, since building one held the pane. A script
 * a command writes is read whole up to `MAX_SCRIPT_BYTES`, as one on disk is, and past it is one
 * the guard cannot read (`readable`). A `>>` append adds to the model the guard holds of that
 * file: with no model the file is one it cannot read, never an empty one, or a no-op append would
 * leave it reading the file as holding only what was appended (LEGION-349). The model's own
 * property is narrower than "a file this command wrote": an entry is whatever the redirection
 * path and `tee` record into this command's state, so it takes a write whose content the guard
 * renders (`echo >`, `printf >`, `cat > <<EOF`, `tee f <<EOF`) and it takes one from anywhere the
 * walk carried that state — a region the shell may never enter included, since `if`, `else`, `&&`,
 * `||`, `case`, `while`, `until` and a `for` whose word list the guard cannot decide are all
 * walked. Such a write seeds a model for code that never runs, the residual LEGION-354 closes.
 * A write the walk never reaches records nothing, and neither does one it judges elsewhere: a
 * `trap` handler's body is read and refused on its own line, but its writes never reach this
 * model, not even on `EXIT`. `tee -a` is stricter
 * and deliberately so: it leaves the file unknown whatever the model, since the guard does not
 * render what `tee` writes, so appending with `tee -a` to a file this command wrote and then
 * running it is refused where `>>` is allowed. Neither rule asks the filesystem what a file
 * holds: at check time that is a state an earlier stage of the same command can choose.
 *
 * Every value the guard produces is known, unset, or unknown, and never one standing in for
 * another: a value it cannot know taken as some harmless concrete one (the empty string, the text
 * left as it was, a parameter taken as unset) turns a refusal into a silent allow. So an argument
 * of a shell whose arguments it does not know is unknown, set or not; `unset` leaves a name unset
 * rather than empty; a pattern it cannot split as bash does leaves the whole expansion unknown; and
 * a word that may be several arguments or none leaves the arguments it is among unknown.
 *
 * The same holds for writes: a write the guard sees a command make to a variable reaches its model,
 * and one it cannot model leaves the value unknown rather than the one already approved. A builtin
 * that assigns by name (`printf -v`, `read`, `mapfile`, `getopts`, `declare` and its kin, `unset`,
 * `wait -p`), arithmetic, `${v:=x}`, a loop variable and a plain assignment to an array's name all
 * assign as bash does; a function's `local` is the caller's variable again once it returns; a
 * name with `readonly`, `-i`, `-l` or `-u` is unknown after a write, which bash refuses or
 * rewrites; and a write under a name the guard cannot read, or `eval` of text it cannot read,
 * leaves every variable unknown and possibly unset, the pane's environment included. A nameref
 * (`declare -n`) is refused, since its writes land elsewhere. Text `eval` runs that the guard
 * cannot read is outside what it sees in any case (`docs/deployment.md`, "The pane guard"):
 * besides running commands the guard never judges, it can make a name read-only, so that a later
 * write the guard trusts is refused. Two writes it does not follow: a `source` of a path it cannot
 * read at check time (a process substitution, `/dev/fd/N`, a file a command before it creates),
 * which `runFile` takes as sourcing nothing (LEGION-332), and an assignment to `IFS`, since it
 * splits an unquoted value on whitespace alone.
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
  ANY_ONE,
  ANY_RUN,
  evaluateTest,
  type Glob,
  globMatches,
  globWork,
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
  /** Only on `UNSET`'s piece. */
  readonly unset?: true;
  /** Only on `NO_ELEMENTS`'s piece. */
  readonly noElements?: true;
  /** A value whose name may also be unset: a branch may have unset it, or a write the guard could
   * not read (`forgetVariables`). Whether it is set is unknown, so `-` and `+` cannot decide. */
  readonly maybeUnset?: true;
  /** A value whose name has an attribute with which bash refuses a later write (`readonly`,
   * `declare -r`) or rewrites it (`-i`, `-l`, `-u`): such a write leaves the name unknown
   * (`attributed`), whichever bash did. */
  readonly rewrites?: true;
}
type Expansion = readonly Piece[];

/** What `unset NAME` and `env -u NAME` leave for NAME: unset, which is neither empty (`${NAME-x}`
 * takes `x`) nor absent (the pane's environment no longer supplies it). */
const UNSET: Expansion = [{ kind: "literal", text: "", unset: true }];

function isUnset(value: Expansion | undefined): boolean {
  return value?.length === 1 && value[0]?.unset === true;
}

/** `$@`, `$*` or `${arr[@]}` over no elements: empty text, and no argument when it is all of a word
 * (`f "$@" x` gives f one argument). An element that is empty is an ordinary empty piece. */
const NO_ELEMENTS: Piece = { kind: "literal", text: "", noElements: true };

/** A value as a variable or element holds it. `NO_ELEMENTS` marks a word, not a value: what
 * `x="$@"` assigns from no elements is the empty string, which `"$x"` passes as one argument, so
 * the mark never outlives the word it came from. */
function stored(value: Expansion): Expansion {
  if (!value.some((piece) => piece.noElements === true)) return value;
  return value.map((piece) => (piece.noElements === true ? literal("") : piece));
}

/** The key under which `vars` holds the value of every name it does not list, once the shell ran
 * a write the guard could not read (`forgetVariables`): any name, one from the pane's environment
 * included, may since have been assigned or unset. */
const ANY_NAME = "\u0000";
const IDENTIFIER = /^[A-Za-z_][A-Za-z0-9_]*$/;
const ARRAY_ELEMENT = /^([A-Za-z_][A-Za-z0-9_]*)\[.*\]$/s;

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
  /** How many bodies the walk has entered that the shell may never enter, or may not have
   * finished, by the time a later command reads what they wrote (`uncertainly`, `modelWrite`).
   * Where the walk is, not what the shell holds, so it lives here and not in `State`. */
  readonly uncertain: { depth: number };
}

/** Why the guard cannot read what a command wrote to a file. A refusal names it, so an agent
 * rewrites for the reason that actually fired: one text serving every cause sent it looking for
 * output it could not render when the write was in a branch, or an append to a file nothing in
 * the command had written. */
interface UnknownContents {
  readonly why: string;
}

/** What a command wrote to a file: the text, or why the guard cannot know it. */
type FileContents = string | UnknownContents;

/** What a command wrote to a file before it ran it: its text, or why the guard cannot know it.
 * Shared by every subshell of one command, and exposing no `set`, so `modelWrite` is the only way
 * a write is recorded and no later site can record one without the rule that governs it. */
interface FileModel {
  has(file: string): boolean;
  get(file: string): FileContents | undefined;
}

/** Files this shell wrote with a pid it started, indexed by resolved path. `delete` stays, since
 * forgetting one only makes the file unknown; `modelPidFile` is the only writer. */
interface PidFileModel {
  get(file: string): Expansion | undefined;
  delete(file: string): boolean;
}

interface State {
  vars: Map<string, Expansion>;
  exported: Set<string>;
  cwd: string | undefined;
  cwdWhy: string;
  positional: readonly Expansion[] | undefined;
  readonly files: FileModel;
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
  readonly pidFiles: PidFileModel;
  /** Per-element values of shell arrays. */
  readonly arrays: Map<string, Map<string, Expansion>>;
  /** Functions available in this shell, with their definition source for diagnostic locations. */
  functions: Map<string, Callee>;
  /** Functions whose current body walk has not returned; recursive calls add no new code to check. */
  readonly runningFunctions: Set<string>;
  /** The handlers this shell set with `trap`. Each body runs when this shell finishes, against its
   * state after its last statement; a subshell starts with none (`subshell`). */
  traps: readonly Trap[];
  /** Inside a function: the names it made local (`local`, or `declare` and `typeset` without
   * `-g`), which bash gives back to the caller when it returns; true for a name every path so far
   * made local, false for one only some did. Undefined outside a function. */
  locals: Map<string, boolean> | undefined;
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
  /** Set inside a body the shell may never enter, so the handler itself may never be set, and
   * even an `EXIT` one may never run (`runSetTraps`). A `merge` that lifts a handler out of a
   * branch stamps it too, since `AndOr` leaves one in this shell with no merge at all. */
  readonly uncertainSet?: boolean;
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
const WALK_LIMIT = `the guard reached its walk limit of ${MAX_WALK_STEPS} nodes; run a smaller script`;
const MAX_ALTERNATIVES = 64;
/** The longest value a pattern expansion is evaluated over; `//` matches from every start of it. */
const MAX_PATTERN_VALUE = 512;
/** The pattern matching work (`globWork`) one walk step stands for, so the budget bounds the time
 * a walk spends matching as it bounds its nodes. Measured on a loaded devbox: a budget's worth of
 * it takes 0.2 s in one `//` over 512 characters with a 762-position pattern, and 0.4 to 0.8 s
 * spread over thousands of expansions or `case` items, against 0.86 s for 100,000 `rm -rf a;`.
 * `globWork` charges every pass its full length, where a pass stops once no pattern position is
 * left, so the charge errs high: too high refuses a command, too low would bring the time back. */
const PATTERN_WORK_PER_STEP = 1024;
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
/** `find` predicates that take the word after them as their value, so that word is no predicate
 * of its own and `-name "$pattern"` is not a `-delete` the guard failed to read. What the note
 * above costs here: a `Record` would answer a bare `constructor` word, which would then swallow
 * the word after it as its value and skip the refusal. `-fprintf` takes two words and is listed
 * as taking one: reading its second word as a possible predicate refuses where it need not,
 * which is the direction a miss here has to fall. */
const FIND_VALUED_PREDICATES = new Set([
  "-amin",
  "-anewer",
  "-atime",
  "-cmin",
  "-cnewer",
  "-ctime",
  "-fls",
  "-fprint",
  "-fprint0",
  "-fprintf",
  "-fstype",
  "-gid",
  "-group",
  "-ilname",
  "-iname",
  "-inum",
  "-ipath",
  "-iregex",
  "-iwholename",
  "-links",
  "-lname",
  "-maxdepth",
  "-mindepth",
  "-mmin",
  "-mtime",
  "-name",
  "-newer",
  "-newerat",
  "-newerct",
  "-newermt",
  "-path",
  "-perm",
  "-printf",
  "-regex",
  "-regextype",
  "-samefile",
  "-size",
  "-type",
  "-uid",
  "-used",
  "-user",
  "-wholename",
  "-xtype",
]);

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

/** A variable's value: this command's assignments, then the pane's environment; undefined for one
 * this command unset. Once a write the guard could not read ran, a name it does not list is
 * unknown (`ANY_NAME`). */
function lookup(name: string, st: State, ctx: Ctx): Expansion | undefined {
  const own = st.vars.get(name);
  if (own !== undefined) return isUnset(own) ? undefined : own;
  const any = st.vars.get(ANY_NAME)?.[0];
  if (any !== undefined) return [{ ...any, why: `\`$${name}\`, which ${any.why}` }];
  if (name === "PWD" && st.cwd !== undefined) return [literal(st.cwd)];
  const value = ctx.env[name];
  return value === undefined ? undefined : [literal(value)];
}

/** `name=value` as bash makes it: an array of that name keeps its other elements and holds the
 * value at 0. A name with an attribute that refuses or rewrites the write is unknown after. */
function assignScalar(st: State, name: string, value: Expansion): void {
  if (attributed(st, name)) {
    keepAttributed(st, name);
    return;
  }
  const kept = stored(value);
  st.vars.set(name, kept);
  const elements = st.arrays.get(name);
  if (elements !== undefined) st.arrays.set(name, new Map(elements).set("0", kept));
}

/** Whether `name` has an attribute bash refuses or rewrites a write with (`Piece.rewrites`). */
function attributed(st: State, name: string): boolean {
  return st.vars.get(name)?.some((piece) => piece.rewrites === true) === true;
}

/** A write to an attributed name: bash refused it or rewrote the value, so the name, and each of
 * its elements, is unknown and keeps the attribute, and may be unset if it was, or may have been,
 * before (a refused write leaves it as it was). */
function keepAttributed(st: State, name: string, unsets = false): void {
  const before = st.vars.get(name);
  const maybeUnset =
    unsets ||
    before === undefined ||
    isUnset(before) ||
    before.some((piece) => piece.maybeUnset === true);
  const kept: Expansion = [
    {
      ...unknown(
        `\`$${name}\`, whose \`readonly\`, \`-i\`, \`-l\` or \`-u\` refuses or rewrites a write`
      ),
      rewrites: true,
      ...(maybeUnset ? { maybeUnset: true as const } : {}),
    },
  ];
  st.vars.set(name, kept);
  if (st.arrays.has(name)) st.arrays.set(name, new Map([[UNKNOWN_ARRAY_INDEX, kept]]));
}

/** A builtin's write to the variable it names (`printf -v`, `read`, `declare`, `wait -p`, an
 * arithmetic assignment): a plain name as `assignScalar`; an array element (`d[0]`), whose index
 * the guard does not evaluate there, leaves that array and its name unknown; and a name the guard
 * cannot read may be any variable (`forgetVariables`). A name bash refuses as no identifier
 * assigns nothing. `writer` names the write for a refusal. */
function assignByName(st: State, name: string | undefined, value: Expansion, writer: string): void {
  if (name === undefined) {
    forgetVariables(st, `${writer} under a name the guard cannot read may have changed`);
    return;
  }
  if (IDENTIFIER.test(name)) {
    assignScalar(st, name, value);
    return;
  }
  const base = ARRAY_ELEMENT.exec(name)?.[1];
  if (base === undefined) return;
  if (attributed(st, base)) {
    keepAttributed(st, base, true);
    return;
  }
  // `unset 'd[0]'` of an array's last element leaves it unset.
  const element: Expansion = [
    { ...unknown(`\`$${base}\`, whose element ${writer} assigns`), maybeUnset: true },
  ];
  st.vars.set(base, element);
  st.arrays.set(base, new Map([[UNKNOWN_ARRAY_INDEX, element]]));
}

/** After a write the guard cannot read: any variable may have been assigned or unset, so every
 * value this shell holds, and every name it does not list (`ANY_NAME`), is unknown and may be
 * unset until the command assigns it again. `why` completes "`$NAME`, which …", naming the write,
 * so a refusal says what made the name unknown. */
function forgetVariables(st: State, why: string): void {
  const forgotten = (name: string): Expansion => [
    {
      ...unknown(`\`$${name}\`, which ${why}`),
      maybeUnset: true,
      ...(attributed(st, name) ? { rewrites: true as const } : {}),
    },
  ];
  for (const name of st.vars.keys()) if (name !== ANY_NAME) st.vars.set(name, forgotten(name));
  st.vars.set(ANY_NAME, [{ ...unknown(why), maybeUnset: true }]);
  for (const name of st.arrays.keys()) {
    st.arrays.set(name, new Map([[UNKNOWN_ARRAY_INDEX, forgotten(name)]]));
  }
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
    return elements(st.positional, name === "*" && quoted);
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
  // Unset in this command, a name expands to nothing.
  if (isUnset(st.vars.get(name))) return [[literal("")]];
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

/** A list's elements as `$@` or `${arr[@]}` expands them, one word each (`NO_ELEMENTS` for none),
 * or `joined`, as a quoted `$*` or `${arr[*]}` does, one word of them all separated by spaces. */
function elements(values: readonly Expansion[], joined: boolean): Piece[][] {
  if (joined)
    return [values.flatMap((value, index) => (index === 0 ? value : [literal(" "), ...value]))];
  return values.length === 0 ? [[NO_ELEMENTS]] : values.map((value) => [...value]);
}

/** A parameter as an operator (`${D-x}`, `${1:-x}`) tests it: whether it is set, undefined when the
 * guard cannot tell (an argument of a shell whose arguments it does not know, `$!` before any
 * background job it saw), and its value when set. A special parameter comes from what the shell
 * holds (`$1` from its arguments, unset past the last), any other name from `lookup`. */
function operatorValue(
  name: string,
  st: State,
  ctx: Ctx
): { readonly set: boolean | undefined; readonly value: Expansion } {
  const unknownValue = [unknown(`\`$${name}\``)];
  if (!/^([0-9]+|[@*#?$!-])$/.test(name)) {
    const value = lookup(name, st, ctx);
    if (value === undefined) return { set: false, value: [] };
    return { set: value.some((piece) => piece.maybeUnset === true) ? undefined : true, value };
  }
  if (/^[1-9][0-9]*$/.test(name) || name === "@" || name === "*") {
    if (st.positional === undefined) return { set: undefined, value: unknownValue };
    const value =
      name === "@" || name === "*"
        ? st.positional.length === 0
          ? undefined
          : elements(st.positional, true)[0]
        : st.positional[Number(name) - 1];
    return value === undefined ? { set: false, value: [] } : { set: true, value };
  }
  if (name === "!" && !st.backgroundStarted) return { set: undefined, value: unknownValue };
  const alternatives = parameter(name, true, st, ctx);
  return { set: true, value: alternatives.length === 1 ? (alternatives[0] ?? []) : unknownValue };
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
      return elements([...array.values()], part.index === "*" && quoted);
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
  const { set, value } = operatorValue(name, st, ctx);
  const nonEmpty = set === true && value.map((p) => p.text).join("") !== "";
  const known = set === false || (set === true && value.every((p) => p.kind === "literal"));
  // A value holding known text or a pid this shell started is non-empty whatever else it holds.
  const surelyNonEmpty =
    set === true &&
    value.some((p) => p.descendantPid === true || (p.kind === "literal" && p.text !== ""));
  const undecided = [[unknown(`\`${part.text}\``)]];
  const operand = (): Piece[][] =>
    part.operand === undefined ? [[literal("")]] : expandWord(part.operand, st, ctx);
  // `:=` and `=` assign the operand they take (bash refuses it for a special parameter). When the
  // guard cannot tell whether they take it, the variable is set after, to a value it cannot know.
  const assigning = (result: Piece[][], maybe: boolean): Piece[][] => {
    if ((operator === ":=" || operator === "=") && IDENTIFIER.test(name)) {
      const only = result.length === 1 && !maybe ? result[0] : undefined;
      assignScalar(st, name, only ?? [unknown(`\`$${name}\`, which \`${part.text}\` may assign`)]);
    }
    return result;
  };
  switch (operator) {
    case ":-":
    case ":=":
      if (surelyNonEmpty) return parameter(name, quoted, st, ctx);
      if (!known) return assigning(undecided, true);
      return nonEmpty ? parameter(name, quoted, st, ctx) : assigning(operand(), false);
    case "-":
    case "=":
      // Only whether the parameter is set decides.
      if (set === undefined) return assigning(undecided, true);
      return set ? parameter(name, quoted, st, ctx) : assigning(operand(), false);
    case ":+":
      if (surelyNonEmpty) return operand();
      if (!known) return undecided;
      return nonEmpty ? operand() : [[literal("")]];
    case "+":
      if (set === undefined) return undecided;
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
  // An operand the guard cannot know makes the expansion unknown, for the operand's reason.
  const because = (pieces: Expansion): Piece[][] => {
    const cause = pieces.find((piece) => piece.kind === "unknown")?.why;
    return cause === undefined ? unknownResult : [[unknown(`\`${part.text}\` (${cause})`)]];
  };
  const operator = part.operator ?? "";
  const head = `\${${part.parameter}${operator}`;
  const accounted =
    part.replace === undefined
      ? part.text === `${head}${part.operand?.text ?? ""}}`
      : part.text === `${head}${part.replace.pattern.text}/${part.replace.replacement.text}}` ||
        (part.replace.replacement.text === "" &&
          part.text === `${head}${part.replace.pattern.text}}`);
  const slashPattern = operator === "/" || operator === "//";
  // A parsed empty pattern after `/` or `//` is `unbash` splitting a `/` bash reads as the
  // pattern's (`${v////x}`). The empty-glob check below refuses it too, so removing either leaves
  // the other the one defence of that split, and `pane-guard-bash.test.ts` fails only with both gone.
  if (!accounted || (slashPattern && part.replace?.pattern.text === "")) return unknownResult;
  // Unset expands empty: an argument past the last one this shell knows it was given, or a name it
  // unset. A name merely absent from the command may still be in the environment bash runs with.
  const { set, value: held } = operatorValue(part.parameter, st, ctx);
  const knownUnset =
    set === false &&
    (/^([1-9][0-9]*|[@*])$/.test(part.parameter) || isUnset(st.vars.get(part.parameter)));
  const value = set === true ? literalText(held) : knownUnset ? "" : undefined;
  if (value === undefined) return because(held);
  if (value.length > MAX_PATTERN_VALUE || !isAscii(value)) return unknownResult;
  const patternWord = part.replace?.pattern ?? part.operand;
  const patterns = patternWord === undefined ? [[literal("")]] : expandWord(patternWord, st, ctx);
  const glob = patterns.length === 1 ? patternGlob(patterns[0] as Expansion) : undefined;
  // Also the second defence of the split above: an empty pattern text gives an empty glob.
  if (glob === undefined || (slashPattern && glob.length === 0)) return unknownResult;
  // Past the budget the value is not computed, and the walk refuses the command.
  if (!spendPatternWork(globWork(glob, value.length, operator), ctx)) return unknownResult;
  let result: string;
  if (part.replace === undefined) {
    result = removePattern(value, operator, glob);
  } else {
    const replacements = expandWord(part.replace.replacement, st, ctx);
    const pieces = replacements.length === 1 ? (replacements[0] as Expansion) : undefined;
    if (pieces === undefined) return unknownResult;
    if (pieces.some((piece) => piece.kind === "unknown")) return because(pieces);
    const replacement = pieces.map((piece) => piece.text).join("");
    if (/[&\\]/.test(replacement)) return unknownResult;
    // `//` replaces at most one match per character; the others at most one. The bound stops the
    // output being built at all: unquoted, it is scanned for splitting before any word joins it.
    const most =
      value.length + (operator === "//" ? Math.max(value.length, 1) : 1) * replacement.length;
    if (most > MAX_VALUE_LENGTH) return [[overLong(`\`${part.text}\``)]];
    result = replacePattern(value, operator, glob, replacement);
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
      const long =
        a.length + b.length > MAX_VALUE_PIECES || textLength(a) + textLength(b) > MAX_VALUE_LENGTH;
      out.push(long ? [overLong("a word")] : [...a, ...b]);
    }
  }
  return out;
}

/** The longest value the guard builds, in characters and in pieces. A path is at most 4,096 bytes
 * on Linux, so a longer value is never one target it resolves, and building values without a bound
 * held the pane: a replacement of a replacement (`${v//?/$w}`) or `x="$x$x"` repeated in one line
 * reached tens of millions of characters in a fraction of a second, and judging each read of one
 * took seconds. The piece bound is its own: `z=""; z="$z$z"` repeated doubles empty pieces the
 * length bound never sees. The bound is kept by `product` for every word join, plus a bound on
 * each producer that can outgrow its inputs before a join: `patternExpansion`'s replacement, and
 * `printf -v`, which assigns its rendered text without one. The join bound alone is not enough: an
 * unquoted read (`$y`, `q=$y`, `case $y in`) scans the whole value for splitting and globbing
 * (`parameter`, `unquotedLiteral`) before any join exists, so no value a variable holds may be
 * longer either. Past the bound a value is unknown, and a refusal says why (`overLong`). */
const MAX_VALUE_LENGTH = 65_536;
const MAX_VALUE_PIECES = 4_096;

function overLong(what: string): Piece {
  return unknown(
    `${what}, longer than the guard builds (${MAX_VALUE_LENGTH} characters or ${MAX_VALUE_PIECES} pieces)`
  );
}

function textLength(pieces: readonly Piece[]): number {
  let length = 0;
  for (const piece of pieces) length += piece.text.length;
  return length;
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
      // `"$@"` or `"${arr[@]}"` over no elements, and nothing else in the word, is no argument.
      if (exp.length > 0 && exp.every((piece) => piece.noElements === true)) {
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

/** What the guard can read of a word: its leading literal text, and whether that is all of it.
 * `-C` reads whole, `root="$1"` reads as `root=` and not whole, `"$flag"` as nothing and not
 * whole. The leading text is the whole leading RUN of literal pieces, not the first piece alone:
 * a quote boundary splits `-"C$dir"` into a literal `-` and the quoted part, so reading only the
 * first piece stopped at `-` and hid the `-C` that word carries. A word it cannot read whole
 * still constrains what the word can be, and that prefix is the difference between refusing
 * `fuser "$flag" f` and refusing every `local root="$1"`. */
function readableWord(arg: Arg): { text: string; whole: boolean } {
  const whole = literalText(arg.exp);
  if (whole !== undefined) return { text: whole, whole: true };
  let text = "";
  for (const piece of arg.exp) {
    if (piece.kind !== "literal") break;
    text += piece.text;
  }
  return { text, whole: false };
}

/** Whether a word may be an option `matches` accepts. A word the guard reads whole answers the
 * test; one it does not may be any option its readable prefix allows, so `"$flag"`, `-a"$rest"`
 * and `--"$name"` may each be an option while `root="$1"` and `--socket="$s"` may not. Taking a
 * word it cannot read as no option at all is the one reading it may never assume: that is what
 * let `f=$(cat mode); fuser "$f" "$HOME"` through while `fuser -k "$HOME"` was refused. Both
 * spellings count, since the options this guard tests for have long forms (`--recursive`,
 * `--directory`, `--extract`, `--destination`, `--kill`): reading only the short one left those
 * long spellings a way through. `declare -n` is the exception with no long form at all. */
function mayBeOption(arg: Arg, matches: (text: string) => boolean): boolean {
  const { text, whole } = readableWord(arg);
  if (whole) return matches(text);
  return text === "" || /^--?[a-zA-Z-]*$/.test(text);
}

/** The expansion of a word past its first `count` characters, which the guard has read: the value
 * an option carries inside its own word (`-C/tmp`, `--directory=/tmp`). Undefined when the word is
 * shorter than that, which means it carries no value and the next word is the option's. */
function valueInWord(arg: Arg, count: number): Expansion | undefined {
  const pieces: Piece[] = [];
  let dropped = 0;
  for (const piece of arg.exp) {
    if (dropped >= count) {
      pieces.push(piece);
      continue;
    }
    if (piece.kind !== "literal") return undefined;
    if (piece.text.length + dropped <= count) {
      dropped += piece.text.length;
      continue;
    }
    pieces.push({ ...piece, text: piece.text.slice(count - dropped) });
    dropped = count;
  }
  return dropped < count || pieces.length === 0 ? undefined : pieces;
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
        walkSubstitution(part.script, part.inner, part.text, st, ctx);
        break;
      case "ProcessSubstitution":
        // `<(…)` and `>(…)` run beside this shell, which does not wait for either, so what one
        // writes may not be there when the next command reads it.
        uncertainly(ctx, () => walkSubstitution(part.script, part.inner, part.text, st, ctx));
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

const ARITHMETIC_ASSIGNMENTS = new Set([
  "=",
  "+=",
  "-=",
  "*=",
  "/=",
  "%=",
  "<<=",
  ">>=",
  "&=",
  "^=",
  "|=",
]);

/** Walks an arithmetic expression, which assigns: `d = 0`, `d += 1`, `d++` and `--d` leave `d` a
 * number the guard does not compute, and a name written with an expansion (`$n = 1`) may be any
 * variable. */
function visitArithmetic(expression: ArithmeticExpression | undefined, st: State, ctx: Ctx): void {
  if (expression === undefined) return;
  switch (expression.type) {
    case "ArithmeticBinary":
      visitArithmetic(expression.left, st, ctx);
      visitArithmetic(expression.right, st, ctx);
      if (ARITHMETIC_ASSIGNMENTS.has(expression.operator)) assignArithmetic(expression.left, st);
      break;
    case "ArithmeticUnary":
      visitArithmetic(expression.operand, st, ctx);
      if (expression.operator === "++" || expression.operator === "--") {
        assignArithmetic(expression.operand, st);
      }
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

function assignArithmetic(target: ArithmeticExpression, st: State): void {
  if (target.type !== "ArithmeticWord") return;
  const name = /[$`]/.test(target.value) ? undefined : target.value;
  const number = [unknown(`\`$${target.value}\`, an arithmetic result`)];
  assignByName(st, name, number, "an arithmetic expression");
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
    locals: st.locals === undefined ? undefined : new Map(st.locals),
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
 * definition they leave (`Callee`), and a handler any of them sets stays. A variable some branch
 * may leave unset is also unknown in whether it is set (`maybeUnset`), and a name a function makes
 * local on some branches only is local on some paths (`locals`). A variable every branch leaves
 * empty or a pid this shell started (`$!` in a retry loop, empty had it not run) stays a pid
 * that is safe to signal. Not merged: `files` and `pidFiles`, which every branch shares; `output`,
 * which each caller compares itself (`outputChanged`); and `runningFunctions`, which only a
 * function's own walk changes.
 *
 * A map every branch shares may only be moved towards unknown inside one. A write recording what
 * a file holds carries that text out of the branch it sits in, into the one the shell took, so a
 * write the walk reaches inside a body the shell may never enter, or may not have finished,
 * records only that the file is unknown (`modelWrite`, `uncertainly`).
 *
 * That covers a body the walk ENTERS. It does not cover a body the shell LEAVES early: the guard
 * models no `return`, `exit`, `break` or `continue`, so a statement after one of them is still on
 * what the walk calls the straight-line path, and a write there is still recorded as text. That
 * is the documented residual of this rule, and closing it needs a mechanism of its own rather
 * than this one. */
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
      const maybeUnset = branches.some((branch) => {
        const value = branch.vars.get(name);
        return value === undefined || isUnset(value) || value.some((piece) => piece.maybeUnset);
      });
      const rewrites = branches.some((branch) => attributed(branch, name));
      // Name what a branch did to it, so a refusal says why the value is unknown.
      const cause = branches
        .flatMap((branch) => branch.vars.get(name) ?? [])
        .find((piece) => piece.kind === "unknown")?.why;
      const merged = unknown(
        name === ANY_NAME
          ? `${cause ?? "a write the guard cannot read may have changed"}, in a branch or loop body`
          : `\`$${name}\`, which a branch sets differently${cause === undefined ? "" : ` (${cause})`}`,
        signalSafe ? true : undefined
      );
      target.vars.set(name, [
        {
          ...merged,
          ...(maybeUnset ? { maybeUnset: true as const } : {}),
          ...(rewrites ? { rewrites: true as const } : {}),
        },
      ]);
    }
  }
  if (target.locals !== undefined) {
    const locals = new Map<string, boolean>();
    for (const branch of branches) {
      for (const name of branch.locals?.keys() ?? []) {
        locals.set(
          name,
          branches.every((other) => other.locals?.get(name) === true)
        );
      }
    }
    target.locals = locals;
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
  // them. A removal inside a branch that may not run resets nothing here. The branch may never
  // have run, so the handler may never have been set: `uncertainSet` keeps `runSetTraps` from
  // reading what even an `EXIT` one writes as fact.
  //
  // The mirror of that: a handler this shell did set, which a branch may have removed, is kept
  // for the path where the branch did not run — and may not run on the path where it did, so it
  // is uncertain from here on for the same reason. One branch holding it is not enough; the
  // handler stands as fact only where every branch still holds it.
  //
  // One lift per branch that holds the handler, not one per handler: each carries the variables
  // THAT branch left (`handlerVars`), and with a branch per `&&` prefix those differ as soon as a
  // later prefix reassigns a name the handler reads when it runs. Skipping the second lift as a
  // duplicate drops the capture that names what the handler would delete. The cost is one walk of
  // the body per branch holding it, linear in the prefixes.
  const lifted: Trap[] = [];
  let merged: ReadonlyMap<string, Expansion> | undefined;
  for (const branch of branches) {
    for (const trap of branch.traps) {
      if (target.traps.includes(trap)) continue;
      merged ??= new Map(target.vars);
      lifted.push({ ...trap, vars: handlerVars(trap, branch.vars), merged, uncertainSet: true });
    }
  }
  target.traps = [
    ...target.traps.map((trap) =>
      branches.every((branch) => branch.traps.includes(trap))
        ? trap
        : { ...trap, uncertainSet: true }
    ),
    ...lifted,
  ];
}

/** Walks a body the shell may never enter — a branch, a loop body, a handler for a signal that
 * may never arrive, one of several definitions a name may hold — or may not have finished when a
 * later command reads what it wrote: a background command, a coprocess, a part of a pipeline.
 * Every file modelled inside is unknown afterwards (`modelWrite`).
 *
 * Whether the shell takes such a branch is a fact of the run — whether a file exists, what a
 * `grep` finds, what `uname` prints — so no rule here can decide it, and the model may not assume
 * either answer. */
function uncertainly(ctx: Ctx, walk: () => void): void {
  ctx.uncertain.depth += 1;
  try {
    walk();
  } finally {
    ctx.uncertain.depth -= 1;
  }
}

/** Records the text a command writes to `file`, which a later command of the same command may run
 * (`runFile`). Inside a body the shell may not have performed (`uncertainly`) the file is unknown
 * instead: `files` is one map the whole walk shares, and a stale model is worse than none, since
 * `runFile` prefers the model to what is on disk. A file that is unknown is refused, which is why
 * a conditional write followed by running the file is refused whether or not the branch runs. */
function modelWrite(st: State, ctx: Ctx, file: string, content: FileContents): void {
  (st.files as Map<string, FileContents>).set(
    file,
    ctx.uncertain.depth > 0 ? UNCERTAIN_WRITE : content
  );
}

/** The reason a write inside a body the shell may not have performed leaves behind, and the two
 * a rendered write leaves when the guard cannot read what it produced. */
const UNCERTAIN_WRITE: UnknownContents = {
  why:
    "the write is inside a branch, a loop body, a handler, or a command this shell does not wait " +
    "for, so the guard cannot know the shell performed it",
};
const UNRENDERED_WRITE: UnknownContents = {
  why: "the command writes output the guard cannot render",
};
const OVERLONG_WRITE: UnknownContents = {
  why: `the command writes more than the ${MAX_SCRIPT_BYTES} bytes the guard reads of a script`,
};
const UNREAD_FILE: UnknownContents = {
  why: "the command appends to a file it has not written, whose contents on disk the guard never reads",
};

/** What `file` holds as a pid after a command wrote it. The command's write always forgets what
 * the file held, since it is no longer there.
 *
 * A write the shell certainly performed then records the pid, when what the command wrote is
 * one. Inside a body the shell may not have performed, the file holds either what that body
 * wrote or what it held before, so it records a pid whose number is unknown exactly when both
 * are pids this shell started — `merge`'s own rule for a variable a branch sets to another of
 * them — and nothing at all otherwise, where an unknown pid is refused rather than signalled. */
function modelPidFile(st: State, ctx: Ctx, file: string, pid: Expansion | undefined): void {
  const model = st.pidFiles as Map<string, Expansion>;
  const before = model.get(file);
  model.delete(file);
  if (ctx.uncertain.depth === 0) {
    if (pid !== undefined) model.set(file, pid);
  } else if (pid !== undefined && before !== undefined) {
    model.set(file, [unknown("a pid a branch or loop body wrote to this file", true)]);
  }
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
 * what the merge wrote for them (`handlerVars`), and the pids it expanded (`Trap.pids`) over it.
 * An `EXIT` handler this shell certainly set runs when the shell ends, so what it writes is
 * written before anything after the shell reads it. Everything else is uncertain: a handler for a
 * signal runs only if that signal arrives, and a handler set inside a body the shell may never
 * enter (`Trap.uncertainSet`) may never have been set at all, `EXIT` included. Both matter here
 * rather than only at the outermost shell, because a subshell and a script this command runs each
 * run their handlers mid-command, in front of a later read. */
function runSetTraps(st: State, ctx: Ctx): void {
  for (const trap of st.traps) {
    const vars = new Map([...handlerVars(trap, st.vars), ...(trap.pids ?? [])]);
    const run = () => {
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
    };
    if (trap.signals.includes("EXIT") && trap.uncertainSet !== true) run();
    else uncertainly(ctx, run);
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
  uncertainly(ctx, () => {
    walkList(node.clause.commands, body, ctx);
    walkList(node.body.commands, body, ctx);
  });
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
        patterns.length === 1 ? matchesPattern(patterns[0] as Expansion, value, ctx) : undefined;
      if (matched === undefined) return undefined;
      if (matched)
        return item.terminator === undefined || item.terminator === ";;" ? item : undefined;
    }
  }
  return undefined;
}

/** Whether a `case` pattern matches `value` (`patternGlob`); undefined when the guard cannot
 * match it exactly, a value outside ASCII included, or when the match would spend the walk's
 * budget. */
function matchesPattern(pattern: Expansion, value: string, ctx: Ctx): boolean | undefined {
  const glob = patternGlob(pattern);
  if (glob === undefined || !isAscii(value)) return undefined;
  if (!spendPatternWork(globWork(glob, value.length, ""), ctx)) return undefined;
  return globMatches(glob, value);
}

/** A bash pattern as a `Glob`: literal ASCII text exactly, unquoted `*` and `?` as wildcards.
 * Undefined for what the guard cannot match exactly: an unknown part, a bracket expression, text
 * that may be an extended pattern (`@(a|b)`), and text outside ASCII. */
function patternGlob(pattern: Expansion): Glob | undefined {
  const glob: number[] = [];
  for (const piece of pattern) {
    if (piece.kind === "unknown" || !isAscii(piece.text)) return undefined;
    if (piece.kind === "glob") {
      if (piece.text === "*") glob.push(ANY_RUN);
      else if (piece.text === "?") glob.push(ANY_ONE);
      else return undefined;
    } else if (/[()|]/.test(piece.text)) return undefined;
    else for (let i = 0; i < piece.text.length; i += 1) glob.push(piece.text.charCodeAt(i));
  }
  return glob;
}

/** Charges `work` (`globWork`) to the walk's budget; false when that spends it, and then the walk
 * refuses the command. */
function spendPatternWork(work: number, ctx: Ctx): boolean {
  ctx.steps.count += Math.ceil(work / PATTERN_WORK_PER_STEP);
  return ctx.steps.count <= MAX_WALK_STEPS;
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
    throw new Refusal(site.snippet, site.line, WALK_LIMIT);
  }
  switch (node.type) {
    case "Statement": {
      checkRedirects(node.redirects, undefined, siteOf(node, st), st, ctx);
      if (node.background) {
        // `command &` runs in a subshell: nothing it changes (a handler, a variable, the working
        // directory, the arguments) reaches this shell, and the handlers it sets run at its end.
        // This shell does not wait for it, so what it writes may not be there when the next
        // command reads it.
        const before = st.output;
        const child = subshell(st);
        uncertainly(ctx, () => {
          walkNode(node.command, child, ctx, pipeIn);
          runSetTraps(child, ctx);
        });
        if (outputChanged(before, child.output)) st.output = undefined;
        st.backgroundStarted = true;
        return;
      }
      if (node.command.type === "BraceGroup") {
        const written = groupOutputFile(node, st, ctx);
        if (written !== undefined) {
          modelWrite(
            st,
            ctx,
            written,
            walkRenderingGroup(node.command, st, ctx) ?? UNRENDERED_WRITE
          );
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
      // Bash starts every part at once, so a part's write may not be there when a later part
      // reads it, and the guard walks the parts in order over one shared model.
      const before = st.output;
      const parts: State[] = [];
      uncertainly(ctx, () => {
        for (const [index, command] of node.commands.entries()) {
          const part = subshell(st);
          walkNode(command, part, ctx, index > 0);
          runSetTraps(part, ctx);
          parts.push(part);
        }
      });
      if (parts.some((part) => outputChanged(before, part.output))) st.output = undefined;
      return;
    }
    case "AndOr": {
      // `a && b`, `a || b`: the first command runs, and whether each one after it runs is the
      // exit status of the one before, a fact of the run. So the shell may stop after any of
      // them, and each prefix is a branch of its own: what `b` left stands in this shell only
      // where every branch agrees, as an `if` body's does.
      const [first, ...rest] = node.commands;
      if (first !== undefined) walkNode(first, st, ctx, pipeIn);
      if (rest.length === 0) return;
      const before = st.output;
      const branches: State[] = [clone(st)];
      const running = clone(st);
      uncertainly(ctx, () => {
        for (const command of rest) {
          walkNode(command, running, ctx, pipeIn);
          branches.push(clone(running));
        }
      });
      merge(st, branches);
      if (branches.some((branch) => outputChanged(before, branch.output))) st.output = undefined;
      return;
    }
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
      const otherwise = clone(st);
      uncertainly(ctx, () => {
        walkList(node.then.commands, then, ctx);
        if (node.else !== undefined) walkNode(node.else, otherwise, ctx, pipeIn);
      });
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
          assignScalar(body, name, [literal(value as string)]);
          uncertainly(ctx, () => walkList(node.body.commands, body, ctx));
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
        assignScalar(
          body,
          name,
          signalSafe
            ? [unknown(`\`$${name}\`, a loop variable`, true)]
            : [unknown(`\`$${name}\`, a loop variable`)]
        );
        if (node.type === "Select")
          assignScalar(body, "REPLY", [unknown("`$REPLY`, read from input")]);
        uncertainly(ctx, () => walkList(node.body.commands, body, ctx));
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
      uncertainly(ctx, () => walkList(node.body.commands, body, ctx));
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
        uncertainly(ctx, () => walkList(item.body.commands, body, ctx));
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
      // A coprocess runs beside this shell, which does not wait for it.
      const child = subshell(st);
      uncertainly(ctx, () => {
        walkNode(node.body, child, ctx, false);
        runSetTraps(child, ctx);
      });
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
        if (heredoc !== undefined) content = heredocText(heredoc);
        else if (herestring !== undefined) content = herestringText(herestring, st, ctx);
      } else if (command?.name === "echo") {
        content = echoText(command.args);
      } else if (command?.name === "printf") {
        content = printfText(command.args, MAX_SCRIPT_BYTES, true) ?? null;
      }
      // `echoText` and `printfText` answer null for a write the guard cannot render AND for one
      // that is merely longer than it reads, so both report `UNRENDERED_WRITE` when the honest
      // cause is `OVERLONG_WRITE`: `printf '%0999999d%0999999d' 1 1 > f` and a 1.1 MB literal
      // both say "output the guard cannot render". Measured, and left: telling them apart means
      // changing what `printfText` returns, and every such write is refused either way, so the
      // remedy the refusal names is right even where the cause it names is coarse. The
      // here-document path below does reach `OVERLONG_WRITE`, so that reason is not dead.
      // An append adds to the model of that file, and to nothing else: with no model the file is
      // whatever is on disk, which the guard has not read, so it is unknown and never empty.
      // Taking it as empty left one no-op append (`echo '' >> f`) making the guard read the file
      // as holding only what was appended, a destructive line already there running unseen
      // (LEGION-349). A model means "a path a rendered write in this command named, on a path the
      // shell certainly takes": `files` is shared across regions, so `false && echo ok > f`, and
      // the same inside `if`, `case`, `while`, `until` or a `for` whose list the guard cannot
      // decide, seeded one for code the shell never runs — those writes now record only that the
      // file is unknown (`modelWrite`), which an append onto it keeps unknown. Nothing here asks
      // the filesystem either: a path absent at check time is one an earlier stage of the same
      // command can fill (`cp evil.sh t; echo hi >> t; bash t`).
      const before = appends ? (st.files.get(file) ?? UNREAD_FILE) : "";
      modelWrite(
        st,
        ctx,
        file,
        content === null
          ? UNRENDERED_WRITE
          : typeof before !== "string"
            ? before
            : (readable(`${before}${content}`) ?? OVERLONG_WRITE)
      );
      const pidWritten =
        writes &&
        !appends &&
        command?.name === "echo" &&
        command.args.length === 1 &&
        command.args[0]?.exp.length > 0 &&
        command.args[0].exp.every((piece) => piece.descendantPid);
      modelPidFile(st, ctx, file, pidWritten ? command?.args[0]?.exp : undefined);
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
 * marker, or null once a statement prints something it cannot render or the text passes what the
 * guard reads (`readable`). Each statement's output is read before the statement is walked, so it
 * sees the variables the statements before it set. */
function walkRenderingGroup(group: BraceGroup, st: State, ctx: Ctx): string | null {
  let content: string | null = "";
  for (const statement of group.body.commands) {
    if (content !== null) {
      const text = statementOutput(statement, st, ctx);
      content = text === undefined ? null : readable(content + text);
    }
    walkNode(statement, st, ctx, false);
  }
  return content;
}

/** What `echo WORD...` prints to a file: its line, or null for one the guard cannot render. An
 * option first (`-n`, `-e` and their kin, which change what it prints) and a word the guard cannot
 * know (written as it is, into a script bash will parse) both leave it unrendered, and so does a
 * line longer than a script the guard reads: joining the words builds the whole string, so the
 * length is checked first. */
function echoText(list: readonly Arg[]): string | null {
  if (literalText(list[0]?.exp)?.startsWith("-")) return null;
  const words: string[] = [];
  let length = 0;
  for (const arg of list) {
    const word = literalText(arg.exp);
    if (word === undefined) return null;
    words.push(word);
    length += word.length + 1;
  }
  return length > MAX_SCRIPT_BYTES ? null : `${words.join(" ")}\n`;
}

/** The text of a file a command writes, as the guard models it, or null (a file it cannot read)
 * when it is longer than a script the guard reads from disk. Building it is cheap, since the text
 * is joined lazily; reading it is not: a 176 KB command rendering 655 MB held the pane for 30 s. */
function readable(content: string): string | null {
  return content.length > MAX_SCRIPT_BYTES ? null : content;
}

/** A here-document's text as the command reads it, or null when bash expands it into something
 * else: unquoted, `$` and a backquote expand, and a backslash before `$`, a backquote, a backslash
 * or a newline is removed, while the guard holds the text as written. */
function heredocText(heredoc: Redirect): string | null {
  const text = heredoc.content;
  if (text === undefined) return null;
  return !heredoc.heredocQuoted && /[$`]|\\[$`\\\n]/.test(text) ? null : text;
}

/** What one statement of a rendered group prints: nothing for an assignment, a here-document fed
 * to `cat` (`heredocText`), `printf`, or `echo` of values the guard knows. Undefined for anything
 * else. */
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
    return heredocText(heredoc) ?? undefined;
  }
  if (command.redirects.length > 0) return undefined;
  if (name === "printf") return printfText(rest, MAX_SCRIPT_BYTES, true);
  if (name === "echo") return echoText(rest) ?? undefined;
  return undefined;
}

/** What `printf FORMAT ARG...` prints, with each part the guard cannot know as the unknown marker:
 * `%s`, `%q` (quoted for a shell to read back), `%d`, `%%`, and the escapes `\n`, `\t` and `\\`,
 * the format reused while arguments remain. Undefined for a format it does not render: an unknown
 * one, `-v`, a width or precision, or another conversion. It stops once the text passes `limit`,
 * the most its caller keeps, so the text it returns is past the limit rather than built whole: a
 * format reused over 100,000 arguments would build and scan gigabytes.
 *
 * Rendering a `script` bash will parse, it is also undefined for a value it cannot know under `%s`
 * or `%q`, and for an argument that may be several (every value after it may belong to another
 * conversion), since the marker standing in would read as one harmless word. `%s` writes the value
 * as it is, and no position confines it, a comment included (a newline ends one). `%q`'s output
 * form is not fixed. A value containing a newline selects the `$'…'` form, whose output carries the
 * value's own quote characters raw, and that closes the format's quote in both the single- and
 * double-quoted positions; the guard does not follow which position a conversion sits in. `%d`
 * stays: its output is digits and a sign (`UNKNOWN_MARKER`). */
function printfText(list: readonly Arg[], limit: number, script = false): string | undefined {
  const [formatArg, ...values] = list;
  const format = literalText(formatArg?.exp);
  if (format === undefined || format.startsWith("-")) return undefined;
  if (script && values.some((value) => value.fields === "unknown")) return undefined;
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
      if (script && part.conversion !== "d" && value !== undefined) {
        if (literalText(value.exp) === undefined) return undefined;
      }
      const text = value === undefined ? "" : runtimeText(value.exp);
      if (part.conversion === "s") out += text;
      else if (part.conversion === "d")
        out += /^-?[0-9]+$/.test(text) ? text : value === undefined ? "0" : UNKNOWN_MARKER;
      else out += shellQuoted(text);
      if (out.length > limit) return out;
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
    const expanded = stored(
      assignment.value === undefined
        ? [literal("")]
        : (expandWord(assignment.value, st, ctx)[0] ?? [])
    );
    const arrayAssignment =
      assignment.array !== undefined || assignment.append || assignment.index !== undefined;
    const descendantPid = expanded.length > 0 && expanded.every((piece) => piece.descendantPid);
    const value = arrayAssignment
      ? [unknown(`\`$${assignment.name}\`, an array or appended value`, descendantPid || undefined)]
      : expanded;
    // A name whose attribute refuses or rewrites the write is unknown to the command it prefixes.
    overlay.set(
      assignment.name,
      attributed(st, assignment.name)
        ? [{ ...unknown(`\`$${assignment.name}\`, an attributed variable`), rewrites: true }]
        : value
    );
    // `a=1 b=$a` alone assigns left to right in this shell.
    if (command.name === undefined) {
      if (!arrayAssignment) {
        assignScalar(st, assignment.name, value);
      } else if (attributed(st, assignment.name)) {
        keepAttributed(st, assignment.name);
      } else {
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
          // The words bash gives the literal: none for `"$@"` of none, and one per element; a word
          // that may be several or none leaves every element unknown.
          const words = args(assignment.array, st, ctx);
          if (words.some((word) => word.fields === "unknown")) {
            elements.clear();
            elements.set(UNKNOWN_ARRAY_INDEX, [
              unknown(`\`$${assignment.name}\`, an array a word of which may be several elements`),
            ]);
          } else {
            for (const word of words) {
              if (word.fields === "none") continue;
              elements.set(String(index), stored(word.exp));
              index += 1;
            }
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
        st.vars.set(assignment.name, value);
      }
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

/** Strips the programs that run another program unchanged: `sudo rm` is `rm`.
 *
 * `alts` are the other argvs the same command line may run, for the one wrapper where words the
 * guard cannot read leave more than one program possible: `timeout` takes a duration after its
 * options, so each unreadable word there may be an option, leaving the program after the
 * duration, or the duration itself, leaving the program next. There is one reading per stop
 * position the scan passed, not two — with two unreadable words neither the stopped scan nor the
 * fully read one names the real program, which is how
 * `timeout "$opt" "$duration" rm -rf ~` went unchecked. */
function unwrap(
  argv: readonly Arg[],
  st: State,
  ctx: Ctx,
  site: Site
): { argv: readonly Arg[]; st: State; alts: readonly (readonly Arg[])[] } {
  const alts: (readonly Arg[])[] = [];
  let list = argv;
  let state = st;
  for (let guard = 0; guard < 16; guard += 1) {
    const name = literalText(list[0]?.exp);
    if (name === undefined) break;
    const base = path.basename(name);
    const rest = list.slice(1);
    // `positional` marks a wrapper whose options are followed by an argument of its own before
    // the program (`timeout <duration> <program>`). There a word the guard cannot read may be
    // that argument, and reading past it would take the program to be the word after the one
    // that is really the program, so the scan stops instead.
    const skip = (
      valued: string,
      longValued: readonly string[] = [],
      positional = false
    ): readonly Arg[] => {
      let i = 0;
      for (; i < rest.length; i += 1) {
        const arg = rest[i] as Arg;
        const text = literalText(arg.exp);
        if (text === "--") return rest.slice(i + 1);
        // A word the guard cannot read may be one of this wrapper's options, and stopping here
        // left the wrapper unstripped and the program it runs unchecked, even when that program
        // is written plainly right after it (`sudo "$flag" rm -rf ~`). Reading past it takes the
        // dangerous reading; the harmless one, where the unreadable word is itself the program,
        // is the residual an unreadable command name already is.
        if (text === undefined) {
          if (positional || !mayBeOption(arg, (option) => option.startsWith("-"))) break;
          continue;
        }
        if (!text.startsWith("-") || text === "-") break;
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
    // A multi-call binary's applet word stands where a program name stands, so a readable one is
    // promoted and checked as that program, and the loop starts again on it: `busybox busybox ls`
    // promotes twice. An unreadable one is left in place for `dispatch`, which knows the applet
    // may be any of the binary's own; promoting it would leave a command name the guard cannot
    // read, which it allows.
    if (
      (base === "busybox" || base === "toybox") &&
      rest[0] !== undefined &&
      literalText(rest[0].exp) !== undefined
    ) {
      list = rest;
      continue;
    }
    if (base === "sudo" || base === "doas") list = skip("ughpCDrtUT", ["--user", "--group"]);
    else if (base === "nice") list = skip("n", ["--adjustment"]);
    else if (["nohup", "setsid", "builtin", "time"].includes(base)) list = skip("");
    else if (base === "exec") list = skip("a");
    else if (base === "command") {
      const flags = rest.map((arg) => literalText(arg.exp));
      if (flags[0] === "-v" || flags[0] === "-V") return { argv: [], st: state, alts };
      list = skip("");
    } else if (base === "stdbuf") list = skip("ioe", ["--input", "--output", "--error"]);
    else if (base === "ionice") list = skip("cnt", ["--class", "--classdata"]);
    else if (base === "timeout") {
      // The duration reading: the scan stops at a word it cannot read, taking that word to be
      // the duration, so the program is the one after it.
      const stopped = skip("sk", ["--signal", "--kill-after"], true);
      list = stopped.slice(1);
      // Every unreadable word between the two stop points is a reading of its own: it may be the
      // duration, making the next word the program. With one such word the fully read scan
      // covers it; with two, neither that scan nor the stopped one names the real program.
      for (let at = rest.length - stopped.length; at < rest.length; at += 1) {
        if (literalText(rest[at]?.exp) !== undefined) break;
        alts.push(rest.slice(at + 1));
      }
      // The all-options reading: every word the scan passed was one of `timeout`'s options, so
      // the duration is the next readable word and the program the one after that. When the scan
      // did not stop early this agrees with `stopped` and there is nothing extra to dispatch.
      const read = skip("sk", ["--signal", "--kill-after"]);
      if (read.length !== stopped.length) alts.push(read.slice(1));
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
        } else if (text === undefined && mayBeOption(arg, (option) => option.startsWith("-"))) {
        } else break;
      }
      if (i !== -1) list = rest.slice(i);
      state = { ...clone(state), cwd, cwdWhy: "`env -C`" };
      for (const name of unset) {
        state.vars.set(name, UNSET);
        state.exported.delete(name);
      }
      for (const [name, value] of overlay) {
        state.vars.set(name, value);
        state.exported.add(name);
      }
    } else break;
  }
  return { argv: list, st: state, alts };
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
    locals: new Map(),
  };
  child.runningFunctions.add(name);
  try {
    walkNode(definition.body, child, ctx, false);
  } catch (error) {
    locateRefusal(error, definition.file);
  }
  returnLocals(name, child, outer);
  // A call runs in this shell, so everything the body leaves stays: the copy-back rule on `merge`
  // holds here too. Not carried: the positional parameters, which bash restores to the caller's
  // when a function returns; `locals`, the call's own frame, whose names `returnLocals` has
  // handed back; `files` and `pidFiles` (shared); and `runningFunctions`.
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

/** A returning function's locals are the caller's variables again: a name every path made local
 * gets back the value, array and export it had at the call; one only some paths made local may be
 * either, so it is unknown and may be unset unless both agree. */
function returnLocals(name: string, child: State, caller: State): void {
  for (const [local, everyPath] of child.locals ?? []) {
    const value = caller.vars.get(local);
    const elements = caller.arrays.get(local);
    if (everyPath) {
      if (value === undefined) child.vars.delete(local);
      else child.vars.set(local, value);
      if (elements === undefined) child.arrays.delete(local);
      else child.arrays.set(local, new Map(elements));
      if (caller.exported.has(local)) child.exported.add(local);
      else child.exported.delete(local);
      continue;
    }
    const same =
      JSON.stringify(child.vars.get(local) ?? null) === JSON.stringify(value ?? null) &&
      JSON.stringify([...(child.arrays.get(local) ?? [])]) ===
        JSON.stringify([...(elements ?? [])]);
    if (same) continue;
    const either: Expansion = [
      { ...unknown(`\`$${local}\`, local to \`${name}\` on some paths`), maybeUnset: true },
    ];
    child.vars.set(local, either);
    if (elements !== undefined || child.arrays.has(local)) {
      child.arrays.set(local, new Map([[UNKNOWN_ARRAY_INDEX, either]]));
    }
  }
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
    locals: new Map(),
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
  // Every other program the line may run, when the words the guard cannot read leave more than
  // one possible (`unwrap`'s `alts`). Each is walked for its refusal alone, in a state of its
  // own, since only one reading is what bash will do and none may be recorded as what happened.
  for (const other of unwrapped.alts) {
    dispatch({ ...invocation, args: other }, clone(outer), ctx);
  }
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
    // branch of its own: which one the name runs is the branch the shell took.
    const runs: State[] = [];
    uncertainly(ctx, () => {
      for (const definition of callee) {
        const run = clone(outer);
        if (definition !== undefined) runFunction(base, definition, rest, run, ctx);
        else {
          run.functions.delete(base);
          dispatch(invocation, run, ctx);
        }
        runs.push(run);
      }
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
      declaration(base, rest, outer, ctx, site);
      return;
    case "unset": {
      const found = operands(rest, "");
      const functionsOnly = found.options.some((option) => /^-[a-zA-Z]*f/.test(option));
      const variablesOnly = found.options.some((option) => /^-[a-zA-Z]*v/.test(option));
      for (const arg of found.operands) {
        const name = literalText(arg.exp);
        // `unset -f`, and a plain `unset` of a name no variable holds, remove a function, so the
        // name runs the command of that name again.
        if (!variablesOnly) {
          if (name === undefined) {
            for (const [fn, callee] of outer.functions) outer.functions.set(fn, orCommand(callee));
          } else {
            const callee = outer.functions.get(name);
            const held = functionsOnly ? undefined : lookup(name, outer, ctx);
            if (callee !== undefined && held === undefined) outer.functions.delete(name);
            else if (callee !== undefined && held?.some((piece) => piece.maybeUnset)) {
              outer.functions.set(name, orCommand(callee));
            }
          }
        }
        if (functionsOnly) continue;
        if (name === undefined || !IDENTIFIER.test(name)) {
          assignByName(outer, name, [], "`unset`");
          continue;
        }
        // Bash refuses to unset a `readonly` name, and unsets an `-i`, `-l` or `-u` one.
        if (attributed(outer, name)) {
          keepAttributed(outer, name, true);
          continue;
        }
        outer.vars.set(name, UNSET);
        outer.exported.delete(name);
        outer.arrays.delete(name);
      }
      return;
    }
    case "read":
    case "mapfile":
    case "readarray":
    case "getopts": {
      readInput(base, rest, outer);
      return;
    }
    case "wait": {
      // `wait -p NAME` assigns NAME the pid of the job it waited for, or nothing.
      for (const [index, arg] of rest.entries()) {
        if (!/^-[a-zA-Z]*p$/.test(literalText(arg.exp) ?? "")) continue;
        // With no job left to report it unsets NAME.
        const pid = [{ ...unknown("the pid `wait -p` assigns", true), maybeUnset: true as const }];
        assignByName(outer, literalText(rest[index + 1]?.exp), pid, "`wait -p`");
      }
      return;
    }
    case "let": {
      // Each argument is an arithmetic expression, assigning as `(( … ))` does.
      for (const arg of rest) {
        const text = literalText(arg.exp);
        const command =
          text === undefined ? undefined : parse(`(( ${text} ))`).commands[0]?.command;
        if (command?.type === "ArithmeticCommand") visitArithmetic(command.expression, outer, ctx);
        else forgetVariables(outer, "`let` in text the guard cannot read may have changed");
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
      const head = rest[0]?.exp ?? [];
      const leading = head[0];
      const spaced = literalText(head) === "-v";
      const joined =
        !spaced &&
        leading?.kind === "literal" &&
        leading.text.startsWith("-v") &&
        (leading.text.length > 2 || head.length > 1);
      // An option word the guard cannot read may be `-v`, and then the word after it names the
      // variable printf writes. Taking it as a format string instead records no write at all, so
      // a name the guard has already approved keeps its approved value while bash replaces it —
      // the one direction it may not take. Which variable is unknown as well when that word is
      // unreadable, and `assignByName` then forgets every name. With no word after it there is
      // no such reading at all: `printf -v` alone is a usage error that assigns nothing, so
      // forgetting every variable there was a refusal with nothing behind it.
      const first = rest[0];
      const mayBeV =
        !spaced &&
        !joined &&
        first !== undefined &&
        rest[1] !== undefined &&
        mayBeOption(first, (text) => text.startsWith("-v"));
      if (spaced || joined || mayBeV) {
        const variable =
          spaced || mayBeV
            ? literalText(rest[1]?.exp)
            : literalText([
                { ...(leading as Piece), text: leading?.text.slice(2) ?? "" },
                ...head.slice(1),
              ]);
        // `printf -v NAME '%s' VALUE` assigns VALUE as it is, a pid of this shell's included;
        // another format assigns what printf renders, known only when every part of it is.
        const printed = rest.slice(spaced || mayBeV ? 2 : 1);
        const [format, value, ...others] = printed;
        let assigned: Expansion;
        if (mayBeV) {
          assigned = [
            unknown(
              `\`$${variable ?? "?"}\` (printf with an option word the guard cannot read, which may be \`-v\`)`
            ),
          ];
        } else if (
          literalText(format?.exp) === "%s" &&
          value !== undefined &&
          others.length === 0
        ) {
          assigned = value.exp;
        } else {
          const text = printfText(printed, MAX_VALUE_LENGTH);
          // An argument the guard cannot know names why the value is unknown.
          const cause = printed.flatMap((arg) => arg.exp).find((p) => p.kind === "unknown")?.why;
          assigned =
            text === undefined || text.includes(UNKNOWN_MARKER)
              ? [
                  unknown(
                    `\`$${variable ?? "?"}\` (printf -v${cause === undefined ? "" : `: ${cause}`})`
                  ),
                ]
              : // `printfText`'s early stop bounds the cost; this check bounds the truth. Past the
                // limit it returns text it stopped building, which is never the value, and without
                // this check that truncated text would be assigned as if whole. While the limit is
                // this same bound, a partial would still be refused at the next join; if the limit
                // moves or the early stop goes, this check is again the only bound on what
                // `printf -v` stores, and an unquoted read scans a stored value whole.
                text.length > MAX_VALUE_LENGTH
                ? [overLong(`\`$${variable ?? "?"}\` (printf -v)`)]
                : [literal(text)];
        }
        assignByName(outer, variable, assigned, "`printf -v`");
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
      const text = rest.map((arg) => runtimeText(arg.exp)).join(" ");
      runText(text, "the `eval` text", outer, ctx, site);
      // What the guard could not read of the text may assign any variable.
      if (text.includes(UNKNOWN_MARKER)) {
        forgetVariables(outer, "`eval` of text the guard cannot read may have changed");
      }
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
        outer.traps = [
          ...outer.traps,
          {
            text: handlerText,
            site,
            signals: handled,
            file,
            pids,
            uncertainSet: ctx.uncertain.depth > 0,
          },
        ];
      } else if (text === undefined) {
        throw new Refusal(
          site.snippet,
          site.line,
          `the guard cannot read the handler this \`trap\` sets, \`${handler.text}\` (${handler.exp.find((piece) => piece.kind !== "literal")?.why ?? "a value the guard cannot know"}), so it cannot check what runs when this shell exits; write the handler out, or put it in a function`
        );
      } else if (!text.startsWith("-")) {
        outer.traps = [
          ...outer.traps,
          { text, site, signals: handled, file, uncertainSet: ctx.uncertain.depth > 0 },
        ];
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
          // Bare mode letters, first on the line, are the one extract mode that is no option
          // word, so a prefix of them the guard cannot read whole may still gain an `x`. Every
          // other extract mode is an option word, and a prefix that can be none of them rules it
          // out, so `tar cf "$archive" notes.txt` stays a create.
          const { text, whole } = readableWord(arg);
          if (!whole && index === 0 && /^[A-Za-z]*$/.test(text)) return true;
          return mayBeOption(
            arg,
            (mode) =>
              mode === "--extract" ||
              mode === "--get" ||
              (mode.startsWith("-") && !mode.startsWith("--") && mode.includes("x")) ||
              (index === 0 && /^[A-Za-z]*x[A-Za-z]*$/.test(mode))
          );
        });
      if (!extracting) return;
      const targetDirectories: Arg[] = [];
      const short = base === "tar" ? "-C" : "-d";
      const long = base === "tar" ? "--directory" : "--destination";
      // A short option may stand in a cluster (`tar -xC dir`), and wherever it stands its value
      // is whatever follows it in the same word (`-C/tmp`, `-xC"$dir"`, `--directory=/tmp`), or
      // the next word when nothing does. Judging the next word regardless passed a command whose
      // real destination was never looked at, and reported on a path it never touched.
      const letter = short.slice(1);
      for (const [index, arg] of rest.entries()) {
        const { text, whole } = readableWord(arg);
        const clusterAt =
          text.startsWith("-") && !text.startsWith("--") ? text.indexOf(letter, 1) : -1;
        const carried = text.startsWith(`${long}=`)
          ? valueInWord(arg, long.length + 1)
          : clusterAt !== -1 && (!whole || text.length > clusterAt + 1)
            ? valueInWord(arg, clusterAt + 1)
            : undefined;
        if (carried !== undefined) {
          targetDirectories.push({ text: arg.text, exp: carried });
          continue;
        }
        if (clusterAt !== -1 || mayBeOption(arg, (option) => option === short || option === long)) {
          const target = rest[index + 1];
          if (target !== undefined) targetDirectories.push(target);
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
          // `tee -a` leaves the file unknown whatever the model, which is stricter than the `>>`
          // rule in `checkRedirects` and stays so on purpose: routing it through that rule would
          // start reading a file `tee` appended to, and the guard does not render what `tee`
          // writes. So `tee -a` onto a file this command wrote, then run, is refused.
          const text = heredoc === undefined ? null : heredocText(heredoc);
          modelWrite(
            st,
            ctx,
            file,
            append
              ? UNREAD_FILE
              : text === null
                ? UNRENDERED_WRITE
                : (readable(text) ?? OVERLONG_WRITE)
          );
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
    case "fuser": {
      // A word the guard cannot read may be `-k`, so a command holding one is a kill unless
      // every reading left is harmless: `fuser -k` with no file to search kills nothing, so a
      // lone unreadable word — either the option or the file, not both — is not a kill, and
      // `fuser "$path"` still runs. That count is of the words bash will make, not of the words
      // written: an unquoted word bash may split into several (`fields`) can be both the option
      // and the file at once, which is how `a=$(cat f); fuser $a` reached a kill.
      // `--kill` is the long spelling, which the short-cluster pattern cannot match: real PSmisc
      // kills on either.
      const kills = rest.some((arg) =>
        mayBeOption(arg, (text) => /^-[a-zA-Z]*k/.test(text) || text === "--kill")
      );
      const readable = rest.every((arg) => literalText(arg.exp) !== undefined);
      const mayBeSeveral = rest.some((arg) => arg.fields === "unknown");
      if (kills && (readable || mayBeSeveral || rest.length > 1)) {
        throw new Refusal(
          site.snippet,
          site.line,
          "`fuser -k` kills every process using a file, other agents' included; a pane signals only " +
            "processes it started"
        );
      }
      return;
    }
    case "tmux": {
      let socket: Arg | undefined;
      let socketIsPath = false;
      // tmux takes one subcommand per segment, and a literal `;` argument starts another
      // segment: `tmux new-window \; kill-server` runs two commands on one line. The subcommand
      // of a segment is its first word that is neither a server option nor an option's value,
      // and every word after it belongs to that subcommand, which is what keeps
      // `tmux set-option -t "$session" …` out of the kill test while `tmux "$command"` stays in.
      // Scanning only the first segment let every command after a `;` past the rule.
      //
      // A word the guard cannot read may itself be that `;`, and a word bash may split into
      // several may hold one, so either starts a segment as well as joining the one before it:
      // `s=$(cat f); tmux -S "$sock" list-sessions "$s" kill-server` runs a kill when `$s` is
      // `;`. A segment that exists only under that reading is `speculative`, and there the
      // subcommand must be a word the guard can read something of: a speculative segment whose
      // subcommand is also unreadable is two unknowns deep, the same shape as a command whose
      // own name the guard cannot read, which it already allows. Refusing it instead refused
      // four ordinary `tmux new-session`/`send-keys` lines in this repository's own scripts.
      interface Segment {
        readonly words: Arg[];
        /** True when this segment exists only because a word may have been the `;`. */
        readonly speculative: boolean;
      }
      let current: Segment = { words: [], speculative: false };
      const segments: Segment[] = [current];
      for (const arg of rest) {
        const word = literalText(arg.exp);
        // Real tmux takes a `;` at the END of a word as the separator too, so `list-sessions;`
        // is the subcommand `list-sessions` and starts a new command after it. Testing only for
        // a word that is exactly `;` made `list-sessions;` the subcommand itself and swallowed
        // the kill standing after it. A trailing `;` is one the guard READ, so the segment it
        // starts is not speculative.
        const trailing = word !== ";" && word !== undefined && word.endsWith(";");
        if (trailing) current.words.push({ text: arg.text, exp: [literal(word.slice(0, -1))] });
        else if (word !== ";") current.words.push(arg);
        if (word === ";" || trailing) {
          current = { words: [], speculative: false };
          segments.push(current);
        } else if (word === undefined || arg.fields === "unknown") {
          current = { words: [], speculative: true };
          segments.push(current);
        }
      }
      const killCommands = ["kill-server", "kill-session", "kill-window", "kill-pane"];
      const respawnCommands = ["respawn-pane", "respawn-window"];
      // A subcommand the guard cannot read whole may be completed into any tmux abbreviates from
      // the prefix it did read, so `tmux "$command"` is taken as a kill and the resolved socket
      // still decides; a prefix no kill command extends rules it out.
      const names = (word: Arg | undefined, commands: readonly string[]): boolean => {
        if (word === undefined) return false;
        const { text, whole } = readableWord(word);
        const compact = text.replace("-", "");
        const reachable = commands.filter((command) =>
          command.replace("-", "").startsWith(compact)
        );
        return whole ? reachable.length === 1 : reachable.length > 0;
      };
      let killing: Arg | undefined;
      for (const segment of segments) {
        const words = segment.words;
        let subcommand: Arg | undefined;
        for (let i = 0; i < words.length; i += 1) {
          const arg = words[i] as Arg;
          const { text, whole } = readableWord(arg);
          const attached = (option: string): Expansion | undefined =>
            text.startsWith(option) && (!whole || text.length > option.length)
              ? valueInWord(arg, option.length)
              : undefined;
          const carried = attached("-L") ?? attached("-S") ?? attached("--socket=");
          if (carried !== undefined) {
            // The socket is named inside the option's own word (`-S"$sock"`), which is the
            // ordinary way to name a private server.
            socket = { text: arg.text, exp: carried };
            socketIsPath = !text.startsWith("-L");
          } else if (text === "-L" || text === "-S" || text === "--socket") {
            socket = words[i + 1];
            socketIsPath = text !== "-L";
            i += 1;
          } else if (text === "-f" || text === "-T") {
            i += 1;
          } else if (subcommand === undefined && !text.startsWith("-")) {
            // The test is on the prefix the guard read, not on the whole word: asking
            // `literalText` made an option word it could not read whole (`-L"$sock"`) the
            // segment's subcommand, and the kill standing after it was never tested. An empty
            // prefix does not start with `-`, so `tmux "$command"` is still taken as a kill.
            subcommand = arg;
          }
        }
        // A segment that exists only because a word may have been the `;` needs a subcommand the
        // guard can read something of; unreadable there, it is the two-unknowns case the guard
        // already allows for a command whose own name it cannot read.
        if (
          segment.speculative &&
          (subcommand === undefined || readableWord(subcommand).text === "")
        )
          continue;
        const respawning =
          names(subcommand, respawnCommands) &&
          words.some((arg) => mayBeOption(arg, (text) => text === "-k"));
        if (names(subcommand, killCommands) || respawning) killing ??= subcommand;
      }
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
        `\`tmux ${killing.text}\` needs a socket path inside this pane's roots; the ` +
          "resolved socket can end panes and processes this pane did not start"
      );
    }
    case "busybox":
    case "toybox": {
      // `unwrap` promoted the applet word if it could read it, so reaching here with one left
      // means it cannot. The applet may be any the binary carries, and no reading of an
      // unreadable one is harmless: the list holds `killall`, whose refusal does not depend on
      // its arguments, `rm` and `sh`, and `halt`, `poweroff` and `reboot`, which need no operand
      // at all. An earlier form let an applet word with nothing after it run, on the claim that
      // every applet is then inert; those three are why that claim was wrong.
      if (rest.length === 0) return;
      throw new Refusal(
        site.snippet,
        site.line,
        `the guard cannot read the applet \`${(rest[0] as Arg).text}\` ${base} would run, and the ` +
          "applets it carries include `rm`, `sh` and `killall`, which reach paths and processes " +
          `this pane did not start; name the applet (\`${base} rm …\`) so it can be checked`
      );
    }
    default:
      break;
  }
  if (SHELLS.has(base)) {
    runShell(rest, invocation, st, ctx);
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

/** `export`, `declare`, `typeset`, `local` and `readonly` over their arguments. A name is the
 * expanded argument's text before its first `=`, so `declare "d=x"` assigns `d`, and a name the
 * guard cannot read may be any variable (`assignByName`). `-n` makes a nameref, whose later writes
 * land in the variable it names, which the guard does not follow, so it refuses; `-f` and `-F`
 * name functions. Inside a function, `local`, and `declare` or `typeset` without `-g`, make a name
 * local to it (`locals`), unset until assigned; `local` outside one assigns nothing, as bash
 * refuses it. `readonly` and `-r` keep the value and refuse later writes, and `-i`, `-l` and `-u`
 * rewrite this value and later ones, so the name carries the attribute (`Piece.rewrites`). */
function declaration(builtin: string, list: readonly Arg[], st: State, ctx: Ctx, site: Site): void {
  const options = list
    .map((arg) => literalText(arg.exp) ?? "")
    .filter((text) => /^[-+][a-zA-Z]+$/.test(text));
  const option = (letter: string) =>
    options.some((text) => text.startsWith("-") && text.includes(letter));
  // An option word the guard cannot read may be `-n`, and a nameref it did not see would send
  // every later write to a variable it is not watching. A word whose readable prefix is no option
  // (`root="$1"`) is not one however unreadable its value. `export` is exempt because bash's
  // `export -n` only drops the export attribute.
  const mayBeNameref = list.some((arg) => mayBeOption(arg, (text) => /^-[a-zA-Z]*n/.test(text)));
  if (builtin !== "export" && mayBeNameref) {
    throw new Refusal(
      site.snippet,
      site.line,
      option("n")
        ? "the guard does not follow a nameref (`declare -n`), whose writes land in the variable it names"
        : `an option word \`${builtin}\` carries that the guard cannot read may be \`-n\`, a nameref ` +
            "whose writes land in the variable it names and which the guard does not follow; write " +
            "the option as a literal"
    );
  }
  if (option("f") || option("F")) return;
  if (builtin === "local" && st.locals === undefined) return;
  const array = option("a") || option("A");
  const exports = builtin === "export" || option("x");
  const keeps = builtin === "readonly" || option("r");
  const rewrites = option("i") || option("l") || option("u");
  const locals =
    builtin === "local" || ((builtin === "declare" || builtin === "typeset") && !option("g"))
      ? st.locals
      : undefined;
  const writer = `\`${builtin}\``;
  for (const arg of list) {
    const text = literalText(arg.exp);
    if (text !== undefined && /^[-+][a-zA-Z]+$/.test(text)) continue;
    const { name, value: assigned } = nameAndValue(arg);
    if (name === undefined) {
      forgetVariables(st, `${writer} under a name the guard cannot read may have changed`);
      continue;
    }
    const base = ARRAY_ELEMENT.exec(name)?.[1] ?? name;
    if (!IDENTIFIER.test(base)) continue;
    if (exports) st.exported.add(base);
    // Bash refuses the write, or rewrites the value it takes; `export` or `readonly` of an
    // attributed name alone changes nothing.
    if (rewrites || (attributed(st, base) && (assigned !== undefined || locals !== undefined))) {
      keepAttributed(st, base);
      continue;
    }
    if (attributed(st, base)) continue;
    if (locals !== undefined) {
      locals.set(base, true);
      if (assigned === undefined) {
        st.vars.set(base, UNSET);
        st.arrays.delete(base);
      }
    }
    if (assigned === undefined) {
      if (keeps) markKept(st, base, ctx);
      continue;
    }
    // Tilde expands after an assignment's `=` in a declaration's argument too.
    const value = arg.text.startsWith(`${name}=~`)
      ? [...unquotedLiteral(arg.text.slice(name.length + 1), 0, st, ctx), ...arg.exp.slice(1)]
      : assigned;
    if (!array || name !== base) {
      assignByName(st, name, value, writer);
    } else {
      st.vars.set(name, [unknown(`\`$${name}\`, an array`)]);
      if (arg.text === `${name}=()`) {
        st.arrays.set(name, new Map());
      } else {
        const unknownArray = [unknown(`\`$${name}\`, an array assignment`)];
        st.arrays.set(name, new Map([[UNKNOWN_ARRAY_INDEX, unknownArray]]));
      }
    }
    if (keeps) markKept(st, base, ctx);
  }
}

/** `readonly NAME`: the value NAME holds now, from the pane's environment or unset included,
 * stays, and a later write is refused (`Piece.rewrites`). */
function markKept(st: State, name: string, ctx: Ctx): void {
  const held = st.vars.get(name) ?? lookup(name, st, ctx) ?? UNSET;
  const value = held.length === 0 ? [literal("")] : held;
  st.vars.set(
    name,
    value.map((piece) => ({ ...piece, rewrites: true }))
  );
}

/** `read`, `mapfile`/`readarray` and `getopts`, which assign what they read, unknown to the guard:
 * `read` its names (`REPLY` when it has none) or the array `-a` names, `mapfile` its array
 * (`MAPFILE` when it has none), and `getopts` its name, `OPTARG` and `OPTIND`. An option that takes
 * a value takes it spelled apart or joined (`read -ra arr`, `read -rarr`). */
function readInput(builtin: string, rest: readonly Arg[], st: State): void {
  const valued = builtin === "read" ? "adinNptu" : builtin === "getopts" ? "" : "dnOsuCc";
  const writer = `\`${builtin}\``;
  const input = (name: string | undefined): Expansion => [
    unknown(`\`$${name ?? "?"}\`, read from input`),
  ];
  let array: Arg | undefined;
  let arrayOption = false;
  const names: Arg[] = [];
  let options = builtin !== "getopts";
  for (let i = 0; i < rest.length; i += 1) {
    const arg = rest[i] as Arg;
    const text = literalText(arg.exp);
    if (options && text === "--") {
      options = false;
      continue;
    }
    if (options && text !== undefined && /^-./.test(text)) {
      for (let j = 1; j < text.length; j += 1) {
        const flag = text.charAt(j);
        if (!valued.includes(flag)) continue;
        const attached = text.slice(j + 1);
        i += attached === "" ? 1 : 0;
        const value = attached === "" ? rest[i] : { text: attached, exp: [literal(attached)] };
        if (flag === "a" && builtin === "read") {
          arrayOption = true;
          array = value;
        }
        break;
      }
      continue;
    }
    options = false;
    names.push(arg);
  }
  const fill = (arg: Arg | undefined, fallback: string): void => {
    const name = arg === undefined ? fallback : literalText(arg.exp);
    if (name === undefined || !IDENTIFIER.test(name) || attributed(st, name)) {
      assignByName(st, name, input(name), writer);
      return;
    }
    st.vars.set(name, input(name));
    st.arrays.set(name, new Map([[UNKNOWN_ARRAY_INDEX, input(name)]]));
  };
  if (builtin === "getopts") {
    const name = names[1];
    if (name !== undefined) assignByName(st, literalText(name.exp), input(name.text), writer);
    for (const variable of ["OPTARG", "OPTIND"]) assignScalar(st, variable, input(variable));
  } else if (builtin !== "read") {
    fill(names[0], "MAPFILE");
  } else if (arrayOption) {
    if (array !== undefined) fill(array, "");
  } else if (names.length === 0) {
    assignScalar(st, "REPLY", input("REPLY"));
  } else {
    for (const name of names) assignByName(st, literalText(name.exp), input(name.text), writer);
  }
}

/** A function's callee after an `unset` that may have removed it: its name may run the command
 * of that name too. */
function orCommand(callee: Callee): Callee {
  return callee.includes(undefined) ? callee : [...callee, undefined];
}

/** A declaration argument's name, its expanded text before the first `=` (undefined when the
 * guard cannot read it), and the value after that `=` (undefined when it has none). */
function nameAndValue(arg: Arg): { name: string | undefined; value: Expansion | undefined } {
  let name = "";
  for (const [index, piece] of arg.exp.entries()) {
    if (piece.kind !== "literal") return { name: undefined, value: undefined };
    const equals = piece.text.indexOf("=");
    if (equals === -1) {
      name += piece.text;
      continue;
    }
    const rest = piece.text.slice(equals + 1);
    const value = [...(rest === "" ? [] : [{ ...piece, text: rest }]), ...arg.exp.slice(index + 1)];
    return { name: name + piece.text.slice(0, equals), value };
  }
  return { name, value: undefined };
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
  const verb =
    program === "chmod" ? "recursively change the mode of" : "recursively change the owner of";
  if (recursive) {
    checkTargets(`${program} -R`, verb, reference ? found : found.slice(1), true, st, ctx, site);
    return;
  }
  // No option the guard read is `-R`, but an operand it could not read whole may be one. Under
  // that reading the word is the option rather than a path, so the paths this command would
  // change recursively are the other operands: `chmod +x "$out"` has none and runs, while
  // `chmod "$mode" 700 ~` would reach the home directory. A word bash may split into several
  // (`fields`) is not one or the other: it can supply the option AND leave every other operand
  // standing, which is how `a=$(cat f); chmod $a "$HOME"` reached the home directory.
  for (const [index, arg] of found.entries()) {
    if (!mayBeOption(arg, (text) => /^-[A-Za-z]*R/.test(text))) continue;
    // A word bash may split into several supplies the option AND leaves every other operand
    // standing, and may carry paths of its own inside it, so nothing is taken off the list.
    const others = arg.fields === "unknown" ? found : found.filter((_, at) => at !== index);
    const targets = reference || arg.fields === "unknown" ? others : others.slice(1);
    if (targets.length === 0) continue;
    checkTargets(`${program} -R`, verb, targets, true, st, ctx, site);
  }
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
  // A root word the guard cannot read whole may be a predicate instead of a path. The two
  // readings are judged together, not worst-of-each: under the predicate reading the roots are
  // the ones before it, so `find "$dir" -type f` searches `.` and is allowed, while
  // `find ~ "$word"` may delete under the home directory and is not.
  let rootsBeforePredicate: Arg[] | undefined;
  for (; i < list.length; i += 1) {
    const arg = list[i] as Arg;
    const text = literalText(arg.exp);
    if (
      text !== undefined &&
      (text.startsWith("-") || text === "(" || text === "!" || text === ")")
    )
      break;
    if (
      rootsBeforePredicate === undefined &&
      mayBeOption(arg, (option) => option.startsWith("-"))
    ) {
      rootsBeforePredicate = [...starts];
    }
    starts.push(arg);
  }
  const targets = starts.length > 0 ? starts : [{ text: ".", exp: [literal(".")] }];
  const expression = list.slice(i).map((arg) => literalText(arg.exp));
  let action: string | undefined;
  let shell: readonly Arg[] | undefined;
  // A predicate that takes a value consumes the word after it, so that word is its value and no
  // predicate of its own: `-name "$pattern"` is not a `-delete` the guard failed to read.
  let consumedByPredicate = false;
  for (const [index, word] of expression.entries()) {
    const isValue = consumedByPredicate;
    consumedByPredicate = word !== undefined && FIND_VALUED_PREDICATES.has(word);
    // Any other word the guard cannot read may be `-delete`, so what the search matched is taken
    // as deleted and the roots still decide.
    if (word === undefined && !isValue) action ??= "find with a predicate the guard cannot read";
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
  if (action === undefined) {
    // No predicate the guard read deletes anything, but a root it could not read whole may be
    // one, and then the roots are those before it.
    if (rootsBeforePredicate === undefined) return;
    const under =
      rootsBeforePredicate.length > 0 ? rootsBeforePredicate : [{ text: ".", exp: [literal(".")] }];
    checkTargets(
      "find with a root the guard cannot read, which may be a predicate",
      "delete or change what it finds under",
      under,
      false,
      st,
      ctx,
      site
    );
    return;
  }
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
  // A name unset here is not in the child's environment either, the pane's included.
  for (const [name, value] of st.vars) if (isUnset(value)) vars.set(name, value);
  for (const [name, value] of overlay) vars.set(name, value);
  const exported = new Set(vars.keys());
  // After a write the guard could not read, any name may have been exported with any value.
  const any = st.vars.get(ANY_NAME);
  if (any !== undefined) vars.set(ANY_NAME, any);
  return {
    vars,
    exported,
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
    locals: undefined,
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
  if (heredoc !== undefined) {
    const text = heredocText(heredoc);
    if (text === null) {
      throw new Refusal(
        site.snippet,
        site.line,
        "the guard cannot read the script this shell reads from its here-document, which bash expands first; quote its delimiter (`<<'EOF'`) or write the script with the write tool"
      );
    }
    runText(text, "the script it reads from the heredoc", child, ctx, site);
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
    // A word the guard cannot read stops it naming what the interpreter runs, but the words after
    // it still may: `python3 "$flag" -c <code>` has its code checked. Reading the whole line is
    // why the walk carries on rather than giving up here.
    if (word === undefined) continue;
    if (codeFlags.includes(word)) {
      runCode(language, list[i + 1], invocation, st, ctx, site);
      return;
    }
    if (language === "py" && word === "-m") return;
    // `python3 - <<'PY'`: the program is standard input.
    if (word === "-") {
      const heredoc = invocation.redirects.find((r) => r.operator === "<<" || r.operator === "<<-");
      if (heredoc !== undefined) {
        const text = heredocText(heredoc);
        if (text === null) {
          throw new Refusal(
            site.snippet,
            site.line,
            "the guard cannot read the program this interpreter reads from its here-document, which bash expands first; quote its delimiter (`<<'PY'`) or write the program with the write tool"
          );
        }
        runCode(language, { text: "<heredoc>", exp: [literal(text)] }, invocation, st, ctx, site);
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
    if (written === undefined || typeof written !== "string") {
      throw new Refusal(
        site.snippet,
        site.line,
        `this command may write ${abs} in a way the guard cannot read before running it (${written?.why ?? "the guard cannot say what it holds"}); write the script with the write tool first`
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
        // A `local` in a file sourced inside a function belongs to that function's frame.
        st.locals = child.locals;
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
    if (isUnset(value)) delete env[name];
    else if (text !== undefined) env[name] = text;
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
    uncertain: { depth: 0 },
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
    locals: undefined,
  });
  const run = (ctx: Ctx, attempt: () => void, fallback: string): string | undefined => {
    try {
      attempt();
      // A value past the budget is left uncomputed, and the walk's next node refuses; a command
      // whose last expansion spent the budget has no next node, so it is refused here.
      return ctx.steps.count > MAX_WALK_STEPS
        ? `refused \`${fallback}\`: ${WALK_LIMIT}`
        : undefined;
    } catch (error) {
      if (!(error instanceof Refusal)) throw error;
      return `refused \`${error.snippet || fallback}\`: ${error.detail}`;
    }
  };
  return {
    bash: (command, cwd, env) => {
      const ctx = context(env);
      return run(ctx, () => walkScript(parse(command), initial(cwd, command), ctx), command.trim());
    },
    argv: (argv, cwd, env) => {
      const ctx = context(env);
      const list: Arg[] = argv.map((item) => ({ text: item, exp: [literal(item)] }));
      const snippet = argv.join(" ");
      return run(
        ctx,
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
        ctx,
        () => checkCode(language, code, initial(cwd, code), ctx, true),
        `${language} code`
      );
    },
  };
}
