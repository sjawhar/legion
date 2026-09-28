/**
 * What bash computes for the constructs the pane guard (`pane-guard.ts`) evaluates rather than
 * walks: pattern removal and replacement over a known value, `test`'s string and integer forms,
 * and `printf %q`'s quoting. Pure functions of text; `pane-guard-bash.test.ts` holds them to real
 * bash. A pattern arrives as a regular expression built by `pane-guard.ts`'s `patternSource`.
 */

/** Whether `text` is ASCII alone. Outside it, what bash counts as one character (`?`, a removal's
 * length) is a character in a UTF-8 locale and a byte in the C locale, and the guard knows neither
 * the locale nor the encoding. */
export function isAscii(text: string): boolean {
  for (let i = 0; i < text.length; i += 1) if (text.charCodeAt(i) > 0x7f) return false;
  return true;
}

/** The length of the prefix (`start`) or suffix (`end`) of `value` that `pattern` matches whole,
 * the shortest or the longest; undefined when none does. */
function matchEdge(
  value: string,
  pattern: RegExp,
  from: "start" | "end",
  longest: boolean
): number | undefined {
  const n = value.length;
  for (let k = 0; k <= n; k += 1) {
    const length = longest ? n - k : k;
    if (pattern.test(from === "start" ? value.slice(0, length) : value.slice(n - length))) {
      return length;
    }
  }
  return undefined;
}

/** `value` with the shortest (`#`, `%`) or longest (`##`, `%%`) prefix (`#`) or suffix (`%`)
 * `pattern` matches removed; `value` itself when none matches. */
export function removePattern(value: string, operator: string, pattern: RegExp): string {
  const from = operator.startsWith("#") ? "start" : "end";
  const length = matchEdge(value, pattern, from, operator.length === 2);
  if (length === undefined) return value;
  return from === "start" ? value.slice(length) : value.slice(0, value.length - length);
}

/** `value` with the longest match of `pattern` replaced: the first (`/`), every one left to right
 * (`//`), one at the start (`/#`) or one at the end (`/%`). `/` and `//` replace an empty match
 * only in an empty value; their pattern is never null (`patternExpansion` in `pane-guard.ts`). */
export function replacePattern(
  value: string,
  operator: string,
  pattern: RegExp,
  replacement: string
): string {
  const n = value.length;
  if (operator === "/#" || operator === "/%") {
    const from = operator === "/#" ? "start" : "end";
    const length = matchEdge(value, pattern, from, true);
    if (length === undefined) return value;
    return from === "start"
      ? replacement + value.slice(length)
      : value.slice(0, n - length) + replacement;
  }
  if (n === 0) return pattern.test("") ? replacement : value;
  let out = "";
  let at = 0;
  while (at < n) {
    let end = n;
    while (end > at && !pattern.test(value.slice(at, end))) end -= 1;
    if (end === at) {
      out += value[at];
      at += 1;
      continue;
    }
    out += replacement;
    at = end;
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
