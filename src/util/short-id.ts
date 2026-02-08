import { randomUUID } from "node:crypto";

// Base62 alphabet (0-9, A-Z, a-z)
const ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
const BASE = 62n;

/**
 * Encode an integer to base62.
 */
export function encodeBase62(num: bigint): string {
  if (num === 0n) {
    return ALPHABET[0];
  }

  const result: string[] = [];
  let n = num;
  while (n > 0n) {
    const rem = n % BASE;
    result.push(ALPHABET[Number(rem)]);
    n = n / BASE;
  }

  return result.reverse().join("");
}

/**
 * Decode a base62 string to an integer.
 */
export function decodeBase62(s: string): bigint {
  let num = 0n;
  for (const char of s) {
    const index = ALPHABET.indexOf(char);
    if (index === -1) {
      throw new Error(`Invalid character in base62 string: ${char}`);
    }
    num = num * BASE + BigInt(index);
  }
  return num;
}

/**
 * Convert a UUID string to a short base62 ID.
 *
 * Takes first `length` characters of the base62 encoding.
 * 8 chars of base62 = ~47 bits of entropy (62^8 ≈ 2^47).
 */
export function uuidToShort(uuidStr: string, length: number = 8): string {
  // Remove hyphens and convert to bigint
  const clean = uuidStr.replace(/-/g, "");
  const num = BigInt(`0x${clean}`);
  const encoded = encodeBase62(num);
  return encoded.slice(0, length);
}

/**
 * Generate a new short ID.
 */
export function generateShortId(length: number = 8): string {
  return uuidToShort(randomUUID(), length);
}
