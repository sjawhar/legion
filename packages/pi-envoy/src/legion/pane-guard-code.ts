/**
 * The part of the pane guard (LEGION-121, `pane-guard.ts`) that reads Python and JavaScript: the
 * `eval` tool's code, and a `python`/`node`/`bun` script a `bash` command runs. It is a tokenizer
 * (strings, comments, template literals, brackets) and a small evaluator for the path expressions
 * scripts build, never a pattern over raw text. What it can honestly see is a call to a known
 * deletion, move, truncation, overwrite, signal, or shell-out function whose arguments evaluate to
 * known strings: literals, concatenation, f-strings and template literals of known parts,
 * `os.path.join`/`path.join`, `Path(...) / "x"`, the home directory (`Path.home()`,
 * `os.path.expanduser`, `os.homedir()`, `HOME` from the environment), and names assigned once from
 * such expressions. An argument it cannot evaluate is left to run: that is the residual risk the
 * pane-guard documentation names, since arbitrary runtime code cannot be judged before it runs.
 */

export type CodeLanguage = "py" | "js";

/** Stands in, in code or shell text a command builds at run time, for a part the guard cannot
 * know (a shell variable read from input, a command's output); a value holding it evaluates to
 * unknown. */
export const UNKNOWN_MARKER = "__LEGION_GUARD_UNKNOWN__";

/** A value the evaluator knows, or `undefined` for one it cannot know before the code runs. */
type Value = string | readonly Value[] | undefined;

/** What the scanner found, handed back to the shell half of the guard, which owns the policy. */
export interface CodeSinks {
  /** A filesystem call on `target` (a known path string). */
  readonly path: (
    call: string,
    verb: "delete" | "move" | "truncate" | "overwrite",
    target: string
  ) => void;
  /** A signal to `pid` (a known integer string); a process group when `group` is set. */
  readonly signal: (call: string, pid: string, group: boolean) => void;
  /** Shell text run through a shell (`os.system`, `execSync`, `shell=True`); `unknown` holds the
   * placeholders `Bun.$` interpolations left for values the scanner cannot know. */
  readonly shell: (call: string, text: string, unknown: readonly string[]) => void;
  /** An argument vector run without a shell (`subprocess.run([...])`, `spawnSync`); an element the
   * scanner cannot know is `undefined`. */
  readonly argv: (call: string, argv: readonly (string | undefined)[]) => void;
}

/** The environment the code runs in: the pane's variables and the directory relative paths
 * resolve against. */
export interface CodeEnvironment {
  readonly env: NodeJS.ProcessEnv;
  readonly cwd: string;
  /** Where `tempfile.mkdtemp()`, `fs.mkdtempSync(...)` and friends create their directory. */
  readonly tmpdir: string;
  /** The `eval` tool's IPython kernel, whose `!cmd` lines and `%%bash` cells run shell text. */
  readonly ipython?: boolean;
}

interface Token {
  readonly kind: "name" | "string" | "number" | "punct" | "newline";
  readonly text: string;
  /** A string's decoded value; a template literal's or f-string's literal chunks alternate with
   * the source of each interpolation in `interpolations`. */
  readonly value?: string;
  readonly chunks?: readonly string[];
  readonly interpolations?: readonly string[];
  /** A JavaScript template literal (`` `...` ``), which is what a `Bun.$` tag takes. */
  readonly template?: boolean;
}

const PY_STRING_PREFIX = new Set(["r", "u", "b", "f", "br", "rb", "fr", "rf"]);

function isNameStart(char: string): boolean {
  return /[A-Za-z_$]/.test(char);
}

function isNameChar(char: string): boolean {
  return /[A-Za-z0-9_$]/.test(char);
}

/** Decodes the common backslash escapes of a quoted string body; unknown escapes keep the
 * character, as both languages do for the ones that matter to a path. */
function decodeEscapes(body: string): string {
  let out = "";
  for (let i = 0; i < body.length; i += 1) {
    const char = body.charAt(i);
    if (char !== "\\" || i + 1 >= body.length) {
      out += char;
      continue;
    }
    i += 1;
    const next = body.charAt(i);
    out += next === "n" ? "\n" : next === "t" ? "\t" : next === "\n" ? "" : next;
  }
  return out;
}

/** Splits an f-string or template literal body into literal chunks and interpolation sources. */
function splitInterpolations(
  body: string,
  open: "{" | "${"
): { chunks: string[]; interpolations: string[] } {
  const chunks: string[] = [];
  const interpolations: string[] = [];
  let chunk = "";
  for (let i = 0; i < body.length; i += 1) {
    if (open === "{" && body.startsWith("{{", i)) {
      chunk += "{";
      i += 1;
      continue;
    }
    if (open === "{" && body.startsWith("}}", i)) {
      chunk += "}";
      i += 1;
      continue;
    }
    if (body.startsWith(open, i)) {
      let depth = 1;
      let j = i + open.length;
      for (; j < body.length && depth > 0; j += 1) {
        if (body.charAt(j) === "{") depth += 1;
        else if (body.charAt(j) === "}") depth -= 1;
      }
      chunks.push(chunk);
      chunk = "";
      interpolations.push(body.slice(i + open.length, j - 1).replace(/[!:][^}]*$/, ""));
      i = j - 1;
      continue;
    }
    chunk += body.charAt(i);
  }
  chunks.push(chunk);
  return { chunks: chunks.map(decodeEscapes), interpolations };
}

