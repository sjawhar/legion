import { expect, test } from "bun:test";

import { dayText, searchFilterChips, searchRequest } from "./search-filters";

test("a chip reads back the filter in the words the person chose", () => {
  const chips = searchFilterChips({
    author: "alice",
    project: "INFRA",
    status: "in_progress",
    updatedSince: "2026-11-01",
  });

  expect(chips.map(({ name }) => name)).toEqual(["project", "status", "author", "updatedSince"]);
  expect(chips[0]?.label).toBe("Project INFRA");
  expect(chips[1]?.label).toBe("Status In progress");
  expect(chips[2]?.label).toBe("Author alice");
  // The day is the reader's own month name and day number, never the `2026-11-01` of the wire.
  expect(chips[3]?.label).toStartWith("Updated since ");
  expect(chips[3]?.label).toContain("November");
  expect(chips[3]?.label).toContain("1");
  expect(chips[3]?.label).not.toContain("2026-11-01");
});

test("nothing chosen is no chip at all", () => {
  expect(searchFilterChips({})).toEqual([]);
});

test("a day of another year carries that year, and one of this year does not", () => {
  const today = new Date(2026, 10, 6);

  expect(dayText("2026-11-01", today)).not.toContain("2026");
  expect(dayText("2025-11-01", today)).toContain("2025");
});

test("a value the date control cannot have written reads as it stands", () => {
  expect(dayText("whenever")).toBe("whenever");
});

test("the request carries each chosen filter under the route's own name", () => {
  expect(
    searchRequest({ author: "alice", project: "INFRA", status: "needs_review" })
  ).toMatchObject({
    author: "alice",
    project: "INFRA",
    status: "needs_review",
  });
  expect(searchRequest({})).toEqual({});
});

test("the day is sent as the instant its midnight began where the reader is", () => {
  const { updated_since: since } = searchRequest({ updatedSince: "2026-11-01" });
  const sent = new Date(since ?? "");

  expect(since).toEndWith("Z");
  expect([sent.getFullYear(), sent.getMonth() + 1, sent.getDate(), sent.getHours()]).toEqual([
    2026, 11, 1, 0,
  ]);
});

// The page's project is context for a bare issue number, never a narrowing: it rides under its
// own parameter and sets no chip, so a search from a project page is the search a person would
// have got from anywhere.
test("the page's project rides as context and narrows nothing", () => {
  expect(searchRequest({}, "INFRA")).toEqual({ number_project: "INFRA" });
  expect(searchRequest({}, undefined)).toEqual({});
  expect(searchRequest({ project: "LINUX" }, "INFRA")).toEqual({
    number_project: "INFRA",
    project: "LINUX",
  });
});
