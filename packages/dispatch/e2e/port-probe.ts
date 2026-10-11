import { connect } from "node:net";

// Playwright's own port predicate (playwright@1.63.0 `lib/runner/index.js:958-977`): a port counts
// as used when either `127.0.0.1` or `::1` accepts a connection. A probe that dialled only
// `127.0.0.1` would miss a listener bound on `::1` alone — what a docker-published port binds —
// and leave that case to Playwright's backstop, which names no variable.
export async function isPortUsed(port: number): Promise<boolean> {
  const dial = (host: string) => {
    const { promise, resolve } = Promise.withResolvers<boolean>();
    const connection = connect(port, host)
      .on("error", () => resolve(false))
      .on("connect", () => {
        connection.end();
        resolve(true);
      });
    return promise;
  };
  return (await Promise.all([dial("127.0.0.1"), dial("::1")])).includes(true);
}

/** The `listeners` whose port is taken, as `<port> (<variable>)` items joined for a refusal, or
 *  undefined when every port is free. e2e/playwright.config.ts and e2e/local-instance.ts both
 *  refuse to start on it, each naming its own remedy. */
export async function takenPorts(
  listeners: readonly { readonly port: number; readonly variable: string }[]
): Promise<string | undefined> {
  const used = await Promise.all(listeners.map((listener) => isPortUsed(listener.port)));
  const taken = listeners.filter((_, index) => used[index]);
  if (taken.length === 0) return undefined;
  return taken.map((listener) => `${listener.port} (${listener.variable})`).join(", ");
}
