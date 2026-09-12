export interface LineReader {
  /** Feeds one socket read; fires `onLine` for every complete line it completes. */
  push(chunk: Uint8Array): void;
  /** Drops any partial line and decoder state; wire it to the socket's `close` handler when the
   * same reader outlives the connection (a tail left by one peer must not prefix the next). */
  reset(): void;
}

/**
 * Splits a byte stream into trimmed, non-empty newline-delimited lines — the read side of the
 * NDJSON framing `createSocketLineWriter` produces. Decodes through a streaming `TextDecoder`, so
 * a multi-byte UTF-8 character that a socket read splits in two is reassembled instead of
 * becoming U+FFFD in the frame; a per-chunk `toString("utf8")` cannot tell a partial character
 * from an invalid one.
 */
export function createLineReader(onLine: (line: string) => void): LineReader {
  let decoder = new TextDecoder();
  let buffer = "";
  return {
    push(chunk) {
      buffer += decoder.decode(chunk, { stream: true });
      let index = buffer.indexOf("\n");
      while (index !== -1) {
        const line = buffer.slice(0, index).trim();
        buffer = buffer.slice(index + 1);
        if (line) onLine(line);
        index = buffer.indexOf("\n");
      }
    },
    reset() {
      decoder = new TextDecoder();
      buffer = "";
    },
  };
}