/** Tokenizes Python or JavaScript source: names, numbers, strings (decoded), punctuation, and, in
 * Python, logical newlines outside brackets. Comments are dropped. */
export function tokenize(source: string, language: CodeLanguage): Token[] {
  const tokens: Token[] = [];
  let depth = 0;
  let i = 0;
  const previousSignificant = (): Token | undefined => {
    for (let k = tokens.length - 1; k >= 0; k -= 1) {
      const token = tokens[k];
      if (token !== undefined && token.kind !== "newline") return token;
    }
    return undefined;
  };
  while (i < source.length) {
    const char = source.charAt(i);
    if (char === "\n") {
      if (language === "py" && depth === 0) tokens.push({ kind: "newline", text: "\n" });
      else if (language === "js") tokens.push({ kind: "newline", text: "\n" });
      i += 1;
      continue;
    }
    if (char === " " || char === "\t" || char === "\r") {
      i += 1;
      continue;
    }
    if (char === "\\" && source.charAt(i + 1) === "\n") {
      i += 2;
      continue;
    }
    if (language === "py" && char === "#") {
      while (i < source.length && source.charAt(i) !== "\n") i += 1;
      continue;
    }
    if (language === "js" && source.startsWith("//", i)) {
      while (i < source.length && source.charAt(i) !== "\n") i += 1;
      continue;
    }
    if (language === "js" && source.startsWith("/*", i)) {
      const end = source.indexOf("*/", i + 2);
      i = end === -1 ? source.length : end + 2;
      continue;
    }
    // Python string with an optional prefix (r, b, f, ...).
    if (language === "py" && isNameStart(char)) {
      let j = i;
      while (j < source.length && isNameChar(source.charAt(j))) j += 1;
      const word = source.slice(i, j);
      const quote = source.charAt(j);
      if ((quote === '"' || quote === "'") && PY_STRING_PREFIX.has(word.toLowerCase())) {
        const scanned = scanPythonString(source, j, word.toLowerCase());
        tokens.push(scanned.token);
        i = scanned.end;
        continue;
      }
      tokens.push({ kind: "name", text: word });
      i = j;
      continue;
    }
    if (language === "py" && (char === '"' || char === "'")) {
      const scanned = scanPythonString(source, i, "");
      tokens.push(scanned.token);
      i = scanned.end;
      continue;
    }
    if (language === "js" && (char === '"' || char === "'")) {
      let j = i + 1;
      while (j < source.length && source.charAt(j) !== char && source.charAt(j) !== "\n") {
        if (source.charAt(j) === "\\") j += 1;
        j += 1;
      }
      const body = source.slice(i + 1, j);
      tokens.push({ kind: "string", text: source.slice(i, j + 1), value: decodeEscapes(body) });
      i = j + 1;
      continue;
    }
    if (language === "js" && char === "`") {
      let j = i + 1;
      let braces = 0;
      while (j < source.length) {
        const c = source.charAt(j);
        if (c === "\\") {
          j += 2;
          continue;
        }
        if (braces === 0 && c === "`") break;
        if (source.startsWith("${", j)) {
          braces += 1;
          j += 2;
          continue;
        }
        if (braces > 0 && c === "{") braces += 1;
        if (braces > 0 && c === "}") braces -= 1;
        j += 1;
      }
      const body = source.slice(i + 1, j);
      const { chunks, interpolations } = splitInterpolations(body, "${");
      tokens.push({
        kind: "string",
        text: source.slice(i, j + 1),
        value: interpolations.length === 0 ? chunks.join("") : undefined,
        chunks,
        interpolations,
        template: true,
      });
      i = j + 1;
      continue;
    }
    if (language === "js" && char === "/") {
      // A regular expression literal where an operand is expected, division otherwise.
      const before = previousSignificant();
      const operandBefore =
        before !== undefined &&
        (before.kind === "name" ||
          before.kind === "number" ||
          before.kind === "string" ||
          before.text === ")" ||
          before.text === "]" ||
          before.text === "}");
      if (!operandBefore) {
        let j = i + 1;
        let inClass = false;
        while (j < source.length && source.charAt(j) !== "\n") {
          const c = source.charAt(j);
          if (c === "\\") j += 1;
          else if (c === "[") inClass = true;
          else if (c === "]") inClass = false;
          else if (c === "/" && !inClass) break;
          j += 1;
        }
        j += 1;
        while (j < source.length && isNameChar(source.charAt(j))) j += 1;
        tokens.push({ kind: "punct", text: "regex" });
        i = j;
        continue;
      }
    }
    if (isNameStart(char)) {
      let j = i;
      while (j < source.length && isNameChar(source.charAt(j))) j += 1;
      tokens.push({ kind: "name", text: source.slice(i, j) });
      i = j;
      continue;
    }
    if (/[0-9]/.test(char)) {
      let j = i;
      while (j < source.length && /[0-9A-Za-z_.]/.test(source.charAt(j))) j += 1;
      tokens.push({ kind: "number", text: source.slice(i, j) });
      i = j;
      continue;
    }
    if ("([{".includes(char)) depth += 1;
    if (")]}".includes(char)) depth = Math.max(0, depth - 1);
    // Multi-character operators the evaluator distinguishes from their first character.
    const two = source.slice(i, i + 2);
    if (["==", "!=", "<=", ">=", "=>", "**", "//", "+=", "-="].includes(two)) {
      tokens.push({ kind: "punct", text: two });
      i += 2;
      continue;
    }
    tokens.push({ kind: "punct", text: char });
    i += 1;
  }
  return tokens;
}

