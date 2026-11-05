import { expect, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";

import { capSearchQuery } from "./query-param";

test("a query under the cap is written as it was typed", () => {
  expect(capSearchQuery("")).toBe("");
  expect(capSearchQuery("two words")).toBe("two words");
  expect(capSearchQuery("x".repeat(SEARCH_QUERY_MAX))).toBe("x".repeat(SEARCH_QUERY_MAX));
});

test("a query over the cap is cut to the cap", () => {
  expect(capSearchQuery("x".repeat(SEARCH_QUERY_MAX + 1))).toBe("x".repeat(SEARCH_QUERY_MAX));
});

test("a cut that would split a surrogate pair drops the lone high surrogate", () => {
  // The emoji starts at the last character the cap allows, so the cut lands inside it: half a
  // code point in the address is what this rule exists to prevent.
  const split = `${"x".repeat(SEARCH_QUERY_MAX - 1)}😀`;
  expect(split.length).toBe(SEARCH_QUERY_MAX + 1);
  expect(capSearchQuery(split)).toBe("x".repeat(SEARCH_QUERY_MAX - 1));

  // A pair that ends exactly on the cap is kept whole.
  const whole = `${"x".repeat(SEARCH_QUERY_MAX - 2)}😀`;
  expect(capSearchQuery(whole)).toBe(whole);
});
