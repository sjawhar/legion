import { expect, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import { EDIT_REDRAW_DELAY_MS } from "../src/mermaid-code-block-view";
import { type MermaidDrawer, type MermaidDrawing, mermaidSvgElement } from "../src/mermaid-svg";
import { withMarksEditor } from "./marks-editor";

// The view's redraw timer is the happy-dom window's own `setTimeout`, which Bun's fake timers do
// not drive, so these cases run on real time with short margins: the delay is asserted from the
// time each request is made, and a sleep only shows that nothing more is asked.

const FIRST = "graph TD\n  A --> B";
const SECOND = "graph TD\n  C --> D";
const TWO_DIAGRAMS = `Intro\n\n\`\`\`mermaid\n${FIRST}\n\`\`\`\n\n\`\`\`mermaid\n${SECOND}\n\`\`\`\n`;
const ONE_DIAGRAM = `\`\`\`mermaid\n${FIRST}\n\`\`\`\n`;

interface DrawRequest {
  readonly source: string;
  /** When the view asked, by `performance.now()`. */
  readonly at: number;
  cancels: number;
  /** Delivers an answer, unless the request was cancelled: a drawer never calls `draw` after its
   *  cancel, which is what the view relies on to drop an answer to an earlier source. */
  readonly answer: (drawing: MermaidDrawing) => void;
}

/** A drawer that records each request and answers only when the case calls `answer`, or at once
 *  with `answerAtOnce`'s drawing when it returns one; like the host's, it never draws a request
 *  after its cancel. */
function fakeDrawer(answerAtOnce?: (source: string) => MermaidDrawing | undefined): {
  drawMermaid: MermaidDrawer;
  requests: DrawRequest[];
} {
  const requests: DrawRequest[] = [];
  const drawMermaid: MermaidDrawer = (source, draw) => {
    const request: DrawRequest = {
      answer: (drawing) => {
        if (request.cancels === 0) draw(drawing);
      },
      at: performance.now(),
      cancels: 0,
      source,
    };
    requests.push(request);
    const drawing = answerAtOnce?.(source);
    if (drawing !== undefined) draw(drawing);
    return () => {
      request.cancels++;
    };
  };
  return { drawMermaid, requests };
}

/** A sanitized diagram of `document` whose one `<rect>` carries `id`; no renderer runs. */
function diagram(document: Document, id: string): MermaidDrawing {
  return {
    svg: mermaidSvgElement(
      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect id="${id}" width="4" height="4"/></svg>`,
      document
    ),
  };
}

interface BlockParts {
  readonly frame: HTMLElement;
  readonly pre: HTMLElement;
  readonly code: HTMLElement;
  readonly preview: HTMLElement | null;
  readonly status: HTMLElement | null;
}

function blocks(view: EditorView): BlockParts[] {
  return Array.from(view.dom.querySelectorAll<HTMLElement>("div.proof-code-block"), (frame) => {
    const pre = frame.querySelector<HTMLElement>("pre.proof-code-block-pre");
    const code = pre?.querySelector<HTMLElement>(":scope > code");
    if (!pre || !code) throw new Error("a code block has no pre > code");
    return {
      code,
      frame,
      pre,
      preview: frame.querySelector<HTMLElement>(":scope > div.proof-mermaid-preview"),
      status: frame.querySelector<HTMLElement>(":scope > div.proof-mermaid-status"),
    };
  });
}

/** The id of the `<rect>` the preview's shadow root draws, or null when it draws nothing. */
function shownDiagram(block: BlockParts): string | null {
  return block.preview?.shadowRoot?.querySelector("svg rect")?.id ?? null;
}

/** Appends `text` to the `index`th code block's source, as typing at its end does. */
function typeInto(view: EditorView, index: number, text: string): void {
  const ends: number[] = [];
  view.state.doc.descendants((node, pos) => {
    if (node.type.name === "code_block") ends.push(pos + 1 + node.content.size);
  });
  const end = ends[index];
  if (end === undefined) throw new Error(`the document has no code block ${index}`);
  view.dispatch(view.state.tr.insertText(text, end));
}

/** Switches the language of the document's last `mermaid` code block. The position is read once,
 *  at this call: a block that has left mermaid is not found again, and switching it back needs
 *  the position it had. */
function languageSetter(view: EditorView): (language: string) => void {
  let pos = -1;
  view.state.doc.descendants((node, at) => {
    if (node.attrs.language === "mermaid") pos = at;
  });
  return (language) => view.dispatch(view.state.tr.setNodeAttribute(pos, "language", language));
}

/** Waits until `check` holds, for at most a second. */
async function until(check: () => boolean): Promise<void> {
  const deadline = performance.now() + 1000;
  while (!check()) {
    if (performance.now() > deadline) throw new Error("timed out waiting");
    await Bun.sleep(5);
  }
}

function expectSourceShown(block: BlockParts): void {
  const details = block.pre.parentElement;
  expect(details?.matches("details.proof-mermaid-source")).toBe(true);
  expect((details as HTMLDetailsElement).open).toBe(true);
  expect(details?.firstElementChild?.matches("summary.proof-mermaid-source-summary")).toBe(true);
  expect(block.frame.classList.contains("proof-mermaid-block")).toBe(true);
  expect(block.preview?.hidden).toBe(true);
  expect(block.status?.hidden).toBe(true);
}

test("at open each mermaid block asks once and shows its source; a late answer draws without moving the pre", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    TWO_DIAGRAMS,
    async ({ view }) => {
      expect(requests.map((request) => request.source)).toEqual([FIRST, SECOND]);
      const [first, second] = blocks(view);
      if (!first || !second) throw new Error("the document draws two code blocks");
      expectSourceShown(first);
      expectSourceShown(second);
      expect(first.code.textContent).toBe(FIRST);

      const details = first.pre.parentElement;
      const moved: Node[] = [];
      const Observer = view.dom.ownerDocument.defaultView?.MutationObserver;
      if (!Observer) throw new Error("the editor's document has a window");
      const observer = new Observer((records) => {
        for (const record of records) {
          for (const node of [...record.addedNodes, ...record.removedNodes]) {
            if (node === first.pre || node === first.code || node === details) moved.push(node);
          }
        }
      });
      observer.observe(first.frame, { childList: true, subtree: true });
      requests[0]?.answer(diagram(view.dom.ownerDocument, "first"));
      await Promise.resolve();
      observer.disconnect();

      const [after] = blocks(view);
      expect(shownDiagram(first)).toBe("first");
      expect(first.preview?.hidden).toBe(false);
      expect(first.status?.hidden).toBe(true);
      expect(after?.pre).toBe(first.pre);
      expect(after?.code).toBe(first.code);
      expect(first.pre.parentElement).toBe(details);
      expect(moved).toEqual([]);
      expect(requests).toHaveLength(2);
    },
    { drawMermaid }
  );

  const atOnce = fakeDrawer(() => diagram(document, "cached"));
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      expect(atOnce.requests).toHaveLength(1);
      expect(shownDiagram(block)).toBe("cached");
      expect(block.preview?.hidden).toBe(false);
      expect(block.pre.parentElement?.matches("details.proof-mermaid-source[open]")).toBe(true);
    },
    { drawMermaid: atOnce.drawMermaid }
  );
});

test("edits ask nothing until the pause after the last one, then ask once for the current source", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    async ({ view }) => {
      const document = view.dom.ownerDocument;
      requests[0]?.answer(diagram(document, "before"));
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");

      typeInto(view, 0, "\n  B --> C");
      await Bun.sleep(50);
      // Taken before the edit, whose update starts the timer: the time measured is never shorter.
      const lastEdit = performance.now();
      typeInto(view, 0, "\n  C --> D");
      expect(requests).toHaveLength(1);

      await until(() => requests.length > 1);
      // A timer never fires early, so the request comes no sooner than the delay after the last
      // edit: one counted from the first edit would come 50 ms sooner, one without a delay at once.
      expect((requests[1]?.at ?? 0) - lastEdit).toBeGreaterThanOrEqual(EDIT_REDRAW_DELAY_MS - 1);
      expect(requests[1]?.source).toBe(`${FIRST}\n  B --> C\n  C --> D`);
      expect(requests[0]?.cancels).toBe(1);
      expect(shownDiagram(block)).toBe("before");

      await Bun.sleep(50);
      expect(requests).toHaveLength(2);
      requests[1]?.answer(diagram(document, "after"));
      expect(shownDiagram(block)).toBe("after");
      expect(requests[1]?.cancels).toBe(0);
    },
    { drawMermaid }
  );
});

test("destroying the editor cancels the request and the redraw timer", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    async ({ handle, view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      typeInto(view, 0, "\n  B --> C");

      handle.destroy();
      // The harness destroys the handle again when this case ends, which a destroyed editor
      // refuses; once is what is under test.
      handle.destroy = () => {};
      await until(() => view.isDestroyed);
      expect(requests).toHaveLength(1);
      expect(requests[0]?.cancels).toBe(1);

      await Bun.sleep(EDIT_REDRAW_DELAY_MS + 50);
      expect(requests).toHaveLength(1);
      expect(block.preview?.shadowRoot ?? null).toBeNull();
      expect(block.preview?.hidden).toBe(true);
    },
    { drawMermaid }
  );
});

test("with no drawer a mermaid block asks nothing and shows its source", async () => {
  await withMarksEditor(ONE_DIAGRAM, async ({ view }) => {
    const [block] = blocks(view);
    if (!block) throw new Error("the document draws a code block");
    expectSourceShown(block);
    typeInto(view, 0, "\n  B --> C");
    await Bun.sleep(EDIT_REDRAW_DELAY_MS + 50);
    const [after] = blocks(view);
    expect(after?.pre).toBe(block.pre);
    expectSourceShown(block);
    expect(block.preview?.shadowRoot ?? null).toBeNull();
    expect(block.code.textContent).toBe(`${FIRST}\n  B --> C`);
  });
});

test("a code block in another language keeps upstream's plain layout and asks nothing", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    `\`\`\`js\nconst x = 1;\n\`\`\`\n\n${ONE_DIAGRAM}`,
    ({ view }) => {
      const [plain, mermaid] = blocks(view);
      if (!plain || !mermaid) throw new Error("the document draws two code blocks");
      expect(requests.map((request) => request.source)).toEqual([FIRST]);
      expect(Array.from(plain.frame.children)).toEqual([plain.pre]);
      expect(plain.pre.dataset.language).toBe("js");
      expect(plain.pre.firstElementChild).toBe(plain.code);
      expect(plain.frame.classList.contains("proof-mermaid-block")).toBe(false);

      // Switching the mermaid block to another language cancels its request and puts the pre
      // back alone in the frame; switching back asks again.
      const setLanguage = languageSetter(view);
      setLanguage("ts");
      expect(requests[0]?.cancels).toBe(1);
      expect(Array.from(mermaid.frame.children)).toEqual([mermaid.pre]);
      expect(mermaid.frame.classList.contains("proof-mermaid-block")).toBe(false);
      setLanguage("mermaid");
      expect(requests.map((request) => request.source)).toEqual([FIRST, FIRST]);
      expectSourceShown(mermaid);
    },
    { drawMermaid }
  );
});

// The case above switches a block that never drew, so what `showPlainLayout` clears is never
// observed there. It shows on the way back: `showMermaidLayout` puts the preview and the status
// into the frame again and the block asks for the current source, and until that answer arrives
// whatever the block held before its trip through another language is on the reader's page.
// These two cases take that window, one for each half of what the clearing empties.
test("a mermaid block that drew, changed language and changed back shows no drawing until its new answer", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      const setLanguage = languageSetter(view);
      requests[0]?.answer(diagram(view.dom.ownerDocument, "drawn"));
      expect(shownDiagram(block)).toBe("drawn");

      setLanguage("js");
      setLanguage("mermaid");

      // The block asked again and nothing has answered, so only its source shows: the preview and
      // the status are back in the frame, empty and hidden.
      expect(requests.map((request) => request.source)).toEqual([FIRST, FIRST]);
      expect(shownDiagram(block)).toBeNull();
      expectSourceShown(block);
    },
    { drawMermaid }
  );
});