function scanPythonString(
  source: string,
  quoteAt: number,
  prefix: string
): { token: Token; end: number } {
  const quote = source.charAt(quoteAt);
  const triple = source.startsWith(quote.repeat(3), quoteAt);
  const delimiter = triple ? quote.repeat(3) : quote;
  let j = quoteAt + delimiter.length;
  while (j < source.length && !source.startsWith(delimiter, j)) {
    if (!triple && source.charAt(j) === "\n") break;
    if (source.charAt(j) === "\\") j += 1;
    j += 1;
  }
  const body = source.slice(quoteAt + delimiter.length, j);
  const end = Math.min(source.length, j + delimiter.length);
  const text = source.slice(quoteAt - prefix.length, end);
  const raw = prefix.includes("r");
  if (prefix.includes("f")) {
    const { chunks, interpolations } = splitInterpolations(body, "{");
    return {
      token: {
        kind: "string",
        text,
        value: interpolations.length === 0 ? chunks.join("") : undefined,
        chunks,
        interpolations,
      },
      end,
    };
  }
  return { token: { kind: "string", text, value: raw ? body : decodeEscapes(body) }, end };
}

/** Index of the bracket closing the one at `open`, or the token count when unbalanced. */
function closing(tokens: readonly Token[], open: number): number {
  let depth = 0;
  for (let k = open; k < tokens.length; k += 1) {
    const text = tokens[k]?.text;
    if (text === "(" || text === "[" || text === "{") depth += 1;
    else if (text === ")" || text === "]" || text === "}") {
      depth -= 1;
      if (depth === 0) return k;
    }
  }
  return tokens.length;
}

/** Splits the tokens between two brackets at top-level commas into argument spans; a keyword
 * argument `name=value` comes back under its name. */
function splitArguments(
  tokens: readonly Token[],
  start: number,
  end: number
): { positional: Token[][]; keyword: Map<string, Token[]> } {
  const positional: Token[][] = [];
  const keyword = new Map<string, Token[]>();
  let depth = 0;
  let current: Token[] = [];
  const flush = (): void => {
    const span = current.filter((token) => token.kind !== "newline");
    current = [];
    if (span.length === 0) return;
    if (span.length >= 2 && span[0]?.kind === "name" && span[1]?.text === "=") {
      keyword.set(span[0].text, span.slice(2));
    } else positional.push(span);
  };
  for (let k = start; k < end; k += 1) {
    const token = tokens[k];
    if (token === undefined) break;
    if (token.text === "(" || token.text === "[" || token.text === "{") depth += 1;
    if (token.text === ")" || token.text === "]" || token.text === "}") depth -= 1;
    if (depth === 0 && token.text === ",") {
      flush();
      continue;
    }
    current.push(token);
  }
  flush();
  return { positional, keyword };
}

interface Scope {
  readonly language: CodeLanguage;
  readonly environment: CodeEnvironment;
  /** Names assigned exactly once, to the tokens of their value; a name assigned twice maps to
   * `undefined`. */
  readonly assignments: Map<string, readonly Token[] | undefined>;
  readonly evaluating: Set<string>;
}

function joinPath(parts: readonly Value[]): Value {
  let joined = "";
  for (const part of parts) {
    if (typeof part !== "string") return undefined;
    if (part.startsWith("/")) joined = part;
    else if (joined === "" || joined.endsWith("/")) joined += part;
    else joined += `/${part}`;
  }
  return joined;
}

function expandUser(value: Value, scope: Scope): Value {
  if (typeof value !== "string") return undefined;
  if (value !== "~" && !value.startsWith("~/")) return value;
  const home = scope.environment.env.HOME;
  return home === undefined ? undefined : home + value.slice(1);
}

function absolute(value: Value, scope: Scope): Value {
  if (typeof value !== "string") return undefined;
  return value.startsWith("/") ? value : joinPath([scope.environment.cwd, value]);
}

function environmentValue(name: Value, scope: Scope, fallback?: Value): Value {
  if (typeof name !== "string") return undefined;
  const value = scope.environment.env[name];
  return value === undefined ? fallback : value;
}

