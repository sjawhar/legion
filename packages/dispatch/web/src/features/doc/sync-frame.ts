import { type Decoder, readVarUint } from "lib0/decoding";

/**
 * Reads one length-prefixed field of a Hocuspocus frame as a view of the frame, and throws when
 * the length claims more bytes than the frame has left. A frame is often a view into a larger
 * buffer, so the bound is the frame's own length, never its buffer's.
 */
export function readField(decoder: Decoder): Uint8Array {
  const length = readVarUint(decoder);
  const start = decoder.pos;
  decoder.pos += length;
  if (decoder.pos > decoder.arr.length) {
    throw new RangeError("truncated document sync frame");
  }
  return decoder.arr.subarray(start, decoder.pos);
}
