import assert from "node:assert/strict";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import * as classes from "./classes";

/**
 * The exact regression this check exists to catch: a class string in `classes.ts` that
 * Tailwind's static content scanner cannot see (because it was assembled at runtime instead of
 * written as a literal) silently produces no CSS rule. `no-raw-colors.test.ts` rejects the
 * construction pattern that causes this; this script independently verifies the *outcome* —
 * every class token every `classes.ts` export can produce must have a matching rule in the real,
 * currently-built stylesheet.
 */
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

function main(): void {
  if (!existsSync(DIST_CSS_DIR)) {
    throw new Error(
      `No built CSS assets found at ${DIST_CSS_DIR}. Run "bun run build:web" before "bun run check:classes".`
    );
  }

  const cssFiles = readdirSync(DIST_CSS_DIR).filter((name) => name.endsWith(".css"));
  assert.ok(
    cssFiles.length > 0,
    `No built CSS assets found at ${DIST_CSS_DIR}. Run "bun run build:web" before "bun run check:classes".`
  );
  const css = cssFiles.map((name) => readFileSync(join(DIST_CSS_DIR, name), "utf8")).join("\n");
  const tokens = [...collectClassTokens()].sort();
  assert.ok(tokens.length > 50, "Expected classes.ts to expose more than 50 class tokens.");
  for (const token of tokens) {
    assert.ok(
      css.includes(token.replace(/[:/]/g, "\\$&")),
      `"${token}" has no rule in the built stylesheet.`
    );
  }

  console.log(`Checked ${tokens.length} class tokens across ${cssFiles.length} built CSS assets.`);
}

main();