/** Evaluates an expression's tokens to a known value, or `undefined`. */
function evaluate(tokens: readonly Token[], scope: Scope): Value {
  const span = tokens.filter((token) => token.kind !== "newline");
  if (span.length === 0) return undefined;
  // Binary `+` (concatenation) and, in Python, `/` (pathlib join), left to right at depth 0.
  let depth = 0;
  for (let k = span.length - 1; k > 0; k -= 1) {
    const text = span[k]?.text;
    if (text === ")" || text === "]" || text === "}") depth += 1;
    else if (text === "(" || text === "[" || text === "{") depth -= 1;
    else if (depth === 0 && (text === "+" || (scope.language === "py" && text === "/"))) {
      const left = evaluate(span.slice(0, k), scope);
      const right = evaluate(span.slice(k + 1), scope);
      if (typeof left !== "string" || typeof right !== "string") return undefined;
      return text === "+" ? left + right : joinPath([left, right]);
    }
  }
  return evaluatePrimary(span, scope);
}

function evaluateInterpolated(token: Token, scope: Scope): Value {
  if (token.value !== undefined) return token.value;
  const chunks = token.chunks ?? [];
  const interpolations = token.interpolations ?? [];
  let out = chunks[0] ?? "";
  for (const [index, source] of interpolations.entries()) {
    const value = evaluate(tokenize(source, scope.language), scope);
    if (typeof value !== "string") return undefined;
    out += value + (chunks[index + 1] ?? "");
  }
  return out;
}

function evaluatePrimary(span: readonly Token[], scope: Scope): Value {
  const first = span[0];
  if (first === undefined) return undefined;
  if (span.length === 1) {
    if (first.kind === "string") {
      const value = evaluateInterpolated(first, scope);
      return typeof value === "string" && value.includes(UNKNOWN_MARKER) ? undefined : value;
    }
    if (first.kind === "number") return first.text;
    if (first.kind === "name") return evaluateName(first.text, scope);
    return undefined;
  }
  if (first.text === "(" && closing(span, 0) === span.length - 1) {
    return evaluate(span.slice(1, -1), scope);
  }
  if (first.text === "[" && closing(span, 0) === span.length - 1) {
    return splitArguments(span, 1, span.length - 1).positional.map((item) => evaluate(item, scope));
  }
  // `new Foo(...)` evaluates as `Foo(...)`.
  if (first.kind === "name" && first.text === "new") return evaluatePrimary(span.slice(1), scope);
  // A dotted name, optionally called, optionally indexed, optionally followed by method calls.
  let k = 0;
  const dotted: string[] = [];
  while (k < span.length && span[k]?.kind === "name") {
    dotted.push(span[k]?.text ?? "");
    k += 1;
    if (span[k]?.text === "." && span[k + 1]?.kind === "name") k += 1;
    else break;
  }
  if (dotted.length === 0) return undefined;
  let value: Value;
  const name = dotted.join(".");
  const next = span[k];
  let required: string | undefined;
  if (next?.text === "(") {
    const end = closing(span, k);
    const args = splitArguments(span, k + 1, end);
    required =
      name === "require" ? commonJsModule(evaluate(args.positional[0] ?? [], scope)) : undefined;
    value = required === undefined ? evaluateCall(name, args, scope) : undefined;
    k = end + 1;
  } else if (next?.text === "[") {
    const end = closing(span, k);
    const index = evaluate(span.slice(k + 1, end), scope);
    value =
      name === "os.environ" || name === "process.env" || name === "Bun.env" || name === "environ"
        ? environmentValue(index, scope)
        : undefined;
    k = end + 1;
  } else {
    value = evaluateDotted(name, scope);
  }
  // Method calls on the result: `.expanduser()`, `.resolve()`, `.joinpath(...)`, `.toString()`.
  while (k < span.length) {
    if (span[k]?.text !== "." || span[k + 1]?.kind !== "name") return undefined;
    const method = span[k + 1]?.text ?? "";
    k += 2;
    let args: { positional: Token[][]; keyword: Map<string, Token[]> } = {
      positional: [],
      keyword: new Map(),
    };
    if (span[k]?.text === "(") {
      const end = closing(span, k);
      args = splitArguments(span, k + 1, end);
      k = end + 1;
    }
    value =
      required === undefined
        ? evaluateMethod(value, method, args, scope)
        : evaluateCall(`${required}.${method}`, args, scope);
    required = undefined;
  }
  return value;
}

function evaluateName(name: string, scope: Scope): Value {
  if (!scope.assignments.has(name) || scope.evaluating.has(name)) return undefined;
  const tokens = scope.assignments.get(name);
  if (tokens === undefined) return undefined;
  scope.evaluating.add(name);
  try {
    return evaluate(tokens, scope);
  } finally {
    scope.evaluating.delete(name);
  }
}

function evaluateDotted(name: string, scope: Scope): Value {
  const env = /^(?:process\.env|Bun\.env|import\.meta\.env)\.([A-Za-z_][A-Za-z0-9_]*)$/.exec(name);
  if (env !== null) return environmentValue(env[1], scope);
  if (!name.includes(".")) return evaluateName(name, scope);
  return undefined;
}

