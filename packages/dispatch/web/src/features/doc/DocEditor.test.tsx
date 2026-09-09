import { expect, spyOn, test } from "bun:test";
import { HocuspocusProvider } from "@hocuspocus/provider";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { Artifact } from "../../api/types";
import { DocEditor } from "./DocEditor";

const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "b52b3d6d-eace-486c-98f9-4963e25d5bf8",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

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

test("DocEditor destroys its room provider when it unmounts", () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const destroy = spyOn(HocuspocusProvider.prototype, "destroy");

  try {
    const { unmount } = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifact} isClosed={false} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    unmount();
    expect(destroy).toHaveBeenCalledTimes(1);
  } finally {
    destroy.mockRestore();
    globalThis.WebSocket = originalWebSocket;
  }
});

test("DocEditor makes a closed document visibly read-only", () => {
  const originalWebSocket = globalThis.WebSocket;
  globalThis.WebSocket = WebSocketStub as unknown as typeof WebSocket;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  try {
    const { container, unmount } = render(
      <QueryClientProvider client={queryClient}>
        <DocEditor artifact={artifact} isClosed={true} user={{ login: "alice" }} />
      </QueryClientProvider>
    );
    expect(screen.getByText("This issue is closed. Its document is read-only.")).not.toBeNull();
    expect(container.querySelector(".cm-content")?.getAttribute("contenteditable")).toBe("false");
    unmount();
  } finally {
    globalThis.WebSocket = originalWebSocket;
  }
});
