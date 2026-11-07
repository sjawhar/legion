import { expect, test } from "bun:test";

// Why the suite asserts the DOM with jest-dom rather than with a hand-written read compared by
// `toBe` is written into `setup.ts` and into packages/dispatch/AGENTS.md: the matcher "says what
// it asks for and prints the element it read when it fails". `toHaveAttribute` does neither. Its
// reporting path throws inside bun:test before it builds a message, so a reader of a failing run
// is given `Expected this to be instanceof ExpectMatcherUtils` and nothing else: not the
// attribute, not the value wanted, not the value found - where the
// `expect(el.getAttribute(name)).toBe(value)` it replaced printed the last two. 367 assertions
// were converted to this matcher, so this is what a reader meets whenever one of them fails.
//
// The cause is the matcher context bun:test gives `expect.extend`: jest-dom destructures
// `this.utils`, which unbinds Bun's own methods, and then reaches for `this.equals` and
// `this.utils.stringify`, which that context does not carry. Supplying all three in a wrapper is
// one way to cure it; correcting the two written claims and keeping the attribute family on
// `getAttribute(...)).toBe(...)` is another.
//
// This row reads the message a failing matcher throws, so it needs an assertion that fails:
// `failureOf` runs it and hands back the text, and refuses an assertion that passed, which would
// otherwise leave the row green while proving nothing.

function failureOf(assertion: () => void): string {
  try {
    assertion();
  } catch (error) {
    return (error as Error).message;
  }
  throw new Error("the assertion passed, so there is no failure message to read");
}

test("a failed toHaveAttribute names the attribute and both values", () => {
  const field = document.createElement("input");
  field.setAttribute("aria-activedescendant", "option-actual");

  const message = failureOf(() =>
    expect(field).toHaveAttribute("aria-activedescendant", "option-expected")
  );

  expect(message).toContain("aria-activedescendant");
  expect(message).toContain("option-expected");
  expect(message).toContain("option-actual");
});