function evaluateCall(
  name: string,
  args: { positional: Token[][]; keyword: Map<string, Token[]> },
  scope: Scope
): Value {
  const values = args.positional.map((span) => evaluate(span, scope));
  const [a, b] = values;
  switch (name) {
    case "str":
    case "String":
    case "os.fspath":
    case "Path":
    case "pathlib.Path":
    case "PurePath":
    case "pathlib.PurePath":
    case "PosixPath":
      return values.length === 0 ? "." : values.length === 1 ? a : joinPath(values);
    case "os.path.expanduser":
    case "expanduser":
      return expandUser(a, scope);
    case "os.path.join":
    case "path.join":
    case "join":
    case "posixpath.join":
      return joinPath(values);
    case "os.path.abspath":
    case "os.path.realpath":
    case "os.path.normpath":
    case "path.resolve":
    case "path.normalize":
      return values.length <= 1 ? absolute(a, scope) : absolute(joinPath(values), scope);
    case "Path.home":
    case "pathlib.Path.home":
    case "os.homedir":
    case "homedir":
      return scope.environment.env.HOME;
    case "Path.cwd":
    case "os.getcwd":
    case "process.cwd":
      return scope.environment.cwd;
    case "os.getenv":
    case "os.environ.get":
    case "environ.get":
      return environmentValue(a, scope, b);
    case "tempfile.mkdtemp":
    case "mkdtemp":
    case "tempfile.gettempdir":
    case "os.tmpdir":
    case "tmpdir":
      return joinPath([scope.environment.tmpdir, "tmp.XXXXXXXXXX"]);
    case "fs.mkdtempSync":
    case "mkdtempSync":
      return typeof a === "string" ? absolute(`${a}XXXXXX`, scope) : undefined;
    default:
      return undefined;
  }
}

function evaluateMethod(
  receiver: Value,
  method: string,
  args: { positional: Token[][]; keyword: Map<string, Token[]> },
  scope: Scope
): Value {
  const values = args.positional.map((span) => evaluate(span, scope));
  switch (method) {
    case "expanduser":
      return expandUser(receiver, scope);
    case "resolve":
    case "absolute":
      return absolute(receiver, scope);
    case "joinpath":
      return joinPath([receiver, ...values]);
    case "toString":
    case "__str__":
    case "as_posix":
      return receiver;
    default:
      return undefined;
  }
}

/** Records every `name = value` (Python, at a statement start) and `const|let|var name = value`
 * (JavaScript) assignment; a name assigned more than once is unknown. */
function collectAssignments(
  tokens: readonly Token[],
  language: CodeLanguage
): Scope["assignments"] {
  const assignments = new Map<string, readonly Token[] | undefined>();
  for (let k = 0; k < tokens.length; k += 1) {
    const token = tokens[k];
    const previous = tokens[k - 1];
    const statementStart =
      previous === undefined ||
      previous.kind === "newline" ||
      previous.text === ";" ||
      (language === "js" && ["const", "let", "var"].includes(previous.text));
    if (token?.kind !== "name" || !statementStart || tokens[k + 1]?.text !== "=") continue;
    let end = k + 2;
    let depth = 0;
    for (; end < tokens.length; end += 1) {
      const text = tokens[end]?.text;
      if (text === "(" || text === "[" || text === "{") depth += 1;
      else if (text === ")" || text === "]" || text === "}") depth -= 1;
      else if (depth === 0 && (tokens[end]?.kind === "newline" || text === ";")) break;
    }
    const value = tokens.slice(k + 2, end);
    assignments.set(token.text, assignments.has(token.text) ? undefined : value);
  }
  return assignments;
}

/** The span of tokens forming the receiver of the method call whose `.` is at `dot`. */
function receiverSpan(tokens: readonly Token[], dot: number): Token[] {
  let start = dot - 1;
  for (;;) {
    const token = tokens[start];
    if (token === undefined) break;
    if (token.text === ")" || token.text === "]") {
      let depth = 0;
      let k = start;
      for (; k >= 0; k -= 1) {
        const text = tokens[k]?.text;
        if (text === ")" || text === "]") depth += 1;
        else if (text === "(" || text === "[") {
          depth -= 1;
          if (depth === 0) break;
        }
      }
      start = k - 1;
      continue;
    }
    if (token.kind === "name" || token.kind === "string") {
      if (tokens[start - 1]?.text === "." && token.kind === "name") {
        start -= 2;
        continue;
      }
      break;
    }
    break;
  }
  return tokens.slice(Math.max(0, start), dot);
}

type PathVerb = "delete" | "move" | "truncate" | "overwrite";
/** A filesystem call: what it does and which positional arguments are its paths. Maps, since the
 * callee is read from the code and an object literal would answer `constructor`. */
type PathCall = readonly [verb: PathVerb, ...args: number[]];

