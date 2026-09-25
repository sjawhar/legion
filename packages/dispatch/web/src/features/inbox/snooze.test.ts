import { expect, test } from "bun:test";

import { isIndefinite, isSnoozed, SNOOZE_INDEFINITE, SNOOZE_PRESETS } from "./snooze";

function preset(id: string) {
  const found = SNOOZE_PRESETS.find((option) => option.id === id);
  if (found === undefined) throw new Error(`no preset ${id}`);
  return found;
}

test("every preset returns a moment the server will accept: ahead of the pick", () => {
  const now = new Date("2026-09-25T14:30:00");
  for (const option of SNOOZE_PRESETS) {
    expect(Date.parse(option.until(now))).toBeGreaterThan(now.getTime());
  }
});

test("Later today never returns tomorrow: an evening pick lands at the last minute of today", () => {
  const afternoon = new Date("2026-09-25T14:30:00");
  const threeHoursOn = new Date(preset("later-today").until(afternoon));
  expect(threeHoursOn.getTime()).toBe(afternoon.getTime() + 3 * 60 * 60 * 1000);
  expect(threeHoursOn.getDate()).toBe(25);

  // 22:30 + 3h would be tomorrow's 01:30, which "Later today" does not mean.
  const evening = new Date("2026-09-25T22:30:00");
  const clamped = new Date(preset("later-today").until(evening));
  expect(clamped.getDate()).toBe(25);
  expect([clamped.getHours(), clamped.getMinutes(), clamped.getSeconds()]).toEqual([23, 59, 59]);
  expect(clamped.getTime()).toBeGreaterThan(evening.getTime());

  // The clamp is the last instant of the day, not a fixed hour: a pick at 23:58 must still
  // name a moment ahead of itself, or the server refuses it.
  const almostMidnight = new Date("2026-09-25T23:58:00");
  const squeezed = new Date(preset("later-today").until(almostMidnight));
  expect(squeezed.getTime()).toBeGreaterThan(almostMidnight.getTime());
  expect(squeezed.getDate()).toBe(25);
});

test("Later today gives the next morning once today has too little left to name", () => {
  // Three seconds of today remain: the clamp would land inside the request's own latency, and
  // the server takes only a moment still ahead of it, so the pick is the tomorrow moment.
  const nearlyMidnight = new Date("2026-09-25T23:59:57");
  const at = new Date(preset("later-today").until(nearlyMidnight));
  expect(at.getTime()).toBeGreaterThan(nearlyMidnight.getTime());
  expect([at.getDate(), at.getHours()]).toEqual([26, 9]);
});

test("Tomorrow and Next week return in the reader's morning, Next week on the coming Monday", () => {
  // A Friday afternoon.
  const friday = new Date("2026-09-25T14:30:00");
  const tomorrow = new Date(preset("tomorrow").until(friday));
  expect([tomorrow.getFullYear(), tomorrow.getMonth(), tomorrow.getDate()]).toEqual([2026, 8, 26]);
  expect(tomorrow.getHours()).toBe(9);

  const nextWeek = new Date(preset("next-week").until(friday));
  expect(nextWeek.getDay()).toBe(1);
  expect(nextWeek.getDate()).toBe(28);
  expect(nextWeek.getHours()).toBe(9);
});

test("Next week on a Monday is the Monday after, never later the same day", () => {
  const monday = new Date("2026-09-28T08:00:00");
  const nextWeek = new Date(preset("next-week").until(monday));
  expect(nextWeek.getDay()).toBe(1);
  expect(nextWeek.getDate()).toBe(5);
  expect(nextWeek.getMonth()).toBe(9);
});

test("a snooze counts only while its moment is ahead", () => {
  const now = Date.parse("2026-09-25T12:00:00Z");
  expect(isSnoozed({ snoozed_until: "2026-09-25T18:00:00Z" }, now)).toBe(true);
  expect(isSnoozed({ snoozed_until: "2026-09-25T06:00:00Z" }, now)).toBe(false);
  expect(isSnoozed({ snoozed_until: null }, now)).toBe(false);
  // An issue header's open ask carries no snooze at all.
  expect(isSnoozed({}, now)).toBe(false);
});

test("only the sentinel reads as a snooze the reader ends themselves", () => {
  expect(isIndefinite(SNOOZE_INDEFINITE)).toBe(true);
  expect(isIndefinite(preset("next-week").until(new Date("2026-09-25T14:30:00")))).toBe(false);
});
