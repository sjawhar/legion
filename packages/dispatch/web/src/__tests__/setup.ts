import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { notifyManager } from "@tanstack/react-query";
import serverBlockSchema from "../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { BlockSchema } from "../api/types";
import { blockSchemaCache } from "../features/doc/schema";

GlobalRegistrator.register();

// Unit tests have no Dispatch server to answer GET /api/v1/schema/blocks, so the session cache
// starts already holding the checked-in server schema: every MarkdownBody, ask card, and
// document surface renders through the real schema without a network request, and a test that
// wants a different schema replaces it through its own `api.getBlockSchema` spy before the
// first render in its file.
void blockSchemaCache.load(async () => serverBlockSchema as unknown as BlockSchema);

class WebSocketStub {
  binaryType = "arraybuffer";
  identifier = 0;
  readyState = 0;

  addEventListener(..._args: unknown[]): void {}

  close(): void {
    this.readyState = 3;
  }

  removeEventListener(..._args: unknown[]): void {}

  send(..._args: unknown[]): void {}
}

globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;

// A failing assertion prints the values it compared, and happy-dom's DOM nodes print as their
// whole object graph - megabytes for one element, which buries or replaces the failure. Nodes
// print as their own markup instead. Bun asks a value for this only when a single-value matcher
// reports (`toBe`, `toBeNull`, `toBeUndefined`, `toContain`); the diff matchers
// (`toEqual`, `toStrictEqual`, `toMatchObject`, `toHaveBeenCalledWith`) and snapshots walk the
// object themselves and ignore it, so compare a node with `toBe` or map it to a string first.
Object.defineProperty(Node.prototype, Bun.inspect.custom, {
  configurable: true,
  value(this: Node): string {
    // A document has no markup and an empty `textContent`, so the node name is what is left to
    // print for one.
    return this instanceof Element
      ? this.outerHTML
      : `${this.nodeName} ${JSON.stringify(this.textContent)}`;
  },
  writable: true,
});

// TanStack batches observer notifications through a scheduler that happy-dom does not
// drive promptly; flushing synchronously keeps query updates inside React's act() scope
// so assertions after setQueryData do not wait on a stalled timer.
notifyManager.setScheduler((callback) => callback());