const PY_PATH_CALLS = new Map<string, PathCall>([
  ["shutil.rmtree", ["delete", 0]],
  ["rmtree", ["delete", 0]],
  ["os.remove", ["delete", 0]],
  ["os.unlink", ["delete", 0]],
  ["os.rmdir", ["delete", 0]],
  ["os.removedirs", ["delete", 0]],
  ["shutil.move", ["move", 0, 1]],
  ["os.rename", ["move", 0, 1]],
  ["os.replace", ["move", 0, 1]],
  ["os.renames", ["move", 0, 1]],
  ["os.truncate", ["truncate", 0]],
]);

const JS_PATH_CALLS = new Map<string, PathCall>([["Bun.write", ["overwrite", 0]]]);
for (const [base, call] of [
  ["rm", ["delete", 0]],
  ["rmSync", ["delete", 0]],
  ["rmdir", ["delete", 0]],
  ["rmdirSync", ["delete", 0]],
  ["unlink", ["delete", 0]],
  ["unlinkSync", ["delete", 0]],
  ["rename", ["move", 0, 1]],
  ["renameSync", ["move", 0, 1]],
  ["truncate", ["truncate", 0]],
  ["truncateSync", ["truncate", 0]],
  ["writeFile", ["overwrite", 0]],
  ["writeFileSync", ["overwrite", 0]],
] as const satisfies readonly (readonly [string, PathCall])[]) {
  for (const prefix of ["", "fs.", "fsp.", "fs.promises.", "promises.", "fsPromises."]) {
    JS_PATH_CALLS.set(prefix + base, call);
  }
}

function commonJsModule(value: Value): string | undefined {
  switch (value) {
    case "fs":
    case "node:fs":
      return "fs";
    case "fs/promises":
    case "node:fs/promises":
      return "fs.promises";
    case "child_process":
    case "node:child_process":
      return "child_process";
    case "os":
    case "node:os":
      return "os";
    default:
      return undefined;
  }
}

const PY_METHOD_SINKS = new Map<string, PathVerb>([
  ["unlink", "delete"],
  ["rmdir", "delete"],
  ["rename", "move"],
  ["replace", "move"],
  ["write_text", "overwrite"],
  ["write_bytes", "overwrite"],
]);

const JS_METHOD_SINKS = new Map<string, PathVerb>([
  ["delete", "delete"],
  ["unlink", "delete"],
]);

const PY_SHELL_CALLS = new Set([
  "subprocess.run",
  "subprocess.call",
  "subprocess.check_call",
  "subprocess.check_output",
  "subprocess.Popen",
  "run",
  "call",
  "check_call",
  "check_output",
  "Popen",
]);

