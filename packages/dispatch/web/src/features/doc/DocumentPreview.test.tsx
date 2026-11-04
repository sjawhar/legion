import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { act, render, waitFor } from "@testing-library/react";

import {
  type MarkdownRendering,
  renderWithEngine,
  setParseDeadline,
  warmMarkdownRenderer,
} from "../refs/markdown-engine";
import { markdownWorkerThreshold } from "../refs/markdown-worker-threshold";
import {
  type InstalledParseWorker,
  installFakeParseWorker,
  restoreParseWorker,
} from "../refs/parse-worker-fixture";
import { DocumentPreview } from "./DocumentPreview";
import { leadingBlocks } from "./document-blocks";

/** A document of one-word paragraphs, longer than the threshold several times over, under a
 *  lead line that is the row's own so no row reads the answer the engine cached for another. */
function wholeDocument(lead: string): string {
  return `${lead}\n\n${"x\n\n".repeat(markdownWorkerThreshold)}`.trimEnd();
}

/** The preview's own element, which carries the document role and the pending mark. */
function preview(container: HTMLElement): HTMLElement {
  const element = container.querySelector<HTMLElement>("[role='document']");
  if (element === null) {
    throw new Error("The preview drew no document element.");
  }
  return element;
}

/** The paragraphs the preview holds, which is how much of the document it has drawn. */
function paragraphs(container: HTMLElement): number {
  return preview(container).querySelectorAll(":scope > p").length;
}

/** Whether the engine already holds a parsed answer for `markdown`: it does when something
 *  parsed it on the page, and then a fresh render of it is answered in its own call with a
 *  document and asks the worker for nothing. */
function alreadyParsed(markdown: string): { cached: boolean; cancel: () => void } {
  const seen: MarkdownRendering[] = [];
  const cancel = renderWithEngine(markdown, "space", "refuse", (rendering) => {
    seen.push(rendering);
  });
  return { cached: seen[0]?.pending === false && seen[0]?.parsed !== undefined, cancel };
}

/**
 * What the document preview asks of the shared parse queue, which is not what every other body
 * on the page asks of it (dispatch://INFRA-341, R11). Its source is a whole document - 406,000
 * characters in the measurement its own doc comment records - and the two rows below are the
 * two ways the queue would otherwise put a parse of all of it on the reader's own thread: the
 * queue's deadline, and an answer kept for an element that has gone.
 */
