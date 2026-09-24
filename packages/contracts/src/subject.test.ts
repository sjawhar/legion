import { expect, test } from "bun:test";
import { legionNoticeSubject } from "./subject";

test("builds the persisted Go Legion notice topic from project and issue", () => {
  expect(legionNoticeSubject("omp", "LEGION-208")).toBe("notifications.legion.omp.LEGION-208");
});