/** Scans `source` and reports each sink call whose arguments evaluate to known values. */
export function scanCode(
  language: CodeLanguage,
  source: string,
  environment: CodeEnvironment,
  sinks: CodeSinks
): void {
  if (language === "py" && environment.ipython === true) scanIpythonShell(source, sinks);
  const tokens = tokenize(source, language);
  const scope: Scope = {
    language,
    environment,
    assignments: collectAssignments(tokens, language),
    evaluating: new Set(),
  };
  for (let k = 0; k < tokens.length; k += 1) {
    const token = tokens[k];
    if (token === undefined) continue;
    // A method call on an evaluated receiver: `Path(x).unlink()`, `Bun.file(x).delete()`.
    if (token.text === "." && tokens[k + 1]?.kind === "name" && tokens[k + 2]?.text === "(") {
      const method = tokens[k + 1]?.text ?? "";
      const verb = (language === "py" ? PY_METHOD_SINKS : JS_METHOD_SINKS).get(method);
      // `str.replace` and `str.rename`-like names are common: a move needs a path receiver.
      const receiverTokens = receiverSpan(tokens, k);
      if (verb !== undefined && (verb !== "move" || pathReceiver(receiverTokens, scope))) {
        const receiver =
          language === "js"
            ? bunFileTarget(receiverTokens, scope)
            : evaluate(receiverTokens, scope);
        const call = `${receiverTokens.map((t) => t.text).join("")}.${method}()`;
        if (typeof receiver === "string") sinks.path(call, verb, absoluteOrSelf(receiver, scope));
        if (verb === "move") {
          const end = closing(tokens, k + 2);
          const [destination] = splitArguments(tokens, k + 3, end).positional;
          const value = destination === undefined ? undefined : evaluate(destination, scope);
          if (typeof value === "string") sinks.path(call, verb, absoluteOrSelf(value, scope));
        }
      }
      continue;
    }
    if (token.kind !== "name") continue;
    let j = k;
    let callee: string;
    if (tokens[k - 1]?.text === ".") {
      if (language !== "js" || tokens[k + 1]?.text !== "(") continue;
      const receiver = receiverSpan(tokens, k - 1);
      if (
        receiver[0]?.text !== "require" ||
        receiver[1]?.text !== "(" ||
        receiver.at(-1)?.text !== ")"
      ) {
        continue;
      }
      const [module] = splitArguments(receiver, 2, receiver.length - 1).positional;
      const resolved = commonJsModule(evaluate(module ?? [], scope));
      if (resolved === undefined) continue;
      callee = `${resolved}.${token.text}`;
    } else {
      // The dotted callee starting here.
      const parts = [token.text];
      while (tokens[j + 1]?.text === "." && tokens[j + 2]?.kind === "name") {
        parts.push(tokens[j + 2]?.text ?? "");
        j += 2;
      }
      callee = parts.join(".");
    }
    const after = tokens[j + 1];
    // `Bun.$\`...\`` / `$\`...\``: a tagged template is shell text.
    if (language === "js" && after?.template === true && (callee === "$" || callee === "Bun.$")) {
      const unknown: string[] = [];
      let text = after.chunks?.[0] ?? "";
      for (const [index, source] of (after.interpolations ?? []).entries()) {
        const value = evaluate(tokenize(source, "js"), scope);
        if (typeof value === "string") text += `'${value.replaceAll("'", "'\\''")}'`;
        else {
          const placeholder = `LEGION_GUARD_UNKNOWN_${index}`;
          unknown.push(placeholder);
          text += `"$${placeholder}"`;
        }
        text += after.chunks?.[index + 1] ?? "";
      }
      sinks.shell(callee, text, unknown);
      continue;
    }
    if (after?.text !== "(") continue;
    const end = closing(tokens, j + 1);
    const args = splitArguments(tokens, j + 2, end);
    const value = (index: number): Value => {
      const span = args.positional[index];
      return span === undefined ? undefined : evaluate(span, scope);
    };
    const pathCall = (language === "py" ? PY_PATH_CALLS : JS_PATH_CALLS).get(callee);
    if (pathCall !== undefined) {
      const [verb, ...indexes] = pathCall;
      for (const index of indexes) {
        const target = value(index);
        if (typeof target === "string") sinks.path(callee, verb, absoluteOrSelf(target, scope));
      }
      continue;
    }
    if (language === "py" && callee === "open") {
      const target = value(0);
      const modeSpan = args.positional[1] ?? args.keyword.get("mode");
      const mode = modeSpan === undefined ? "r" : evaluate(modeSpan, scope);
      if (typeof target === "string" && typeof mode === "string" && mode.includes("w")) {
        sinks.path(callee, "overwrite", absoluteOrSelf(target, scope));
      }
      continue;
    }
    if (callee === "os.kill" || callee === "os.killpg" || callee === "process.kill") {
      const pid = value(0);
      if (typeof pid === "string") sinks.signal(callee, pid, callee === "os.killpg");
      else sinks.signal(callee, "", callee === "os.killpg");
      continue;
    }
    if (language === "py") scanPythonProcessCall(callee, args, value, sinks);
    else scanJsProcessCall(callee, args, value, scope, sinks);
  }
}

function absoluteOrSelf(target: string, scope: Scope): string {
  return (absolute(target, scope) as string | undefined) ?? target;
}

const PATH_CONSTRUCTORS = ["Path", "pathlib", "PosixPath", "PurePath", "PurePosixPath"];
/** pathlib methods that return a path, so a chain of them is still one. */
const PATH_METHODS = ["expanduser", "resolve", "absolute", "joinpath", "with_name", "with_suffix"];

/** Whether a receiver is a pathlib path: a `Path` constructor followed only by methods that
 * return a path, or a name assigned once from one. `"a".replace(...)` and
 * `Path(p).read_text().replace(...)` are strings, never a move. */
function pathReceiver(receiver: readonly Token[], scope: Scope, depth = 0): boolean {
  const first = receiver[0];
  if (first === undefined || first.kind !== "name" || depth > 8) return false;
  if (receiver.length === 1) {
    const assigned = scope.assignments.get(first.text);
    return assigned !== undefined && pathReceiver(assigned, scope, depth + 1);
  }
  if (!PATH_CONSTRUCTORS.includes(first.text)) return false;
  let k = 1;
  while (
    receiver[k]?.text === "." &&
    receiver[k + 1]?.kind === "name" &&
    receiver[k + 2]?.text !== "("
  ) {
    k += 2;
  }
  if (receiver[k]?.text !== "(") return false;
  k = closing(receiver, k) + 1;
  while (k < receiver.length) {
    const method = receiver[k + 1]?.text ?? "";
    if (receiver[k]?.text !== "." || !PATH_METHODS.includes(method)) return false;
    k += 2;
    if (receiver[k]?.text === "(") k = closing(receiver, k) + 1;
  }
  return true;
}

/** `Bun.file(x)` evaluates to `x`; any other receiver is evaluated as an expression. */
function bunFileTarget(receiver: readonly Token[], scope: Scope): Value {
  if (
    receiver[0]?.text === "Bun" &&
    receiver[1]?.text === "." &&
    receiver[2]?.text === "file" &&
    receiver[3]?.text === "("
  ) {
    const end = closing(receiver, 3);
    const [first] = splitArguments(receiver, 4, end).positional;
    return first === undefined ? undefined : evaluate(first, scope);
  }
  return undefined;
}
function argvOf(value: Value): (string | undefined)[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.map((item) =>
    typeof item === "string" && !item.includes(UNKNOWN_MARKER) ? item : undefined
  );
}

