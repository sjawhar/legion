/**
 * The server writes every time as RFC 3339 through Go's `time.RFC3339Nano`, which drops trailing
 * fractional zeros, so its timestamps are not fixed-width and do not sort as text: `…00.12Z` is the
 * earlier time and sorts after `…00.123456Z`. Its times also carry microseconds, which `Date.parse`
 * drops. So a timestamp is compared as its whole second, parsed, and then its fraction, digit by
 * digit.
 */
const RFC3339 = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/;

interface Instant {
  /** The digits after the decimal point, as written; empty for a whole second. */
  fraction: string;
  /** The whole second, in epoch milliseconds. */
  second: number;
}

function instant(value: string): Instant {
  const match = RFC3339.exec(value);
  const second = match === null ? Number.NaN : Date.parse(`${match[1]}${match[3]}`);
  if (match === null || Number.isNaN(second)) {
    throw new Error(`not an RFC 3339 timestamp: ${JSON.stringify(value)}`);
  }
  return { fraction: match[2] ?? "", second };
}

/**
 * Orders two timestamps by the instants they name: negative when `left` is earlier, positive when
 * it is later, and zero for one instant however it is written. It accepts the form the server's
 * `time.RFC3339Nano` writes and throws on anything else, since an order built around such a value
 * would be a guess. A value must match the hand-written `RFC3339` pattern above, and `Date.parse`
 * must then read its whole second. So `2023-01-01`, which `Date.parse` reads, throws, as do a
 * lowercase `t` or `z` and the leap second `23:59:60`, both of which RFC 3339 allows, while an
 * impossible date such as `2026-02-30T00:00:00Z` passes both checks and is ordered as 2 March.
 */
export function compareTimestamps(left: string, right: string): number {
  const a = instant(left);
  const b = instant(right);
  if (a.second !== b.second) {
    return a.second - b.second;
  }
  // Padded to one width, the fractions are digit strings whose text order is their numeric order.
  const width = Math.max(a.fraction.length, b.fraction.length);
  const leftFraction = a.fraction.padEnd(width, "0");
  const rightFraction = b.fraction.padEnd(width, "0");
  if (leftFraction === rightFraction) {
    return 0;
  }
  return leftFraction < rightFraction ? -1 : 1;
}