describe("the document preview's parse", () => {
  let worker: InstalledParseWorker;

  beforeEach(async () => {
    await warmMarkdownRenderer();
    worker = installFakeParseWorker();
  });

  afterEach(() => {
    setParseDeadline();
    restoreParseWorker();
  });

  // The queue's deadline is 10 s and the whole document was answered 16 s after it was posted on
  // a throttled phone, so this is the case, not the corner. The fallback every other body wants
  // there - the page parsing it itself - is a synchronous parse of the whole document in the
  // middle of the wait this surface exists to shorten, and this surface needs no fallback: the
  // first pass is already in front of the reader and the editor is arriving behind it. This row
  // shortens the deadline through `setParseDeadline`, as `MarkdownBody.test.tsx` does, so it
  // proves what the deadline ends in without waiting out the page's own 10 s.
  test("parses nothing on the reader's thread when the worker misses its deadline", async () => {
    const markdown = wholeDocument("A document the worker never answers");
    const firstScreenful = leadingBlocks(markdown, markdownWorkerThreshold);
    expect(firstScreenful).not.toBe(markdown);
    const screenfulParagraphs = firstScreenful.split("\n\n").length;

    setParseDeadline(50);
    const view = render(<DocumentPreview building={false} markdown={markdown} />);
    try {
      // The first pass: the blocks the document opens with, parsed on the page in one short
      // task, which is what puts the whole document in front of the reader next.
      await waitFor(() => expect(paragraphs(view.container)).toBe(screenfulParagraphs));
      await worker.waitForRequests(1);
      expect(worker.requests[0].markdown).toBe(markdown);

      // Well past the deadline, which fires with the preview still mounted and waiting.
      const pastTheDeadline = Promise.withResolvers<void>();
      setTimeout(pastTheDeadline.resolve, 300);
      await act(() => pastTheDeadline.promise);

      // Nothing was parsed for it: the element still holds the first pass, and the engine holds
      // no answer for the whole document, which a parse on the page would have cached.
      expect(paragraphs(view.container)).toBe(screenfulParagraphs);
      expect(preview(view.container).getAttribute("data-markdown-fallback")).toBeNull();
      const { cached, cancel } = alreadyParsed(markdown);
      cancel();
      expect(cached).toBe(false);
    } finally {
      view.unmount();
    }
  }, 20_000);

  // The other way round: the editor lands first, this element unmounts, and the worker's answer
  // arrives after it. Keeping a place on a posted parse is for the streaming turn in the live
  // view, whose reader would otherwise watch a body that never changes; a document's text does
  // not stream, so a kept place here buys nothing and costs the whole parsed document built on
  // the reader's thread for a paint `useRenderedMarkdown` throws away in its next line, plus
  // one of the four places in the answer cache.
  test("builds nothing on the reader's thread for an answer that arrives after it has gone", async () => {
    const markdown = wholeDocument("A document whose editor landed first");
    const view = render(<DocumentPreview building={false} markdown={markdown} />);
    try {
      await worker.waitForRequests(1);
      expect(worker.requests[0].markdown).toBe(markdown);
    } finally {
      view.unmount();
    }

    // The worker answers the body it was already holding, as it does whatever became of the
    // element that asked: the queue must build nothing out of that answer.
    await act(() => worker.answer(0));

    const { cached, cancel } = alreadyParsed(markdown);
    cancel();
    expect(cached).toBe(false);
  }, 20_000);

  // The third way the whole document would be parsed or painted for nothing: the editor's
  // build has begun. Every batch of it hands the thread back (`progressive-build.ts`), and a
  // paint of the whole document into those gaps is work for a draw the editor is about to
  // replace - it delays the editor the reader is waiting for by the whole of its own cost
  // (dispatch://INFRA-341, R11).
  test("paints no more of the document once the editor's build has begun", async () => {
    const markdown = wholeDocument("A document whose editor began building");
    const firstScreenful = leadingBlocks(markdown, markdownWorkerThreshold);
    const screenfulParagraphs = firstScreenful.split("\n\n").length;

    const view = render(<DocumentPreview building={false} markdown={markdown} />);
    try {
      await waitFor(() => expect(paragraphs(view.container)).toBe(screenfulParagraphs));
      await worker.waitForRequests(1);
      expect(worker.requests[0].markdown).toBe(markdown);

      // The build begins while the worker still holds the whole document.
      view.rerender(<DocumentPreview building markdown={markdown} />);
      await act(() => worker.answer(0));

      // The reader keeps the pass they were reading, and nothing was built out of that answer:
      // not the paint, and not the cache entry a paint would have left behind.
      expect(paragraphs(view.container)).toBe(screenfulParagraphs);
      expect(preview(view.container).getAttribute("data-markdown-fallback")).toBeNull();
      const { cached, cancel } = alreadyParsed(markdown);
      cancel();
      expect(cached).toBe(false);
    } finally {
      view.unmount();
    }
  }, 20_000);

  // And where the build begins before the first pass has even landed, the whole document is
  // never posted at all: the reader gets the first pass, and the thread is the build's.
  test("asks for no second pass when the build begins before the first one lands", async () => {
    const markdown = wholeDocument("A document whose build began first");
    const firstScreenful = leadingBlocks(markdown, markdownWorkerThreshold);
    const screenfulParagraphs = firstScreenful.split("\n\n").length;

    const view = render(<DocumentPreview building markdown={markdown} />);
    try {
      await waitFor(() => expect(paragraphs(view.container)).toBe(screenfulParagraphs));
      expect(worker.requests).toEqual([]);
    } finally {
      view.unmount();
    }
  }, 20_000);
});
