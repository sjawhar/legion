/**
 * What bash computes for the constructs the pane guard (`pane-guard.ts`) evaluates rather than
 * walks: pattern matching, removal and replacement over a known value, `test`'s string and integer
 * forms, and `printf %q`'s quoting. Pure functions of text; `pane-guard-bash.test.ts` holds them to
 * real bash. A pattern arrives as a `Glob` built by `pane-guard.ts`'s `patternGlob`.
 */

/** Whether `text` is ASCII alone. Outside it, what bash counts as one character (`?`, a removal's
 * length) is a character in a UTF-8 locale and a byte in the C locale, and the guard knows neither
 * the locale nor the encoding. */
export function isAscii(text: string): boolean {
  for (let i = 0; i < text.length; i += 1) if (text.charCodeAt(i) > 0x7f) return false;
  return true;
}

/** A bash pattern of known ASCII text and unquoted `*` and `?`, one element per character:
 * `ANY_RUN` for `*`, `ANY_ONE` for `?`, a character code otherwise. It is matched by stepping the
 * set of pattern positions still reachable along the value, so matching a value costs at most its
 * length times the pattern's (`globWork`), whatever the pattern. A regular expression backtracks
 * instead: `^.*a.*a.*a.*b$` over a run of `a`s takes time polynomial in the run, the stars the
 * exponent, and one such test would hold the pane for hours. */
export type Glob = readonly number[];
export const ANY_RUN = -1;
export const ANY_ONE = -2;

/** The most matching work an operator does over a value of `n` characters, in pattern positions
 * stepped: one pass for a `case` item and an anchored operator, one from every start for `/` and
 * `//`. A character costs its pattern's positions plus about eight more, the clearing and closing
 * of the reached set, measured on the devbox. */
export function globWork(glob: Glob, n: number, operator: string): number {
  const characters = operator === "/" || operator === "//" ? (n * (n + 3)) / 2 + 1 : n + 1;
  return characters * (glob.length + 8);
}

/** Element `k` is 1 when `glob` matches `value.slice(from, from + k)` whole. */
function prefixMatches(glob: Glob, value: string, from: number): Uint8Array {
  const m = glob.length;
  const matches = new Uint8Array(value.length - from + 1);
  let reached = new Uint8Array(m + 1);
  let next = new Uint8Array(m + 1);
  reached[0] = 1;
  closeRuns(glob, reached);
  for (let k = 0; ; k += 1) {
    matches[k] = reached[m] as number;
    if (from + k === value.length) break;
    const c = value.charCodeAt(from + k);
    next.fill(0);
    let any = false;
    for (let i = 0; i < m; i += 1) {
      if (reached[i] !== 1) continue;
      const element = glob[i];
      if (element === ANY_RUN) next[i] = 1;
      else if (element === ANY_ONE || element === c) next[i + 1] = 1;
      else continue;
      any = true;
    }
    // No position left: no longer slice matches either.
    if (!any) break;
    closeRuns(glob, next);
    [reached, next] = [next, reached];
  }
  return matches;
}

/** A `*` matches the empty run too, so reaching it reaches the position after it. */
function closeRuns(glob: Glob, reached: Uint8Array): void {
  for (let i = 0; i < glob.length; i += 1) {
    if (reached[i] === 1 && glob[i] === ANY_RUN) reached[i + 1] = 1;
  }
}

/** Whether `glob` matches all of `value`, as a `case` item's pattern does. */
export function globMatches(glob: Glob, value: string): boolean {
  return prefixMatches(glob, value, 0)[value.length] === 1;
}

/** The length of the prefix (`start`) or suffix (`end`) of `value` that `glob` matches whole, the
 * shortest or the longest; undefined when none does. A suffix is the prefix of the reversed value
 * the reversed pattern matches. */
function matchEdge(
  value: string,
  glob: Glob,
  from: "start" | "end",
  longest: boolean
): number | undefined {
  const matches =
    from === "start"
      ? prefixMatches(glob, value, 0)
      : prefixMatches([...glob].reverse(), [...value].reverse().join(""), 0);
  const length = longest ? matches.lastIndexOf(1) : matches.indexOf(1);
  return length === -1 ? undefined : length;
}

/** `value` with the shortest (`#`, `%`) or longest (`##`, `%%`) prefix (`#`) or suffix (`%`)
 * `glob` matches removed; `value` itself when none matches. */
export function removePattern(value: string, operator: string, glob: Glob): string {
  const from = operator.startsWith("#") ? "start" : "end";
  const length = matchEdge(value, glob, from, operator.length === 2);
  if (length === undefined) return value;
  return from === "start" ? value.slice(length) : value.slice(0, value.length - length);
}

/** `value` with the longest match of `glob` replaced: the first (`/`), every one left to right
 * (`//`), one at the start (`/#`) or one at the end (`/%`). `/` and `//` replace an empty match
 * only in an empty value, and a null pattern leaves the value as it is; `/#` and `/%` insert at
 * their end for one. */
export function replacePattern(
  value: string,
  operator: string,
  glob: Glob,
  replacement: string
): string {
  const n = value.length;
  if (operator === "/#" || operator === "/%") {
    const from = operator === "/#" ? "start" : "end";
    const length = matchEdge(value, glob, from, true);
    if (length === undefined) return value;
    return from === "start"
      ? replacement + value.slice(length)
      : value.slice(0, n - length) + replacement;
  }
  if (glob.length === 0) return value;
  if (n === 0) return globMatches(glob, "") ? replacement : value;
  let out = "";
  let at = 0;
  while (at < n) {
    const length = prefixMatches(glob, value, at).lastIndexOf(1);
    if (length <= 0) {
      out += value[at];
      at += 1;
      continue;
    }
    out += replacement;
    at += length;
    if (operator === "/") return out + value.slice(at);
  }
  return out;
}

/** `test`'s answer for the forms an argument loop uses: a string's emptiness, string equality,
 * and integer comparison. Anything else (file tests, `-a`, `-o`, parentheses) is undecided. */
export function evaluateTest(operands: readonly string[]): boolean | undefined {
  const [left = "", operator = "", right = ""] = operands;
  if (operands.length === 1) return left !== "";
  if (operands.length === 2) {
    if (left === "!") return operator === "";
    if (left === "-n") return operator !== "";
    if (left === "-z") return operator === "";
    return undefined;
  }
  if (operands.length !== 3) return undefined;
  if (operator === "=" || operator === "==") return left === right;
  if (operator === "!=") return left !== right;
  if (!/^-?[0-9]+$/.test(left) || !/^-?[0-9]+$/.test(right)) return undefined;
  const a = Number(left);
  const b = Number(right);
  switch (operator) {
    case "-eq":
      return a === b;
    case "-ne":
      return a !== b;
    case "-gt":
      return a > b;
    case "-ge":
      return a >= b;
    case "-lt":
      return a < b;
    case "-le":
      return a <= b;
    default:
      return undefined;
  }
}

/** `text` as a word a shell reads back as `text`, which is what `printf %q` guarantees. */
export function shellQuoted(text: string): string {
  if (text === "") return "''";
  if (/^[A-Za-z0-9_/.:@%+=,-]+$/.test(text)) return text;
  return `'${text.replaceAll("'", "'\\''")}'`;
}
