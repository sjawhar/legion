import { expect, test } from "bun:test";

// This row holds `jest-dom-matchers.ts` in place, which says why the preload needs it: without
// it, a failing `toHaveAttribute` under bun:test reports only
// `Expected this to be instanceof ExpectMatcherUtils` - not the attribute, not the value wanted,
// not the value found - because jest-dom destructures the methods of the `utils` bun:test hands
// `expect.extend`, which unbinds them from the object they read.
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
