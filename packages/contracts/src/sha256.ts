// SHA-256 (FIPS 180-4) over bytes, synchronous and without `node:crypto`: the subject helpers that
// use it are bundled into the Dispatch dashboard's browser build too, where the only digest is Web
// Crypto's, which answers a Promise.

// Plain arrays rather than typed ones: a bundler drops an unused array literal, but not a call
// that makes a typed array, from a bundle that never digests.
const ROUND_CONSTANTS = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
];

const INITIAL_HASH = [
  0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
];

function rotateRight(word: number, bits: number): number {
  return (word >>> bits) | (word << (32 - bits));
}

/** The SHA-256 digest of `bytes`, as 64 lowercase hex digits. */
export function sha256Hex(bytes: Uint8Array): string {
  // The message, a 1 bit, zeros, and the message's length in bits as a 64-bit big-endian number,
  // padded to whole 64-byte blocks.
  const padded = new Uint8Array(Math.ceil((bytes.length + 9) / 64) * 64);
  padded.set(bytes);
  padded[bytes.length] = 0x80;
  const view = new DataView(padded.buffer);
  view.setUint32(padded.length - 8, Math.floor(bytes.length / 0x20000000));
  view.setUint32(padded.length - 4, (bytes.length << 3) >>> 0);

  const hash = Uint32Array.from(INITIAL_HASH);
  const schedule = new Uint32Array(64);
  for (let block = 0; block < padded.length; block += 64) {
    for (let i = 0; i < 16; i++) {
      schedule[i] = view.getUint32(block + i * 4);
    }
    for (let i = 16; i < 64; i++) {
      const early = schedule[i - 15] as number;
      const late = schedule[i - 2] as number;
      const sigma0 = rotateRight(early, 7) ^ rotateRight(early, 18) ^ (early >>> 3);
      const sigma1 = rotateRight(late, 17) ^ rotateRight(late, 19) ^ (late >>> 10);
      schedule[i] = (schedule[i - 16] as number) + sigma0 + (schedule[i - 7] as number) + sigma1;
    }
    let [a, b, c, d, e, f, g, h] = hash as unknown as [
      number,
      number,
      number,
      number,
      number,
      number,
      number,
      number,
    ];
    for (let i = 0; i < 64; i++) {
      const sum1 = rotateRight(e, 6) ^ rotateRight(e, 11) ^ rotateRight(e, 25);
      const choose = (e & f) ^ (~e & g);
      const temp1 =
        (h + sum1 + choose + (ROUND_CONSTANTS[i] as number) + (schedule[i] as number)) >>> 0;
      const sum0 = rotateRight(a, 2) ^ rotateRight(a, 13) ^ rotateRight(a, 22);
      const majority = (a & b) ^ (a & c) ^ (b & c);
      const temp2 = (sum0 + majority) >>> 0;
      h = g;
      g = f;
      f = e;
      e = (d + temp1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (temp1 + temp2) >>> 0;
    }
    // A Uint32Array keeps each sum modulo 2^32.
    hash[0] = (hash[0] as number) + a;
    hash[1] = (hash[1] as number) + b;
    hash[2] = (hash[2] as number) + c;
    hash[3] = (hash[3] as number) + d;
    hash[4] = (hash[4] as number) + e;
    hash[5] = (hash[5] as number) + f;
    hash[6] = (hash[6] as number) + g;
    hash[7] = (hash[7] as number) + h;
  }
  return Array.from(hash, (word) => word.toString(16).padStart(8, "0")).join("");
}
