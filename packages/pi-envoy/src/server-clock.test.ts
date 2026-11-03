import { expect, test } from "bun:test";
import { ServerClock } from "./server-clock";

const NOW = Date.parse("2026-10-31T11:12:18Z");
const BEHIND = 4 * 3_600_000 + 45 * 60_000;

/** A listener reply carrying `date` as its `Date` header, or none. */
function reply(date: string | undefined): Response {
  return new Response("{}", { headers: date === undefined ? {} : { date } });
}

// Each card states how far this machine's clock runs from the listener's (`./card-age`), and that
// figure comes from the `Date` of the listener's replies through the client's fetch.
test("a listener reply's Date says how far this machine's clock runs ahead of the listener's", async () => {
  const clock = new ServerClock(() => NOW);
  const fetch = clock.observing(async () => reply(new Date(NOW - BEHIND).toUTCString()));

  await fetch("http://listener/v1/interests/subscribe");

  expect(clock.offset()).toBe(BEHIND);
});

test("a reply with no Date, or one that is no time, keeps the last measurement", async () => {
  const dates = [new Date(NOW + BEHIND).toUTCString(), undefined, "not a date"];
  const clock = new ServerClock(() => NOW);
  const fetch = clock.observing(async () => reply(dates.shift()));

  expect(clock.offset()).toBeUndefined();
  for (let each = 0; each < 3; each += 1) await fetch("http://listener/healthz");

  expect(clock.offset()).toBe(-BEHIND);
});
