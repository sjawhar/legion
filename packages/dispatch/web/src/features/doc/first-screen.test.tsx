import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, render, waitFor } from "@testing-library/react";

import { markdownClassName, warmMarkdownRenderer } from "../refs/markdown-engine";
import { markdownWorkerThreshold } from "../refs/markdown-worker-threshold";
import {
  type InstalledParseWorker,
  installFakeParseWorker,
  restoreParseWorker,
} from "../refs/parse-worker-fixture";
import { DocumentPreview } from "./DocumentPreview";
import {
  currentAddress,
  dropFirstScreen,
  firstScreenAwaits,
  takeFirstScreen,
} from "./first-screen";

const ADDRESS = "/issues/CORE-1/artifacts/spec-md";

/** The page the server sends a reader who may read the document at `address`: the build's own
 *  `#first-screen` element with the document's opening blocks drawn into it
 *  (`internal/dispatch/routes/router.go`). */
function pageCarryingADraw(address = ADDRESS, blocks = `<p data-block-id="b-1">Served</p>`): void {
  document.body.innerHTML =
    `<div id="root"></div>` +
    `<div id="first-screen" class="${markdownClassName} max-w-none">` +
    `<div role="document" aria-label="Document text" data-first-screen-address="${address}">` +
    `${blocks}</div></div>`;
}

afterEach(() => {
  document.body.innerHTML = "";
});

describe("the blocks the server drew into the page", () => {
  test("are taken by the address they were drawn for, and the element goes with them", () => {
    pageCarryingADraw();
    expect(firstScreenAwaits(ADDRESS)).toBe(true);

    const blocks = takeFirstScreen(ADDRESS);

    expect(blocks).toBeDefined();
    expect((blocks as DocumentFragment).querySelector("[data-block-id]")?.textContent).toBe(
      "Served"
    );
    expect(document.getElementById("first-screen")).toBeNull();
    expect(firstScreenAwaits(ADDRESS)).toBe(false);
  });

  // A reader can navigate between the page's bytes arriving and the document's own surface
  // mounting, and one document's blocks are never another document's.
  test("are never taken by another address", () => {
    pageCarryingADraw();

    expect(firstScreenAwaits("/issues/CORE-1/artifacts/other-md")).toBe(false);
    expect(takeFirstScreen("/issues/CORE-1/artifacts/other-md")).toBeUndefined();
    expect(document.getElementById("first-screen")).not.toBeNull();
  });

  test("are dropped where the reader is at another address, and kept where they are not", () => {
    pageCarryingADraw();

    dropFirstScreen(ADDRESS);
    expect(document.getElementById("first-screen")).not.toBeNull();

    dropFirstScreen("/inbox");
    expect(document.getElementById("first-screen")).toBeNull();
  });

  test("are nothing to take on a page the server drew none into", () => {
    document.body.innerHTML = `<div id="root"></div><div id="first-screen"></div>`;

    expect(firstScreenAwaits(ADDRESS)).toBe(false);
    expect(takeFirstScreen(ADDRESS)).toBeUndefined();
  });

  // The server stamps the address it answered, which it read decoded; the page's own address is
  // the browser's, which is not.
  test("are matched on the address the server spells", () => {
    expect(currentAddress("/issues/CORE-1/artifacts/a%20b")).toBe("/issues/CORE-1/artifacts/a b");
    expect(currentAddress("/issues/CORE-1/artifacts/100%")).toBe("/issues/CORE-1/artifacts/100%");
  });

  // The page's preview and the server's draw are one element's worth of classes, so the blocks
  // read the same before the page runs as after it.
  test("are drawn in the classes the page's own preview wears", () => {
    const page = readFileSync(join(import.meta.dir, "../../../index.html"), "utf8");
    const element = /<div\s+id="first-screen"\s+class="([^"]*)"/.exec(page);
    expect(element).not.toBeNull();
    for (const className of markdownClassName.split(" ")) {
      expect((element as RegExpExecArray)[1].split(" ")).toContain(className);
    }
  });
});

/** A document of one-word paragraphs, longer than the threshold several times over. */
function wholeDocument(lead: string): string {
  return `${lead}\n\n${"x\n\n".repeat(markdownWorkerThreshold)}`.trimEnd();
}

function preview(container: HTMLElement): HTMLElement {
  const element = container.querySelector<HTMLElement>("[role='document']");
  if (element === null) {
    throw new Error("The preview drew no document element.");
  }
  return element;
}

describe("the document's own draw, where the server drew its opening blocks", () => {
  let worker: InstalledParseWorker;

  beforeEach(async () => {
    await warmMarkdownRenderer();
    worker = installFakeParseWorker();
  });

  afterEach(() => {
    restoreParseWorker();
  });

  // The reader has been reading these blocks since the page was parsed, which on a phone is
  // seconds before this element exists. The page adopts them: it parses no screenful of its own,
  // and nothing goes off the screen while the whole document is still being parsed.
  test("adopts them, and parses no first screenful of its own", async () => {
    pageCarryingADraw();
    const markdown = wholeDocument("A document the server drew the opening of");

    const view = render(<DocumentPreview address={ADDRESS} building={false} markdown={markdown} />);
    try {
      await waitFor(() => expect(worker.requests.length).toBe(1));
      // The only parse asked for is the whole document; the screenful the page would otherwise
      // parse for itself was never asked for.
      expect(worker.requests[0].markdown).toBe(markdown);
      const drawn = preview(view.container);
      expect(drawn.querySelectorAll("[data-block-id]").length).toBe(1);
      expect(drawn.textContent).toBe("Served");
      expect(document.getElementById("first-screen")).toBeNull();

      // The whole document replaces them where it stands, as it replaces the page's own first
      // pass.
      await act(() => worker.answer(0));
      await waitFor(() =>
        expect(preview(view.container).querySelectorAll("p").length).toBeGreaterThan(1)
      );
    } finally {
      view.unmount();
    }
  }, 20_000);

  // The document's text has not arrived yet, and the blocks are already on the screen: the
  // element holds them rather than drawing - or clearing - anything.
  test("holds them while the document's text is still being read", async () => {
    pageCarryingADraw();

    const view = render(<DocumentPreview address={ADDRESS} building={false} markdown="" />);
    try {
      expect(preview(view.container).textContent).toBe("Served");
      expect(worker.requests).toEqual([]);
    } finally {
      view.unmount();
    }
  });

  // The editor's build has begun and nothing of this element's own has been drawn: the blocks
  // the reader is reading stay, and no parse is paid for a draw the editor is about to replace.
  test("holds them from the task the editor's build begins in", async () => {
    pageCarryingADraw();
    const markdown = wholeDocument("A document whose editor began building first");

    const view = render(<DocumentPreview address={ADDRESS} building markdown={markdown} />);
    try {
      expect(preview(view.container).textContent).toBe("Served");
    } finally {
      view.unmount();
    }
  });

  // A page the server drew nothing into reads exactly as it always has: the screenful first,
  // the whole document behind it.
  test("draws its own first screenful where the server drew none", async () => {
    document.body.innerHTML = `<div id="root"></div><div id="first-screen"></div>`;
    const markdown = wholeDocument("A document the server drew nothing of");

    const view = render(<DocumentPreview address={ADDRESS} building={false} markdown={markdown} />);
    try {
      await waitFor(() =>
        expect(preview(view.container).querySelectorAll("p").length).toBeGreaterThan(0)
      );
      await waitFor(() => expect(worker.requests.length).toBe(1));
      expect(worker.requests[0].markdown).toBe(markdown);
    } finally {
      view.unmount();
    }
  }, 20_000);
});
