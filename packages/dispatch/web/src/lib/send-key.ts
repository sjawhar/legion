/** The shape of a key this module mints: a hyphenated UUID whose version nibble is 7. */
const V7_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * A v7 UUID that names one write to Dispatch (its `Idempotency-Key`), built from
 * `crypto.getRandomValues`. `crypto.randomUUID` exists only in a secure context, and Dispatch
 * opened over plain HTTP by a non-loopback name is not one, so a key built from it would throw
 * there on the first write.
 *
 * Its first six bytes are the millisecond it was minted, which `keyMintedAt` reads back: Dispatch
 * keeps a key only `IDEMPOTENCY_KEY_TTL_MS`, so a page still retrying after that would send the
 * same intent under a key Dispatch has forgotten - a second write. The key carries its own mint
 * time because the four places that re-press a write under its existing key would each restart a
 * time stamped on the write record instead.
 */
export function newSendKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  let minted = Date.now();
  for (let i = 5; i >= 0; i--) {
    bytes[i] = minted & 0xff;
    minted = Math.floor(minted / 256);
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x70;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/**
 * The millisecond `key` was minted, from the first six bytes of a key `newSendKey` made, and
 * `undefined` for a key that carries no such time: every key the shipping release minted, and
 * every key from another source.
 */
export function keyMintedAt(key: string): number | undefined {
  if (!V7_KEY.test(key)) return undefined;
  return Number.parseInt(`${key.slice(0, 8)}${key.slice(9, 13)}`, 16);
}
