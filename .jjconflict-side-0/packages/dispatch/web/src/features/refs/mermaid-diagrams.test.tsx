import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import type { MermaidDrawing } from "@legion/proof-editor/mermaid";
import * as mermaidRenderer from "@legion/proof-editor/mermaid-render";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";

import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import {
  mermaidDrawsSettled,
  requestMermaidDrawing,
  resetMermaidDiagrams,
} from "./mermaid-diagrams";
import {
  answerMermaidRequest,
  type MermaidReply,
  type MermaidRequest,
} from "./mermaid-render.worker";

function withQueries(children: ReactNode): ReactNode {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

/**
 * Stands in for the page's `Worker`, so a test decides what the worker answers and when. Like the
 * real one it posts `{ ready: true }` on a later task once started, `readyDelayMs` after it (or,
 * while `startFails` is set, fires `error` as a worker whose module cannot load does), then
 * answers each request on a later task with `answer`'s reply: the worker module's own
 * `answerMermaidRequest` by default, run on the test's thread; a reply of `undefined` never
 * arrives, as from a worker stuck in a layout. `sent` records each request's source as it is
 * posted. A terminated one posts nothing more.
 */
class FakeWorker {
  static created: FakeWorker[] = [];
  static answer: (request: MermaidRequest) => MermaidReply | undefined = answerMermaidRequest;
  static startFails = false;
  static readyDelayMs = 0;
  static sent: string[] = [];

  terminated = false;
  private readonly listeners: Record<string, ((event: unknown) => void)[]> = {
    error: [],
    message: [],
  };

  constructor() {
    FakeWorker.created.push(this);
    const fails = FakeWorker.startFails;
    setTimeout(() => {
      if (fails) this.emit("error", new Event("error"));
      else this.emit("message", { data: { ready: true } });
    }, FakeWorker.readyDelayMs);
  }

  addEventListener(type: string, listener: (event: unknown) => void): void {
    this.listeners[type]?.push(listener);
  }

  postMessage(request: MermaidRequest): void {
    FakeWorker.sent.push(request.source);
    setTimeout(() => {
      const reply = FakeWorker.answer(request);
      if (reply !== undefined) this.emit("message", { data: reply });
    }, 0);
  }

  terminate(): void {
    this.terminated = true;
  }

  /** Fires `error`, as a worker whose module fails to load after it started does. */
  fail(): void {
    this.emit("error", new Event("error"));
  }

  private emit(type: string, event: unknown): void {
    if (this.terminated) return;
    for (const listener of this.listeners[type] ?? []) listener(event);
  }
}

// The worker's renderer runs on the test's thread here, under happy-dom, where ELK, which lays out
// flowcharts, cannot be built; so these draw sequence diagrams, and a dangling edge is refused in
// validation, before any layout.
describe("Mermaid diagrams", () => {
  const pageWorker = globalThis.Worker;

  // The worker, the drawn-source cache and a failure are module state that outlives every render,
  // so each case starts from a page that has started and drawn nothing.
  beforeEach(() => {
    FakeWorker.created = [];
    FakeWorker.answer = answerMermaidRequest;
    FakeWorker.startFails = false;
    FakeWorker.readyDelayMs = 0;
    FakeWorker.sent = [];
    globalThis.Worker = FakeWorker as unknown as typeof Worker;
    resetMermaidDiagrams();
  });

  afterEach(() => {
    resetMermaidDiagrams();
    globalThis.Worker = pageWorker;
  });

  const fence = (source: string): string => `\`\`\`mermaid\n${source}\n\`\`\`\n`;
  const figuresIn = (container: HTMLElement): HTMLElement[] =>
    Array.from(container.querySelectorAll<HTMLElement>("figure.dispatch-mermaid"));
  const diagramIn = (figure: Element): Element | null | undefined =>
    figure
      .querySelector(".dispatch-mermaid-preview")
      ?.shadowRoot?.querySelector('svg[role="img"][aria-label="Rendered Mermaid diagram"]');

  test("a mermaid block draws its diagram in the preview's shadow root, its source closed behind Mermaid source", async () => {
    const view = render(
      <MarkdownBody markdown={`Flow:\n\n${fence("sequenceDiagram\n  Alice->>Bob: Hi")}`} />
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const [figure] = figuresIn(view.container);
      const diagram = diagramIn(figure);
      expect(diagram?.textContent).toContain("Alice");
      expect(diagram?.textContent).toContain("Hi");
      expect(figure.querySelector('[role="status"]')).toBeNull();
      const source = figure.querySelector<HTMLDetailsElement>("details.dispatch-mermaid-source");
      expect(source?.open).toBe(false);
      expect(source?.querySelector("summary")?.textContent).toBe("Mermaid source");
      expect(source?.querySelector("pre code")?.textContent).toContain("Alice->>Bob: Hi");
      expect(view.container.textContent).toContain("Flow:");
    } finally {
      view.unmount();
    }
  });

  test("a source the renderer refuses shows the reason and its source open, and no diagram", async () => {
    const view = render(<MarkdownBody markdown={`Broken:\n\n${fence("graph TD\n  A -->")}`} />);
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const [figure] = figuresIn(view.container);
      expect(figure.querySelector('[role="status"]')?.textContent).toMatch(
        /^Unable to render Mermaid diagram: Dangling Mermaid edge/
      );
      expect(figure.querySelector(".dispatch-mermaid-preview")).toBeNull();
      const source = figure.querySelector<HTMLDetailsElement>("details.dispatch-mermaid-source");
      expect(source?.open).toBe(true);
      expect(source?.querySelector("pre code")?.textContent).toContain("A -->");
    } finally {
      view.unmount();
    }
  });

  test("a dispatch reference in a diagram's label is neither linked nor resolved", async () => {
    const view = render(
      withQueries(
        <MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: see dispatch://CORE-1")} />
      )
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const diagram = diagramIn(figuresIn(view.container)[0]);
      expect(diagram?.textContent).toContain("dispatch://CORE-1");
      expect(diagram?.querySelector("a")).toBeNull();
      expect(view.container.querySelector("a")).toBeNull();
      expect(within(view.container).queryAllByRole("link")).toHaveLength(0);
    } finally {
      view.unmount();
    }
  });

  // The live view re-parses a streamed turn on every frame: a diagram whose fence has not closed
  // yet stays code, and the diagrams before it in the same body draw.
  test("a body ending in an unclosed mermaid fence keeps it as code until the fence closes", async () => {
    const done = fence("sequenceDiagram\n  Alice->>Bob: Hi");
    const streaming = "```mermaid\nsequenceDiagram\n  Carol->>Dave: Yo";
    const view = render(<MarkdownBody markdown={`${done}\n${streaming}`} />);
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Alice");
      const open = view.container.querySelectorAll("pre");
      expect(open).toHaveLength(2);
      const code = open[1];
      expect(code.textContent).toContain("Carol->>Dave: Yo");
      expect(code.closest("figure")).toBeNull();
      view.rerender(<MarkdownBody markdown={`${done}\n${streaming}\n\`\`\`\n`} />);
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(2));
      expect(diagramIn(figuresIn(view.container)[1])?.textContent).toContain("Carol");
    } finally {
      view.unmount();
    }
  });

  // A link reference definition is no block of the parse, so the diagram is the document's last
  // block while the source's last line is the definition, not the diagram's closing fence. The
  // fence did close, so the diagram is finished and draws.
  test("a closed mermaid fence followed by a link reference definition draws its diagram", async () => {
    const view = render(
      <MarkdownBody
        markdown={`${fence("sequenceDiagram\n  Alice->>Bob: Hi")}\n[spec]: https://example.com/spec\n`}
      />
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Alice");
    } finally {
      view.unmount();
    }
  });

  // CommonMark closes a fence only with a line of the opener's character, at least as long as the
  // opener. A streamed turn's last line that would close some other fence (three backticks under
  // a four-backtick opener, tildes under a backtick one) leaves the block open, so it stays code.
  test("a body ending in a line that cannot close its mermaid fence keeps it as code", async () => {
    const done = fence("sequenceDiagram\n  Alice->>Bob: Hi");
    for (const streaming of [
      "````mermaid\nsequenceDiagram\n  Carol->>Dave: Yo\n```",
      "```mermaid\nsequenceDiagram\n  Carol->>Dave: Yo\n~~~",
    ]) {
      resetMermaidDiagrams();
      const view = render(<MarkdownBody markdown={`${done}\n${streaming}`} />);
      try {
        await waitFor(() => expect(figuresIn(view.container).length).toBeGreaterThan(0));
        expect(figuresIn(view.container)).toHaveLength(1);
        const code = view.container.querySelectorAll("pre")[1];
        expect(code?.textContent).toContain("Carol->>Dave: Yo");
        expect(code?.closest("figure")).toBeNull();
      } finally {
        view.unmount();
      }
    }
  });

  // A fence opened inside a quote or a list item is still open while its quote or item runs to
  // the end of the body, so a diagram streamed there stays code too; closed inside its container,
  // it draws.
  test("a mermaid fence still open inside a quote or a list item keeps it as code", async () => {
    const done = fence("sequenceDiagram\n  Alice->>Bob: Hi");
    for (const [streaming, closing] of [
      ["> ```mermaid\n> sequenceDiagram\n>   Carol->>Dave: Yo", "\n> ```\n"],
      ["- ```mermaid\n  sequenceDiagram\n    Carol->>Dave: Yo", "\n  ```\n"],
    ] as const) {
      resetMermaidDiagrams();
      const view = render(<MarkdownBody markdown={`${done}\n${streaming}`} />);
      try {
        await waitFor(() => expect(figuresIn(view.container).length).toBeGreaterThan(0));
        expect(figuresIn(view.container)).toHaveLength(1);
        const code = view.container.querySelectorAll("pre")[1];
        expect(code?.textContent).toContain("Carol->>Dave: Yo");
        expect(code?.closest("figure")).toBeNull();
        view.rerender(<MarkdownBody markdown={`${done}\n${streaming}${closing}`} />);
        await waitFor(() => expect(figuresIn(view.container)).toHaveLength(2));
      } finally {
        view.unmount();
      }
    }
  });

  // A line ends at a lone CR too, as micromark reads it, so a body written with CR endings keeps
  // a diagram still being written inside its quote or list item as code.
  test("a mermaid fence still open inside a quote or a list item of a body with lone-CR line ends keeps it as code", async () => {
    const done = "```mermaid\rsequenceDiagram\r  Alice->>Bob: Hi\r```\r";
    for (const [streaming, closing] of [
      ["> ```mermaid\r> sequenceDiagram\r>   Carol->>Dave: Yo", "\r> ```\r"],
      ["- ```mermaid\r  sequenceDiagram\r    Carol->>Dave: Yo", "\r  ```\r"],
    ] as const) {
      resetMermaidDiagrams();
      const view = render(<MarkdownBody markdown={`Lead\r${done}\r${streaming}`} />);
      try {
        await waitFor(() => expect(figuresIn(view.container).length).toBeGreaterThan(0));
        expect(figuresIn(view.container)).toHaveLength(1);
        const code = view.container.querySelectorAll("pre")[1];
        expect(code?.textContent).toContain("Carol->>Dave: Yo");
        expect(code?.closest("figure")).toBeNull();
        view.rerender(<MarkdownBody markdown={`Lead\r${done}\r${streaming}${closing}`} />);
        await waitFor(() => expect(figuresIn(view.container)).toHaveLength(2));
      } finally {
        view.unmount();
      }
    }
  });

  // A code block keeps its source's lone CRs and the renderer splits lines at LF alone, so the
  // source reaches the worker with its line ends read as LF, and the diagram draws, not refused.
  test("a mermaid block of a body with lone-CR line ends draws its diagram", async () => {
    const view = render(
      <MarkdownBody markdown={"Lead\r```mermaid\rsequenceDiagram\r  Alice->>Bob: Hi\r```\r"} />
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Alice");
      expect(view.container.querySelector('[role="status"]')).toBeNull();
    } finally {
      view.unmount();
    }
  });

  // A known `:::callout` is a typed container in a body (the typed-callout case of
  // `MarkdownBody.test.tsx`), unlike an unknown directive, which reads as a paragraph line. So a
  // fence opened inside one is still open while the callout runs to the end of the body: it stays
  // code, and is never sent to the worker, until the fence closes.
  test("a mermaid fence still open inside a typed callout keeps it as code while open and draws once closed", async () => {
    const done = fence("sequenceDiagram\n  Alice->>Bob: Hi");
    const streaming = ':::callout{kind="warning"}\n```mermaid\nsequenceDiagram\n  Carol->>Dave: Yo';
    const view = render(<MarkdownBody markdown={`${done}\n${streaming}`} />);
    try {
      await waitFor(() => expect(figuresIn(view.container).length).toBeGreaterThan(0));
      expect(figuresIn(view.container)).toHaveLength(1);
      const code = view.container.querySelectorAll("pre")[1];
      expect(code?.textContent).toContain("Carol->>Dave: Yo");
      expect(code?.closest('section[data-proof-block-type="callout"]')).not.toBeNull();
      expect(code?.closest("figure")).toBeNull();
      expect(FakeWorker.sent).toEqual(["sequenceDiagram\n  Alice->>Bob: Hi"]);
      view.rerender(<MarkdownBody markdown={`${done}\n${streaming}\n\`\`\`\n:::\n`} />);
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(2));
      expect(diagramIn(figuresIn(view.container)[1])?.textContent).toContain("Carol");
    } finally {
      view.unmount();
    }
  });

  test("a body whose source changes or that unmounts before the worker answers draws only the newest source", async () => {
    // With the engine loaded the body paints in its layout effect, so only the worker is late.
    await warmMarkdownRenderer();
    const view = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: First")} />);
    const gone = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Erin->>Finn: Gone")} />);
    // Each superseded code block stays in an element of its own, the one the re-paint replaced
    // and the one the unmount removed; only the cancel keeps a late draw from wrapping it.
    const first = view.container.querySelector("pre");
    const gonePre = gone.container.querySelector("pre");
    try {
      expect(first?.textContent).toContain("First");
      expect(gonePre?.textContent).toContain("Gone");
      expect(figuresIn(view.container)).toHaveLength(0);
      gone.unmount();
      view.rerender(<MarkdownBody markdown={fence("sequenceDiagram\n  Carol->>Dave: Second")} />);
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Second");
      // The unmount and the re-paint cancelled both earlier draws in one task, and the cancel's
      // microtask dropped both sources before the worker posted `ready`, so neither was ever
      // sent, and nothing drew either code block.
      expect(FakeWorker.sent).toEqual(["sequenceDiagram\n  Carol->>Dave: Second"]);
      expect(first?.closest("figure")).toBeNull();
      expect(gonePre?.closest("figure")).toBeNull();
      expect(FakeWorker.created).toHaveLength(1);
    } finally {
      view.unmount();
    }
  });

  test("a source drawn before draws in the layout effect that paints its body", async () => {
    await warmMarkdownRenderer();
    const source = fence("sequenceDiagram\n  Alice->>Bob: Again");
    const first = render(<MarkdownBody markdown={source} />);
    try {
      await waitFor(() => expect(figuresIn(first.container)).toHaveLength(1));
    } finally {
      first.unmount();
    }
    const again = render(<MarkdownBody markdown={`Again:\n\n${source}`} />);
    try {
      expect(figuresIn(again.container)).toHaveLength(1);
      expect(diagramIn(figuresIn(again.container)[0])?.textContent).toContain("Again");
    } finally {
      again.unmount();
    }
  });

  test("a diagram draws from the worker's answer alone: the page never runs the renderer", async () => {
    const draw = spyOn(mermaidRenderer, "renderMermaidMarkup");
    FakeWorker.answer = ({ id }) => ({
      id,
      markup: '<svg xmlns="http://www.w3.org/2000/svg"><text>From the worker</text></svg>',
    });
    const view = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: Hi")} />);
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toBe("From the worker");
      expect(draw).not.toHaveBeenCalled();
    } finally {
      draw.mockRestore();
      view.unmount();
    }
  });

  test("a source the worker has not drawn within the time limit is refused, and a fresh worker draws the next", async () => {
    resetMermaidDiagrams(50);
    const draw = spyOn(mermaidRenderer, "renderMermaidMarkup");
    FakeWorker.answer = (request) =>
      request.source.includes("Stuck") ? undefined : answerMermaidRequest(request);
    const view = render(
      <MarkdownBody markdown={`Big:\n\n${fence("sequenceDiagram\n  Stuck->>Bob: Hi")}`} />
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const [figure] = figuresIn(view.container);
      expect(figure.querySelector('[role="status"]')?.textContent).toBe(
        "Unable to render Mermaid diagram: it did not finish within 0.05 seconds"
      );
      expect(figure.querySelector(".dispatch-mermaid-preview")).toBeNull();
      const source = figure.querySelector<HTMLDetailsElement>("details.dispatch-mermaid-source");
      expect(source?.open).toBe(true);
      expect(source?.querySelector("pre code")?.textContent).toContain("Stuck->>Bob: Hi");
      // Terminating the worker is what stops a real layout.
      expect(FakeWorker.created.map((worker) => worker.terminated)).toEqual([true]);

      view.rerender(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: After")} />);
      await waitFor(() =>
        expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("After")
      );
      expect(FakeWorker.created.map((worker) => worker.terminated)).toEqual([true, false]);
      // The renderer ran for the next source, inside the stand-in worker; the refused source
      // never reached it, on the page's thread or any other.
      const drawn = draw.mock.calls.map(([drawnSource]) => drawnSource);
      expect(drawn.some((drawnSource) => drawnSource.includes("After"))).toBe(true);
      expect(drawn.some((drawnSource) => drawnSource.includes("Stuck"))).toBe(false);
    } finally {
      draw.mockRestore();
      view.unmount();
    }
  });

  // A worker's chunk can take longer to download and load than the time limit; the limit is for
  // drawing one source, so it starts when the worker posts `ready` and is handed that source.
  test("a worker that loads slower than the time limit still draws: the limit starts when the worker is ready", async () => {
    resetMermaidDiagrams(50);
    FakeWorker.readyDelayMs = 150;
    const view = render(
      <MarkdownBody markdown={`Slow:\n\n${fence("sequenceDiagram\n  Alice->>Bob: Loaded")}`} />
    );
    try {
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      const [figure] = figuresIn(view.container);
      expect(figure.querySelector('[role="status"]')?.textContent).toBeUndefined();
      expect(diagramIn(figure)?.textContent).toContain("Loaded");
      expect(FakeWorker.created.map((worker) => worker.terminated)).toEqual([false]);
    } finally {
      view.unmount();
    }
  });

  // The worker draws one source at a time and a slow one holds it for the whole time limit. A
  // source whose every body re-painted or unmounted before the worker reached it is shown by no
  // one, so it is never sent: a reader who leaves a page of slow diagrams does not wait for them
  // on the next page.
  test("a source no body still waits for is never sent to the worker", async () => {
    await warmMarkdownRenderer();
    resetMermaidDiagrams(60_000);
    const sent: string[] = [];
    FakeWorker.answer = (request) => {
      sent.push(request.source);
      return request.source.includes("Stuck") ? undefined : answerMermaidRequest(request);
    };
    const left = render(
      <MarkdownBody
        markdown={`${fence("sequenceDiagram\n  Stuck->>Bob: Hi")}\n${fence("sequenceDiagram\n  Gone->>Bob: Hi")}`}
      />
    );
    try {
      await waitFor(() => expect(sent.some((source) => source.includes("Stuck"))).toBe(true));
    } finally {
      left.unmount();
    }
    const next = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: Next")} />);
    try {
      await waitFor(() =>
        expect(diagramIn(figuresIn(next.container)[0] ?? next.container)?.textContent).toContain(
          "Next"
        )
      );
      expect(sent.some((source) => source.includes("Gone"))).toBe(false);
    } finally {
      next.unmount();
    }
  });

  // The live view re-paints a turn on every frame while text after a finished diagram streams:
  // each paint cancels the draw and asks for the same source again in one commit, so the source
  // the worker is drawing keeps its place and its draw.
  test("a body re-painted while the worker draws its source neither stops the worker nor sends the source again", async () => {
    await warmMarkdownRenderer();
    resetMermaidDiagrams(60_000);
    const sent: MermaidRequest[] = [];
    FakeWorker.answer = (request) => {
      sent.push(request);
      return undefined;
    };
    // Holds the page's listener, so the test sends the answer once the re-paints are done.
    const listeners: ((event: unknown) => void)[] = [];
    class HeldWorker extends FakeWorker {
      override addEventListener(type: string, listener: (event: unknown) => void): void {
        super.addEventListener(type, listener);
        if (type === "message") listeners.push(listener);
      }
    }
    globalThis.Worker = HeldWorker as unknown as typeof Worker;
    const diagram = fence("sequenceDiagram\n  Alice->>Bob: Streamed");
    const view = render(<MarkdownBody markdown={diagram} />);
    try {
      await waitFor(() => expect(sent).toHaveLength(1));
      for (const after of ["More", "More text", "More text after it"]) {
        view.rerender(<MarkdownBody markdown={`${diagram}\n${after}`} />);
        await Bun.sleep(10);
      }
      expect(FakeWorker.created.map((worker) => worker.terminated)).toEqual([false]);
      expect(sent).toHaveLength(1);
      expect(figuresIn(view.container)).toHaveLength(0);

      for (const listener of listeners) listener({ data: answerMermaidRequest(sent[0]) });
      await waitFor(() =>
        expect(diagramIn(figuresIn(view.container)[0] ?? view.container)?.textContent).toContain(
          "Streamed"
        )
      );
      expect(view.container.textContent).toContain("More text after it");
      expect(sent).toHaveLength(1);
    } finally {
      view.unmount();
    }
  });

  // The time limit is a minute here, so the next body's diagram drawing within `waitFor`'s second
  // proves it never waited for the source no body shows.
  test("unmounting the only body whose source the worker is drawing stops the worker, caches nothing, and a fresh worker draws the next", async () => {
    await warmMarkdownRenderer();
    resetMermaidDiagrams(60_000);
    const sent: string[] = [];
    FakeWorker.answer = (request) => {
      sent.push(request.source);
      return request.source.includes("Stuck") ? undefined : answerMermaidRequest(request);
    };
    const stuck = fence("sequenceDiagram\n  Stuck->>Bob: Hi");
    const left = render(<MarkdownBody markdown={stuck} />);
    try {
      await waitFor(() => expect(sent).toHaveLength(1));
    } finally {
      left.unmount();
    }
    const next = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: Next")} />);
    try {
      await waitFor(() =>
        expect(diagramIn(figuresIn(next.container)[0] ?? next.container)?.textContent).toContain(
          "Next"
        )
      );
      // Terminating the worker is what stops a real layout.
      expect(FakeWorker.created.map((worker) => worker.terminated)).toEqual([true, false]);
    } finally {
      next.unmount();
    }
    // Nothing was cached for the stopped source: the same body mounted again shows it as code and
    // asks the worker for it again.
    const back = render(<MarkdownBody markdown={stuck} />);
    try {
      expect(figuresIn(back.container)).toHaveLength(0);
      await waitFor(() =>
        expect(sent.filter((source) => source.includes("Stuck"))).toHaveLength(2)
      );
    } finally {
      back.unmount();
    }
  });

  test("a worker that fails to start leaves every block as code, logs once, and is tried again once the window is online", async () => {
    await warmMarkdownRenderer();
    FakeWorker.startFails = true;
    const logged = spyOn(console, "error").mockImplementation(() => {});
    const failures = () =>
      logged.mock.calls.filter(
        ([message]) => typeof message === "string" && message.startsWith("Showing Mermaid source")
      );
    const view = render(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: One")} />);
    try {
      await waitFor(() => expect(failures()).toHaveLength(1));
      // A streamed turn paints again on every frame; no paint starts a worker or logs again.
      for (const word of ["Two", "Three", "Four"]) {
        view.rerender(<MarkdownBody markdown={fence(`sequenceDiagram\n  Alice->>Bob: ${word}`)} />);
        await Bun.sleep(10);
      }
      expect(failures()).toHaveLength(1);
      expect(FakeWorker.created).toHaveLength(1);
      expect(figuresIn(view.container)).toHaveLength(0);
      expect(view.container.querySelector("pre code")?.textContent).toContain("Four");

      FakeWorker.startFails = false;
      window.dispatchEvent(new Event("online"));
      view.rerender(<MarkdownBody markdown={fence("sequenceDiagram\n  Alice->>Bob: Five")} />);
      await waitFor(() => expect(figuresIn(view.container)).toHaveLength(1));
      expect(diagramIn(figuresIn(view.container)[0])?.textContent).toContain("Five");
      expect(FakeWorker.created).toHaveLength(2);
      expect(failures()).toHaveLength(1);
    } finally {
      logged.mockRestore();
      view.unmount();
    }
  });

  // The editor's drawer: the same queue, cache and failure latch as a body's blocks, asked for
  // one source at a time.
  describe("requestMermaidDrawing", () => {
    test("a cached source draws inside the call, and is not sent again", async () => {
      const source = "sequenceDiagram\n  Alice->>Bob: Cached";
      const first: MermaidDrawing[] = [];
      requestMermaidDrawing(source, (drawing) => first.push(drawing));
      await waitFor(() => expect(first).toHaveLength(1));
      const again: MermaidDrawing[] = [];
      requestMermaidDrawing(source, (drawing) => again.push(drawing));
      // Checked before any await: a cached draw deferred to a later microtask or task fails here.
      expect(again).toHaveLength(1);
      expect(again[0]).toBe(first[0]);
      expect(FakeWorker.sent).toEqual([source]);
    });

    test("a cancel before the answer means its draw never runs, and a source no one else waits for is never sent", async () => {
      const shared = "sequenceDiagram\n  Alice->>Bob: Shared";
      const alone = "sequenceDiagram\n  Erin->>Finn: Alone";
      const drawn: string[] = [];
      const cancelShared = requestMermaidDrawing(shared, () => drawn.push("shared, cancelled"));
      requestMermaidDrawing(shared, () => drawn.push("shared, kept"));
      const cancelAlone = requestMermaidDrawing(alone, () => drawn.push("alone, cancelled"));
      cancelShared();
      cancelAlone();
      await waitFor(() => expect(drawn).toContain("shared, kept"));
      // A source still queued would be answered before the queue settles, so its draw would run.
      await mermaidDrawsSettled();
      expect(drawn).toEqual(["shared, kept"]);
      expect(FakeWorker.sent).toEqual([shared]);
    });

    test("while the renderer is failed an uncached source draws unavailable inside the call and is not queued", async () => {
      FakeWorker.startFails = true;
      const logged = spyOn(console, "error").mockImplementation(() => {});
      try {
        requestMermaidDrawing("sequenceDiagram\n  Alice->>Bob: Before", () => {});
        await waitFor(() => expect(logged).toHaveBeenCalledTimes(1));
        const failedDraws: MermaidDrawing[] = [];
        requestMermaidDrawing("sequenceDiagram\n  Alice->>Bob: Failed", (drawing) =>
          failedDraws.push(drawing)
        );
        // Checked before any await: a draw deferred to a later microtask or task fails here.
        expect(failedDraws).toEqual([{ unavailable: true }]);
        expect(FakeWorker.created).toHaveLength(1);

        // A source queued while failed would be sent first by the next worker.
        FakeWorker.startFails = false;
        window.dispatchEvent(new Event("online"));
        const after = "sequenceDiagram\n  Alice->>Bob: After";
        const afterDraws: MermaidDrawing[] = [];
        requestMermaidDrawing(after, (drawing) => afterDraws.push(drawing));
        await waitFor(() => expect(afterDraws).toHaveLength(1));
        await mermaidDrawsSettled();
        expect(afterDraws[0] !== undefined && "svg" in afterDraws[0]).toBe(true);
        expect(failedDraws).toHaveLength(1);
        expect(FakeWorker.sent).toEqual([after]);
        expect(FakeWorker.created).toHaveLength(2);
      } finally {
        logged.mockRestore();
      }
    });

    test("every draw waiting on a worker that fails draws unavailable, once", async () => {
      // The worker never answers, so one source stays in flight with two draws, the other queued.
      FakeWorker.answer = () => undefined;
      const logged = spyOn(console, "error").mockImplementation(() => {});
      try {
        const drawn: string[] = [];
        const record = (label: string) => (drawing: MermaidDrawing) =>
          drawn.push(`${label}: ${JSON.stringify(drawing)}`);
        const inFlight = "sequenceDiagram\n  Alice->>Bob: In flight";
        requestMermaidDrawing(inFlight, record("first"));
        const cancelSecond = requestMermaidDrawing(inFlight, record("second"));
        requestMermaidDrawing("sequenceDiagram\n  Carol->>Dave: Queued", record("queued"));
        await waitFor(() => expect(FakeWorker.sent).toEqual([inFlight]));

        FakeWorker.created[0]?.fail();
        expect(drawn).toEqual([
          'first: {"unavailable":true}',
          'second: {"unavailable":true}',
          'queued: {"unavailable":true}',
        ]);
        expect(logged).toHaveBeenCalledTimes(1);
        // A draw already run has a cancel that does nothing, and nothing waits any more.
        cancelSecond();
        await mermaidDrawsSettled();
        expect(drawn).toHaveLength(3);
      } finally {
        logged.mockRestore();
      }
    });

    test("a source written with CRLF, lone CR or LF line ends is one request and one cache entry", async () => {
      const drawn: MermaidDrawing[] = [];
      requestMermaidDrawing("sequenceDiagram\r\n  Alice->>Bob: Ends", (drawing) =>
        drawn.push(drawing)
      );
      requestMermaidDrawing("sequenceDiagram\n  Alice->>Bob: Ends", (drawing) =>
        drawn.push(drawing)
      );
      await waitFor(() => expect(drawn).toHaveLength(2));
      expect(drawn[1]).toBe(drawn[0]);
      expect(FakeWorker.sent).toEqual(["sequenceDiagram\n  Alice->>Bob: Ends"]);
      requestMermaidDrawing("sequenceDiagram\r  Alice->>Bob: Ends", (drawing) =>
        drawn.push(drawing)
      );
      expect(drawn).toHaveLength(3);
      expect(drawn[2]).toBe(drawn[0]);
    });
  });

  describe("mermaidDrawsSettled", () => {
    /** Whether `promise` settles within `ms`, so a promise that never resolves fails the case on
     *  its assertion rather than on the test's timeout. */
    const settlesWithin = (promise: Promise<void>, ms: number): Promise<boolean> =>
      Promise.race([promise.then(() => true), Bun.sleep(ms).then(() => false)]);

    test("resolves at once when no source is waiting", async () => {
      let settled = false;
      void mermaidDrawsSettled().then(() => {
        settled = true;
      });
      await Promise.resolve();
      expect(settled).toBe(true);
      expect(FakeWorker.created).toHaveLength(0);
    });

    test("resolves only after the last waiting source is answered, not after the first", async () => {
      const events: string[] = [];
      requestMermaidDrawing("sequenceDiagram\n  Alice->>Bob: First", () => events.push("first"));
      requestMermaidDrawing("sequenceDiagram\n  Carol->>Dave: Second", () => events.push("second"));
      const settled = mermaidDrawsSettled().then(() => {
        events.push("settled");
      });
      expect(await settlesWithin(settled, 2000)).toBe(true);
      expect(events).toEqual(["first", "second", "settled"]);
    });

    test("resolves once a failed renderer clears the queue", async () => {
      FakeWorker.startFails = true;
      const logged = spyOn(console, "error").mockImplementation(() => {});
      try {
        const drawn: MermaidDrawing[] = [];
        requestMermaidDrawing("sequenceDiagram\n  Alice->>Bob: Never", (drawing) =>
          drawn.push(drawing)
        );
        expect(await settlesWithin(mermaidDrawsSettled(), 1000)).toBe(true);
        expect(logged).toHaveBeenCalledTimes(1);
        expect(drawn).toEqual([{ unavailable: true }]);
      } finally {
        logged.mockRestore();
      }
    });
  });
});
