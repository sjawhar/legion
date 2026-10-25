/** A socket already open, as the browser's is once it connects; the test plays the server.
 *  `sent` keeps every frame it is sent, and `receive` delivers a frame from the server. */
export class OpenSocket extends EventTarget {
  static opened: OpenSocket[] = [];
  binaryType = "blob";
  readonly readyState: number = 1;
  readonly sent: Uint8Array[] = [];

  constructor(readonly url: string) {
    super();
    OpenSocket.opened.push(this);
    queueMicrotask(() => this.dispatchEvent(new Event("open")));
  }

  close(): void {}

  send(data: Uint8Array): void {
    this.sent.push(new Uint8Array(data));
  }

  receive(data: Uint8Array): void {
    this.dispatchEvent(new MessageEvent("message", { data: data.slice().buffer }));
  }
}

/** Replaces `WebSocket` with OpenSocket, forgetting the sockets opened before, until the returned
 *  restore is called. */
export function installOpenSockets(): () => void {
  const original = globalThis.WebSocket;
  globalThis.WebSocket = OpenSocket as unknown as typeof WebSocket;
  OpenSocket.opened = [];
  return () => {
    globalThis.WebSocket = original;
  };
}
