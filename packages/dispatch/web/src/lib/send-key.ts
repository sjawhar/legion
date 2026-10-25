/**
 * A v4 UUID that names one send to Dispatch (an `idempotency_key`), built from
 * `crypto.getRandomValues`. `crypto.randomUUID` exists only in a secure context, and Dispatch
 * opened over plain HTTP by a non-loopback name is not one, so a key built from it would throw
 * there on the first send.
 */
export function newSendKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
