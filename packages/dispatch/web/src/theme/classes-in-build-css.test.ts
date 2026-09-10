import { beforeAll, describe, expect, test } from "bun:test";
import { execSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import * as classes from "./classes";

/**
 * The exact regression this suite exists to catch: a class string in `classes.ts` that
 * Tailwind's static content scanner cannot see (because it was assembled at runtime instead of
 * written as a literal) silently produces no CSS rule. `no-raw-colors.test.ts` rejects the
 * construction pattern that causes this; this test independently verifies the *outcome* — every
 * class token every `classes.ts` export can produce must have a matching rule in the real,
 * currently-built stylesheet.
 */
const PACKAGE_ROOT = join(import.meta.dir, "..", "..", "..");
const DIST_CSS_DIR = join(import.meta.dir, "..", "..", "dist", "assets");

function collectClassTokens(): Set<string> {
  const tokens = new Set<string>();
  for (const value of Object.values(classes)) {
    if (typeof value === "string") {
      for (const token of value.split(/\s+/)) {
        if (token.length > 0) {
          tokens.add(token);
        }
      }
    } else if (typeof value === "object" && value !== null) {
      for (const nested of Object.values(value)) {
        if (typeof nested === "string") {
          for (const token of nested.split(/\s+/)) {
            if (token.length > 0) {
              tokens.add(token);
            }
          }
        }
      }
    }
  }
  // `inputClasses(recessed)` returns one of two literal strings chosen at runtime — both are
  // static text in classes.ts (Tailwind's scanner sees both), but neither is reachable by
  // iterating `classes`'s own exports, since the function itself is the export.
  for (const recessed of [true, false]) {
    for (const token of classes.inputClasses(recessed).split(/\s+/)) {
      if (token.length > 0) {
        tokens.add(token);
      }
    }
  }
  return tokens;
}

/** Tailwind escapes `:` and `/` (and a few other CSS-special characters) in generated selectors
 * with a backslash, e.g. `dark:bg-slate-950` becomes the selector `.dark\:bg-slate-950`. */
function toEscapedSelectorFragment(token: string): string {
  return token.replace(/[:/]/g, "\\$&");
}

describe("every classes.ts token exists in the built CSS", () => {
  let css: string;

  beforeAll(() => {
    execSync("bun run build:web", { cwd: PACKAGE_ROOT, stdio: "pipe" });
    const cssFiles = readdirSync(DIST_CSS_DIR).filter((name) => name.endsWith(".css"));
    css = cssFiles.map((name) => readFileSync(join(DIST_CSS_DIR, name), "utf8")).join("\n");
  }, 60_000);

  const tokens = [...collectClassTokens()].sort();

  test("collects a non-trivial number of tokens", () => {
    // A regression guard on the collection itself: if this collapsed to 0, every check below
    // would vacuously pass without covering anything.
    expect(tokens.length).toBeGreaterThan(50);
  });

  test.each(
    tokens.map((token) => [token] as const)
  )('"%s" has a rule in the built stylesheet', (token) => {
    const fragment = toEscapedSelectorFragment(token);
    expect(css.includes(fragment)).toBe(true);
  });
});