function scanPythonProcessCall(
  callee: string,
  args: { positional: Token[][]; keyword: Map<string, Token[]> },
  value: (index: number) => Value,
  sinks: CodeSinks
): void {
  if (callee === "os.system" || callee === "os.popen" || callee.endsWith("getoutput")) {
    const text = value(0);
    if (typeof text === "string") sinks.shell(callee, text, []);
    return;
  }
  if (/^os\.exec[lv]p?e?$/.test(callee) || /^os\.spawn[lv]p?e?$/.test(callee)) {
    const offset = callee.startsWith("os.spawn") ? 1 : 0;
    const list = argvOf(value(offset + 1));
    if (list !== undefined) sinks.argv(callee, list);
    return;
  }
  if (!PY_SHELL_CALLS.has(callee)) return;
  const first = value(0);
  const shellSpan = args.keyword.get("shell");
  const shell = shellSpan !== undefined && shellSpan.map((t) => t.text).join("") === "True";
  if (typeof first === "string") {
    if (shell) sinks.shell(callee, first, []);
    else sinks.argv(callee, [first]);
    return;
  }
  const list = argvOf(first);
  if (list === undefined) return;
  if (shell) {
    if (list.every((item): item is string => item !== undefined)) {
      sinks.shell(callee, list.join(" "), []);
    }
    return;
  }
  sinks.argv(callee, list);
}

function scanJsProcessCall(
  callee: string,
  args: { positional: Token[][]; keyword: Map<string, Token[]> },
  value: (index: number) => Value,
  scope: Scope,
  sinks: CodeSinks
): void {
  const base = callee.replace(/^(?:child_process|cp|childProcess)\./, "");
  if (base === "exec" || base === "execSync") {
    const text = value(0);
    if (typeof text === "string") sinks.shell(callee, text, []);
    return;
  }
  if (["spawn", "spawnSync", "execFile", "execFileSync"].includes(base)) {
    const program = value(0);
    const rest = argvOf(value(1)) ?? [];
    if (typeof program !== "string") return;
    const options = args.positional[2];
    const shell =
      options?.some(
        (token, index) => token.text === "shell" && options[index + 2]?.text === "true"
      ) === true;
    if (shell && rest.every((item): item is string => item !== undefined)) {
      sinks.shell(callee, [program, ...rest].join(" "), []);
    } else sinks.argv(callee, [program, ...rest]);
    return;
  }
  if (callee === "Bun.spawn" || callee === "Bun.spawnSync") {
    const first = args.positional[0];
    if (first === undefined) return;
    let list = argvOf(evaluate(first, scope));
    if (list === undefined && first[0]?.text === "{") {
      // `Bun.spawn({ cmd: [...] })`
      const cmd = first.findIndex(
        (token, index) => token.text === "cmd" && first[index + 1]?.text === ":"
      );
      if (cmd !== -1 && first[cmd + 2]?.text === "[") {
        const end = closing(first, cmd + 2);
        list = argvOf(evaluate(first.slice(cmd + 2, end + 1), scope));
      }
    }
    if (list !== undefined) sinks.argv(callee, list);
    return;
  }
  if (callee === "tool.bash") {
    // The `eval` tool's bridge to the `bash` tool: `await tool.bash({ command: "..." })`.
    const first = args.positional[0];
    if (first === undefined) return;
    const command = first.findIndex(
      (token, index) => token.text === "command" && first[index + 1]?.text === ":"
    );
    if (command === -1) return;
    let end = command + 2;
    let depth = 0;
    for (; end < first.length; end += 1) {
      const text = first[end]?.text;
      if (text === "(" || text === "[" || text === "{") depth += 1;
      else if (text === ")" || text === "]" || text === "}") {
        if (depth === 0) break;
        depth -= 1;
      } else if (depth === 0 && text === ",") break;
    }
    const text = evaluate(first.slice(command + 2, end), scope);
    if (typeof text === "string") sinks.shell(callee, text, []);
  }
}

/** IPython's shell escapes in the `eval` tool's Python: `!cmd`, `!!cmd`, `%sx cmd`,
 * `%system cmd`, and a `%%bash`/`%%sh`/`%%script bash` cell. */
function scanIpythonShell(source: string, sinks: CodeSinks): void {
  const lines = source.split("\n");
  const first = lines[0]?.trim() ?? "";
  if (/^%%(?:bash|sh|script\s+(?:bash|sh))\b/.test(first)) {
    sinks.shell(first, lines.slice(1).join("\n"), []);
    return;
  }
  for (const line of lines) {
    const trimmed = line.trimStart();
    const shellEscape = /^(?:!!?|%sx\s|%system\s)(.*)$/.exec(trimmed);
    if (shellEscape !== null) sinks.shell("!", shellEscape[1] ?? "", []);
    // `x = !cmd` assigns a shell escape's output.
    const assigned = /^[A-Za-z_][A-Za-z0-9_]*\s*=\s*!(.*)$/.exec(trimmed);
    if (assigned !== null) sinks.shell("!", assigned[1] ?? "", []);
  }
}
