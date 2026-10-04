/**
 * A v4 UUID from `crypto.getRandomValues`, which every browsing context defines. The block-id default
 * generator uses it rather than `crypto.randomUUID`, which exists only in a secure context (HTTPS or a
 * loopback origin): a document opened over plain HTTP by a LAN address or a host name could otherwise
 * mint no id (LEGION-461). Same construction as the Dispatch SPA's `newSendKey`
 * (`packages/dispatch/web/src/features/agents/broadcast-composition.ts`).
 */
export function uuidV4(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
