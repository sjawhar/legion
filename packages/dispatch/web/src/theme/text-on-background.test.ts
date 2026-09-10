import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";

import * as classes from "./classes";
import type * as P from "./palette";

/**
 * `palette.test.ts` verifies every *registered* foreground/background pair meets WCAG AA.
 * `no-raw-colors.test.ts` verifies every component uses a registered composite instead of a raw
 * literal. Neither one verifies that a composite is only ever *combined* with backgrounds its
 * registration actually covers — a text composite named `textSecondaryOnSurface` rendered
 * directly inside a `calloutInfoBg` container (a real bug this test caught: `Composer.tsx` did
 * exactly that with `textMutedOnSurface`, which measures 4.47:1 there, under AA) is invisible to
 * both of those tests.
 *
 * This test statically resolves, for every text-color composite used in a JSX element, the
 * nearest enclosing element *in the same file* that sets a background-color composite (its own
 * className counts too — the common case of a bg and text role on the same element), then
 * asserts that (foreground, background) swatch pair is registered in `classes.ts`'s
 * `CONTRAST_CHECKS`. `CONTRAST_CHECKS` is imported live, not hand-copied, so a check added there
 * is picked up automatically. Object-valued composites (`badgeMed`, `statusConnected`, ...) are
 * resolved too: a `badgeMed.text`/`badgeMed.bg` property access in a `className` is treated the
 * same as a plain identifier reference to a `"badgeMed.text"`/`"badgeMed.bg"` composite.
 *
 * Deliberately out of scope, and why:
 * - A text composite with no background-setting ancestor anywhere in its own file (e.g. a `<p>`
 *   rendered straight into whatever the *caller* wraps it in) inherits a background this test
 *   cannot see without full cross-file component-tree analysis. These are recorded as
 *   `UNRESOLVED_FILES` below for visibility, not failed — the export's own name (`OnCanvas`,
 *   `OnSurface`, `OnSurfaceMuted`) documents the developer's intent, and `palette.test.ts`
 *   already verifies that composite against the background its name claims.
 * - `checkboxAccent` sets a checkbox's `accent-color`, not text color — WCAG contrast math for
 *   text doesn't apply to it, so it's excluded from the text-composite set entirely.
 * - `disclosureButtonText`'s resting state deliberately reuses `TEXT_DISABLED`'s WCAG-exempt
 *   shade (SC 1.4.11 doesn't apply to a non-active control) — same exemption `TEXT_DISABLED`
 *   itself already gets in `classes.ts`, so it's excluded too.
 * - `errorCodeBlockBg`/`errorCodeBlockText` is a single asymmetric pair whose light half equals
 *   an already-registered "primary text on surface" and whose dark half equals an
 *   already-registered "danger callout text" — `classes.ts` documents this reuse inline rather
 *   than registering a third, redundant check, so this pair is excluded by name.
 */

const EXEMPT_TEXT_IDENTIFIERS = new Set(["checkboxAccent", "disclosureButtonText"]);
const EXEMPT_PAIRS = new Set(["errorCodeBlockText|errorCodeBlockBg"]);

const THEME_DIR = import.meta.dir;
const WEB_SRC_ROOT = join(THEME_DIR, "..");

// ------------------------------------------------------------------------------------------
// Step 1: classify every string-valued `classes.ts` export as a background and/or text role,
// by parsing its own source text (not the evaluated module — the tokens must appear literally
// for Tailwind's scanner to see them, exactly `no-raw-colors.test.ts`'s premise).
// ------------------------------------------------------------------------------------------

