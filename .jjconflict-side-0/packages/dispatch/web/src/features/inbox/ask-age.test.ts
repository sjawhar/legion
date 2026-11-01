import { expect, test } from "bun:test";

import { formatAskAge } from "./ask-age";

const now = Date.parse("2026-09-12T12:00:00Z");

test("formatAskAge uses minutes below one hour", () => {
  expect(formatAskAge("2026-09-12T11:25:00Z", now)).toBe("35m");
  expect(formatAskAge("2026-09-12T11:01:00Z", now)).toBe("59m");
});

test("formatAskAge changes to hours at one hour and days at two days", () => {
  expect(formatAskAge("2026-09-12T11:00:00Z", now)).toBe("1h");
  expect(formatAskAge("2026-09-10T12:01:00Z", now)).toBe("47h");
  expect(formatAskAge("2026-09-10T12:00:00Z", now)).toBe("2d");
});
