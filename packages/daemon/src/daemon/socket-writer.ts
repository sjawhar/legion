/** The subset of a Bun `Socket` a line writer needs: `write` returns the byte count the kernel
 * buffer accepted (`-1` once the socket is closed), which is less than the input whenever the
 * buffer is full. */
export interface WritableSocket {
  write(data: Uint8Array): number;
}

export interface SocketLineWriter {
  /** Queues `line` plus a trailing newline and writes as much as the socket accepts now. */
  write(line: string): void;
  /** Resumes the queue; wire it to the socket's `drain` handler. */
  drain(): void;
  /** Drops everything still queued; wire it to the socket's `close` handler. */
  clear(): void;
}

const encoder = new TextEncoder();

/**
 * Writes newline-delimited frames to a Bun socket without losing bytes to backpressure. A bare
 * `socket.write(line)` silently drops whatever the kernel buffer did not take, and a protocol v2
 * `rpc_chunk` line is ~350 KiB against a unix-socket buffer of roughly 200 KiB, so every chunked
 * `agent_end` used to reach the daemon truncated and unparseable. Bytes the socket refuses stay
 * queued in order and go out on the next `drain`.
 */
export function createSocketLineWriter(socket: WritableSocket): SocketLineWriter {
  const queue: Uint8Array[] = [];
  let headOffset = 0;

  const flush = (): void => {
    while (queue.length > 0) {
      const head = queue[0];
      const remaining = headOffset === 0 ? head : head.subarray(headOffset);
      const written = socket.write(remaining);
      if (written < 0) {
        queue.length = 0;
        headOffset = 0;
        return;
      }
      if (written < remaining.byteLength) {
        headOffset += written;
        return;
      }
      queue.shift();
      headOffset = 0;
    }
  };

  return {
    write(line) {
      queue.push(encoder.encode(`${line}\n`));
      flush();
    },
    drain: flush,
    clear() {
      queue.length = 0;
      headOffset = 0;
    },
  };
}
