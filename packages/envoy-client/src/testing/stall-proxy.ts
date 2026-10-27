import { createConnection, createServer, type Socket } from "node:net";

/** One connection a client opened through the proxy. */
export interface ProxiedConnection {
  /** When the proxy accepted it (`Date.now()`). */
  readonly acceptedAt: number;
  /** When the proxy first forwarded client bytes holding a `SUB ` line: the client subscribed
   *  through it. Undefined until then, and for good on a connection stalled from its first byte. */
  readonly subscribedAt: number | undefined;
}

/**
 * A loopback TCP proxy to one upstream that can freeze a path the way a dead network route does:
 * packets stop, and neither side is closed.
 */
export interface StallProxy {
  /** `nats://127.0.0.1:<port>`, the address clients dial. */
  readonly url: string;
  /** Every connection accepted so far, oldest first. */
  readonly connections: readonly ProxiedConnection[];
  /**
   * Every connection open now stops forwarding in both directions. Each keeps reading and
   * discarding what arrives on both of its sockets, so no sender ever blocks on a full buffer, and
   * the proxy closes neither side. Connections opened later forward normally. Returns the time of
   * the freeze (`Date.now()`).
   */
  freeze(): number;
  /** The next `count` connections behave frozen from their first byte: read, discarded, never
   *  forwarded, never closed. */
  stallNext(count: number): void;
  /** Destroys every socket and stops listening. */
  close(): Promise<void>;
}

/** Starts a proxy on a free 127.0.0.1 port to `upstream`, `host:port`. */
export async function startStallProxy(upstream: string): Promise<StallProxy> {
  const separator = upstream.lastIndexOf(":");
  const host = upstream.slice(0, separator);
  const port = Number(upstream.slice(separator + 1));
  const connections: { acceptedAt: number; subscribedAt: number | undefined }[] = [];
  const open: { frozen: boolean }[] = [];
  const sockets = new Set<Socket>();
  let stalls = 0;

  const track = (socket: Socket): Socket => {
    sockets.add(socket);
    // A peer's reset is the peer's business; the proxy never reacts to it.
    socket.on("error", () => undefined);
    socket.on("close", () => sockets.delete(socket));
    return socket;
  };

  // allowHalfOpen: a client's FIN is not answered with the proxy's own, so a frozen path never
  // closes from the proxy's side.
  const server = createServer({ allowHalfOpen: true }, (client) => {
    track(client);
    const record = { acceptedAt: Date.now(), subscribedAt: undefined as number | undefined };
    connections.push(record);
    if (stalls > 0) {
      stalls -= 1;
      client.on("data", () => undefined);
      return;
    }
    const path = { frozen: false };
    open.push(path);
    const target = track(createConnection({ host, port, allowHalfOpen: true }));
    client.on("data", (chunk: Buffer) => {
      if (path.frozen) return;
      if (record.subscribedAt === undefined && /(^|\n)SUB /.test(chunk.toString("latin1"))) {
        record.subscribedAt = Date.now();
      }
      target.write(chunk);
    });
    target.on("data", (chunk: Buffer) => {
      if (!path.frozen) client.write(chunk);
    });
    // While the path forwards, an end on one side is passed to the other, as a live route does.
    client.on("close", () => {
      if (!path.frozen) target.destroy();
    });
    target.on("close", () => {
      if (!path.frozen) client.destroy();
    });
  });

  const listening = Promise.withResolvers<void>();
  server.once("error", listening.reject);
  server.listen(0, "127.0.0.1", () => listening.resolve());
  await listening.promise;
  const address = server.address();
  if (address === null || typeof address === "string") {
    server.close();
    throw new Error("the stall proxy has no TCP address");
  }

  return {
    url: `nats://127.0.0.1:${address.port}`,
    connections,
    freeze() {
      for (const path of open.splice(0)) path.frozen = true;
      return Date.now();
    },
    stallNext(count) {
      stalls += count;
    },
    async close() {
      for (const socket of sockets) socket.destroy();
      const closed = Promise.withResolvers<void>();
      server.close(() => closed.resolve());
      await closed.promise;
    },
  };
}