const classesSource = readFileSync(join(THEME_DIR, "classes.ts"), "utf8");
const EXPORT_PATTERN = /export const ([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([^"]*)";/g;

interface SwatchPair {
  readonly light: string;
  readonly dark: string;
}

/** A resting (non-`hover:`/`disabled:`/`focus:`-modified) `bg-`/`text-` token, either paired
 * with an explicit `dark:` counterpart or, if there is none, fixed across both schemes. */
const RESTING_TOKEN_PATTERN =
  /^(dark:)?(hover:|disabled:|focus:|focus-visible:|enabled:hover:|enabled:)?(bg|text)-([a-z]+-\d{2,3}|white|black)$/;

function classifyComposite(value: string): { bg?: SwatchPair; text?: SwatchPair } {
  const resting: {
    bg: Partial<Record<"light" | "dark", string>>;
    text: Partial<Record<"light" | "dark", string>>;
  } = { bg: {}, text: {} };
  for (const token of value.split(/\s+/).filter(Boolean)) {
    const match = RESTING_TOKEN_PATTERN.exec(token);
    if (match === null) continue;
    const [, darkPrefix, modifier, prop, swatch] = match;
    if (modifier !== undefined) continue;
    resting[prop as "bg" | "text"][darkPrefix === undefined ? "light" : "dark"] = swatch;
  }
  const result: { bg?: SwatchPair; text?: SwatchPair } = {};
  for (const prop of ["bg", "text"] as const) {
    const { light, dark } = resting[prop];
    if (light !== undefined && dark !== undefined) result[prop] = { dark, light };
    else if (light !== undefined) result[prop] = { dark: light, light };
    else if (dark !== undefined) result[prop] = { dark, light: dark };
  }
  return result;
}

const bgComposites = new Map<string, SwatchPair>();
const textComposites = new Map<string, SwatchPair>();
for (const match of classesSource.matchAll(EXPORT_PATTERN)) {
  const [, name, value] = match;
  if (EXEMPT_TEXT_IDENTIFIERS.has(name)) continue;
  const { bg, text } = classifyComposite(value);
  if (bg !== undefined) bgComposites.set(name, bg);
  if (text !== undefined) textComposites.set(name, text);
}
/** `export const badgeMed = { bg: "...", text: "..." };` — an object-valued composite whose
 * `.bg`/`.text` properties are each classified exactly like a standalone string export, then
 * registered under a qualified `"name.bg"`/`"name.text"` key so a `badgeMed.text` property
 * access in a component resolves to the same swatch pair a plain identifier would. */
const OBJECT_EXPORT_PATTERN = /export const ([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\{([^}]*)\};/g;
for (const match of classesSource.matchAll(OBJECT_EXPORT_PATTERN)) {
  const [, name, body] = match;
  const bgValue = /\bbg:\s*"([^"]*)"/.exec(body)?.[1];
  const textValue = /\btext:\s*"([^"]*)"/.exec(body)?.[1];
  if (bgValue !== undefined) {
    const { bg } = classifyComposite(bgValue);
    if (bg !== undefined) bgComposites.set(`${name}.bg`, bg);
  }
  if (textValue !== undefined) {
    const { text } = classifyComposite(textValue);
    if (text !== undefined) textComposites.set(`${name}.text`, text);
  }
}

test("classifies a non-trivial number of background and text composites", () => {
  // A regression guard on the classification itself.
  expect(bgComposites.size).toBeGreaterThan(15);
  expect(textComposites.size).toBeGreaterThan(30);
});

// ------------------------------------------------------------------------------------------
// Step 2: the set of (foreground, background) swatch-name pairs `classes.ts` has actually
// registered, read live from `CONTRAST_CHECKS` — not hand-copied.
// ------------------------------------------------------------------------------------------

function swatchName(swatch: P.Swatch): string {
  return swatch.name;
}

const registeredPairs = new Set(
  classes.CONTRAST_CHECKS.map(
    (check) =>
      `${swatchName(check.foreground.light)}|${swatchName(check.foreground.dark)}|${swatchName(check.background.light)}|${swatchName(check.background.dark)}`
  )
);

test("registers a non-trivial number of contrast checks", () => {
  expect(registeredPairs.size).toBeGreaterThan(20);
});

// ------------------------------------------------------------------------------------------
// Step 3: walk every component's JSX, tracking the nearest enclosing element (in the same
// file) that sets a background composite, and record every text composite's effective
// background(s).
// ------------------------------------------------------------------------------------------

interface Finding {
  readonly bgIdentifiers: readonly string[];
  readonly line: number;
  readonly textIdentifier: string;
}

/** Collects both plain identifiers (`surfaceMutedBg`) and `name.bg`/`name.text` property
 * accesses (`badgeMed.text`) as composite references, using the same `"name"` /
 * `"name.bg"`/`"name.text"` keys the classification step above registers them under. A matched
 * property access is not descended into further, so `badgeMed` alone (not itself a registered
 * composite) isn't also added as a spurious identifier. */
function findCompositeReferences(node: ts.Node, out: Set<string>): void {
  if (
    ts.isPropertyAccessExpression(node) &&
    ts.isIdentifier(node.expression) &&
    (node.name.text === "bg" || node.name.text === "text")
  ) {
    out.add(`${node.expression.text}.${node.name.text}`);
    return;
  }
  if (ts.isIdentifier(node)) out.add(node.text);
  ts.forEachChild(node, (child) => findCompositeReferences(child, out));
}

/** Resolves an expression into every mutually exclusive set of composite identifiers it could
 * produce at runtime. A `cond ? a : b` ternary contributes two alternative branches rather than
 * one flattened union — only the active branch appears in a rendered component, so treating both
 * as simultaneously present would fabricate a pairing that can never render. A template literal's
 * `${...}` spans, by contrast, are all present together, so their branch sets combine
 * multiplicatively (cross product). Anything else (a function call, a plain string) collapses to
 * a single branch via `findCompositeReferences`, which is the best this static analysis can do.
 */
function resolveBranches(node: ts.Node): Set<string>[] {
  if (ts.isConditionalExpression(node)) {
    return [...resolveBranches(node.whenTrue), ...resolveBranches(node.whenFalse)];
  }
  if (ts.isParenthesizedExpression(node)) {
    return resolveBranches(node.expression);
  }
  if (ts.isTemplateExpression(node)) {
    let branches: Set<string>[] = [new Set<string>()];
    for (const span of node.templateSpans) {
      const spanBranches = resolveBranches(span.expression);
      branches = branches.flatMap((existing) =>
        spanBranches.map((spanBranch) => new Set([...existing, ...spanBranch]))
      );
    }
    return branches;
  }
  const identifiers = new Set<string>();
  findCompositeReferences(node, identifiers);
  return [identifiers];
}

function classNameBranches(openingElement: ts.JsxOpeningLikeElement): Set<string>[] {
  let branches: Set<string>[] = [new Set<string>()];
  for (const attr of openingElement.attributes.properties) {
    if (
      !ts.isJsxAttribute(attr) ||
      attr.name.getText() !== "className" ||
      attr.initializer === undefined
    ) {
      continue;
    }
    const expr = ts.isJsxExpression(attr.initializer)
      ? attr.initializer.expression
      : attr.initializer;
    if (expr === undefined) continue;
    const exprBranches = resolveBranches(expr);
    branches = branches.flatMap((existing) =>
      exprBranches.map((exprBranch) => new Set([...existing, ...exprBranch]))
    );
  }
  return branches;
}

function scanFile(filePath: string, source: string): Finding[] {
  const sourceFile = ts.createSourceFile(
    filePath,
    source,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX
  );
  const findings: Finding[] = [];

  function visit(node: ts.Node, enclosingBg: readonly string[]): void {
    const openingElement = ts.isJsxElement(node)
      ? node.openingElement
      : ts.isJsxSelfClosingElement(node)
        ? node
        : undefined;

    let nextEnclosingBg = enclosingBg;
    if (openingElement !== undefined) {
      const branches = classNameBranches(openingElement);
      const bgAcrossBranches: string[] = [];
      for (const branch of branches) {
        const bgHere = [...branch].filter((name) => bgComposites.has(name));
        const textHere = [...branch].filter((name) => textComposites.has(name));
        const effectiveBg = bgHere.length > 0 ? bgHere : enclosingBg;
        for (const textIdentifier of textHere) {
          const { line } = sourceFile.getLineAndCharacterOfPosition(node.getStart());
          findings.push({ bgIdentifiers: effectiveBg, line: line + 1, textIdentifier });
        }
        bgAcrossBranches.push(...bgHere);
      }
      if (bgAcrossBranches.length > 0) nextEnclosingBg = [...new Set(bgAcrossBranches)];
    }
    ts.forEachChild(node, (child) => visit(child, nextEnclosingBg));
  }
  visit(sourceFile, []);
  return findings;
}

function collectComponentFiles(dir: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (dir === WEB_SRC_ROOT && entry.name === "theme") continue;
    const entryPath = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...collectComponentFiles(entryPath));
      continue;
    }
    if (entry.name.endsWith(".tsx") && !entry.name.endsWith(".test.tsx")) files.push(entryPath);
  }
  return files;
}

