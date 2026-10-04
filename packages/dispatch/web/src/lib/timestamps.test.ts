import { expect, test } from "bun:test";

import { compareTimestamps } from "./timestamps";

test("two times in one second order by time, though their strings sort the other way", () => {
  const earlier = "2026-10-02T00:00:00.12Z";
  const later = "2026-10-02T00:00:00.123456Z";
  // The trap the server's format sets: trailing zeros dropped, so the strings differ in width.
  expect(earlier.localeCompare(later)).toBeGreaterThan(0);

  expect(compareTimestamps(earlier, later)).toBeLessThan(0);
  expect(compareTimestamps(later, earlier)).toBeGreaterThan(0);
  expect(compareTimestamps("2026-10-02T00:00:00Z", "2026-10-02T00:00:00.000001Z")).toBeLessThan(0);
});

test("times that differ only below a millisecond keep their order", () => {
  const earlier = "2026-10-02T00:00:00.123456Z";
  const later = "2026-10-02T00:00:00.1235Z";
  // `Date.parse` keeps milliseconds, so it reads both as …00.123.
  expect(Date.parse(earlier)).toBe(Date.parse(later));

  expect(compareTimestamps(earlier, later)).toBeLessThan(0);
  expect(compareTimestamps(later, earlier)).toBeGreaterThan(0);
  expect(
    compareTimestamps("2026-10-02T00:00:00.123001Z", "2026-10-02T00:00:00.123Z")
  ).toBeGreaterThan(0);
});

test("one instant written two ways compares equal, offsets included", () => {
  expect(compareTimestamps("2026-10-02T00:00:00.12Z", "2026-10-02T00:00:00.120Z")).toBe(0);
  expect(compareTimestamps("2026-10-02T02:00:00.5+02:00", "2026-10-02T00:00:00.5Z")).toBe(0);
  expect(compareTimestamps("2026-10-02T01:00:00.4+01:00", "2026-10-02T00:00:00.5Z")).toBeLessThan(
    0
  );
});

test("a value that is not an RFC 3339 timestamp is refused, naming it", () => {
  expect(() => compareTimestamps("yesterday", "2026-10-02T00:00:00Z")).toThrow('"yesterday"');
  expect(() => compareTimestamps("2026-10-02T00:00:00Z", "2026-13-02T00:00:00Z")).toThrow(
    '"2026-13-02T00:00:00Z"'
  );
});
