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
void blockSchemaCache.load(async () => serverBlockSchema as BlockSchema);

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

// TanStack batches observer notifications through a scheduler that happy-dom does not
// drive promptly; flushing synchronously keeps query updates inside React's act() scope
// so assertions after setQueryData do not wait on a stalled timer.
notifyManager.setScheduler((callback) => callback());