const componentFiles = collectComponentFiles(WEB_SRC_ROOT);

interface Offender {
  readonly bgIdentifier: string;
  readonly file: string;
  readonly line: number;
  readonly textIdentifier: string;
}

const offenders: Offender[] = [];
const unresolved: { file: string; line: number; textIdentifier: string }[] = [];

for (const file of componentFiles) {
  const relativePath = file.slice(WEB_SRC_ROOT.length + 1);
  const source = readFileSync(file, "utf8");
  for (const finding of scanFile(file, source)) {
    if (finding.bgIdentifiers.length === 0) {
      unresolved.push({
        file: relativePath,
        line: finding.line,
        textIdentifier: finding.textIdentifier,
      });
      continue;
    }
    const text = textComposites.get(finding.textIdentifier);
    if (text === undefined) continue;
    for (const bgIdentifier of finding.bgIdentifiers) {
      if (EXEMPT_PAIRS.has(`${finding.textIdentifier}|${bgIdentifier}`)) continue;
      const bg = bgComposites.get(bgIdentifier);
      if (bg === undefined) continue;
      const key = `${text.light}|${text.dark}|${bg.light}|${bg.dark}`;
      if (!registeredPairs.has(key)) {
        offenders.push({
          bgIdentifier,
          file: relativePath,
          line: finding.line,
          textIdentifier: finding.textIdentifier,
        });
      }
    }
  }
}

describe("every text-on-background composition with a statically resolvable background is registered", () => {
  test("scans a non-trivial number of components", () => {
    expect(componentFiles.length).toBeGreaterThan(20);
  });
  test("no component composes a text role onto a background role the contrast table doesn't cover", () => {
    expect(offenders).toEqual([]);
  });
});