test("a mermaid block the renderer refused, changed language and changed back shows no refusal until its new answer", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      const setLanguage = languageSetter(view);
      const error = "Unable to render Mermaid diagram: Dangling Mermaid edge";
      requests[0]?.answer({ error });
      expect(block.status?.textContent).toBe(error);
      expect(block.status?.hidden).toBe(false);

      setLanguage("js");
      setLanguage("mermaid");

      expect(requests.map((request) => request.source)).toEqual([FIRST, FIRST]);
      expect(block.status?.textContent).toBe("");
      expectSourceShown(block);
    },
    { drawMermaid }
  );
});

test("a refusal shows its text in the status line and hides the preview", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    async ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      requests[0]?.answer(diagram(view.dom.ownerDocument, "drawn"));
      expect(shownDiagram(block)).toBe("drawn");

      typeInto(view, 0, " -->");
      await until(() => requests.length > 1);
      const error = "Unable to render Mermaid diagram: Dangling Mermaid edge";
      requests[1]?.answer({ error });

      expect(block.status?.hidden).toBe(false);
      expect(block.status?.textContent).toBe(error);
      expect(block.preview?.hidden).toBe(true);
      expect(block.preview?.shadowRoot?.childNodes).toHaveLength(0);
      expect(block.pre.parentElement?.matches("details.proof-mermaid-source[open]")).toBe(true);
    },
    { drawMermaid }
  );
});

test("an edit answered unavailable (no renderer can draw it) clears the earlier drawing and shows the source", async () => {
  const { drawMermaid, requests } = fakeDrawer();
  await withMarksEditor(
    ONE_DIAGRAM,
    async ({ view }) => {
      const [block] = blocks(view);
      if (!block) throw new Error("the document draws a code block");
      requests[0]?.answer(diagram(view.dom.ownerDocument, "drawn"));
      expect(shownDiagram(block)).toBe("drawn");

      typeInto(view, 0, "\n  B --> C");
      await until(() => requests.length > 1);
      requests[1]?.answer({ unavailable: true });

      expect(block.preview?.hidden).toBe(true);
      expect(block.preview?.shadowRoot?.childNodes).toHaveLength(0);
      expect(block.status?.hidden).toBe(true);
      expect(block.status?.textContent).toBe("");
      expectSourceShown(block);
      expect(block.code.textContent).toBe(`${FIRST}\n  B --> C`);
    },
    { drawMermaid }
  );
});
