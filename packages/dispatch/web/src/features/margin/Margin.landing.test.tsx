import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, type RenderResult, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask, Comment } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { Margin } from "./Margin";
import { MarginProvider } from "./margin-context";
import {
  anchoredAsk,
  CommentLink,
  comment,
  issue,
  ReportEmptyLayoutButton,
  SameCommentLink,
  SelectedItemLabel,
  stubMatchMedia,
} from "./margin-fixture";
import { RELAYOUT_SETTLES_FRAMES } from "./useCardHold";

const secondComment: Comment = {
  ...comment,
  anchor: comment.anchor === null ? null : { ...comment.anchor, mark_id: "m-2" },
  body: "and this one?",
  id: "comment-2",
};

function stubRects(cardTop: () => number): () => void {
  const bounds = (top: number, height: number) =>
    ({
      bottom: top + height,
      height,
      left: 0,
      right: 384,
      toJSON: () => ({}),
      top,
      width: 384,
      x: 0,
      y: top,
    }) as DOMRect;
  // Stubbed on Element, the prototype that owns the method: patching HTMLElement would leave an
  // own property behind that shadows every other suite's spy on Element.
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ): DOMRect {
    if (this.getAttribute("data-testid") === "margin-sheet") {
      return bounds(0, 600);
    }
    if (this.hasAttribute("data-margin-item")) {
      return bounds(cardTop(), 80);
    }
    return bounds(0, 0);
  });
  return () => rect.mockRestore();
}

function landingClient(comments: Comment[], asks: Ask[] = []): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], asks);
  queryClient.setQueryData(["asks", issue.key], asks);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], comments);
  return queryClient;
}

function renderCommentLinkLanding(comments: Comment[] = [comment], asks: Ask[] = []): RenderResult {
  return render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-1`]}
    >
      <QueryClientProvider client={landingClient(comments, asks)}>
        <MarginProvider>
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** One rendering frame. happy-dom runs animation frames as immediates, so this is one turn of the
 *  event loop - and a frame is the unit the hold's relayout window counts. */
async function nextFrame(): Promise<void> {
  const done = Promise.withResolvers<void>();
  requestAnimationFrame(() => done.resolve());
  await done.promise;
}

/** Longer than the hold's relayout window, so what follows is judged on its own: whatever the
 *  margin is still rendering lands first, then the window's frames run out. */
async function settled(): Promise<void> {
  await quiet();
  for (let frame = 0; frame <= RELAYOUT_SETTLES_FRAMES; frame += 1) {
    await nextFrame();
  }
}

async function quiet(): Promise<void> {
  const settled = Promise.withResolvers<void>();
  setTimeout(settled.resolve, 50);
  await settled.promise;
}

test("a phone comment deep link highlights its card without expanding it or opening the Thread dialog", async () => {
  // The link's destination on a phone is the Conversation turn. The margin marks the card so the
  // reader finds it when they open the review panel, but a thread on a phone opens only in the
  // Thread dialog: an inline expansion left behind here would strand a Reply composer whose Cancel
  // opens the fullscreen thread (#1239).
  const restoreMatchMedia = stubMatchMedia(true);
  const view = render(
    <MemoryRouter
      initialEntries={[buildIssuePath({ id: comment.id, key: issue.key, kind: "comment" })]}
    >
      <QueryClientProvider client={landingClient([comment])}>
        <MarginProvider>
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe(comment.id)
    );
    expect(screen.queryByRole("dialog", { name: "Thread" })).toBeNull();
    expect(screen.getByTestId("margin-sheet").getAttribute("data-expanded")).toBe("false");
    const card = screen.queryByTestId(`margin-comment-${comment.id}`);
    expect(card?.getAttribute("aria-expanded") ?? "false").toBe("false");
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

test("a desktop comment deep link activates Comments and scrolls its card from Pinned", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);
  const restoreRects = stubRects(() => 900);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <CommentLink />
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-1");
    fireEvent.click(screen.getByRole("tab", { name: "Pinned" }));
    expect(screen.getByRole("tab", { name: "Pinned" }).getAttribute("aria-selected")).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Open comment" }));

    await waitFor(() => {
      expect(screen.getByRole("tab", { name: "Comments" }).getAttribute("aria-selected")).toBe(
        "true"
      );
      expect(scrollTo).toHaveBeenCalled();
    });
    const card = screen.getByTestId("margin-comment-comment-1");
    fireEvent.click(card);
    expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1");
  } finally {
    view.unmount();
    restoreMatchMedia();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a reader who lands on a comment link can still select another card", async () => {
  // The URL keeps naming its item for the whole visit, so reading it ahead of the reader's own
  // selection - or re-applying it on every render - would pin the linked card as the selected one
  // and leave every later click with nothing to show for it.
  const restoreMatchMedia = stubMatchMedia(false);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const view = renderCommentLinkLanding([comment, secondComment], [anchoredAsk]);

  try {
    const linked = await screen.findByTestId("margin-comment-comment-1");
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );
    expect(linked.getAttribute("aria-current")).toBe("true");

    fireEvent.click(screen.getByTestId("margin-comment-comment-2"));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-2")
    );
    const second = screen.getByTestId("margin-comment-comment-2");
    expect(second.getAttribute("aria-current")).toBe("true");
    expect(second.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByTestId("margin-comment-comment-1").getAttribute("aria-current")).toBeNull();
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
  }
});

test("a reader who lands on a comment link can select a Needs you ask", async () => {
  const restoreMatchMedia = stubMatchMedia(false);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const view = renderCommentLinkLanding([comment], [anchoredAsk]);

  try {
    await screen.findByTestId("margin-comment-comment-1");
    fireEvent.click(await screen.findByTestId(`ask-${anchoredAsk.id}`));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe(anchoredAsk.id)
    );
    expect(screen.getByTestId("margin-comment-comment-1").getAttribute("aria-current")).toBeNull();
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
  }
});

test("following the same comment link again brings the reader back to its card", async () => {
  // Once per link is once per navigation: the URL still names comment-1 after the reader has
  // moved to another card, so a link back to it has to be honoured rather than seen as already
  // applied.
  const restoreMatchMedia = stubMatchMedia(false);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const view = render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-1`]}
    >
      <QueryClientProvider client={landingClient([comment, secondComment])}>
        <MarginProvider>
          <SameCommentLink />
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-1");
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );

    fireEvent.click(screen.getByTestId("margin-comment-comment-2"));
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-2")
    );

    fireEvent.click(screen.getByRole("button", { name: "Open the same comment" }));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
  }
});

test("following a second comment link selects the item that link names", async () => {
  // The route selection is applied once per link, not once ever: a reader who follows another
  // link from the same page has to land on its card.
  const restoreMatchMedia = stubMatchMedia(false);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const view = render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-2`]}
    >
      <QueryClientProvider client={landingClient([comment, secondComment])}>
        <MarginProvider>
          <CommentLink />
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-2");
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-2")
    );

    fireEvent.click(screen.getByRole("button", { name: "Open comment" }));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
  }
});

test("the margin holds a linked card in view while the margin is still filling in", async () => {
  // The margin arrives in pieces. On the frame it first has the card, the open document has
  // published no mark offsets, so every anchored card is stacked at the top of the margin and
  // the linked one is trivially in view; its real placement lands hundreds of milliseconds
  // later, and the asks that need the reader render above it later still. A margin that scrolls
  // once, or stops the first time the card is in view, leaves it below the fold.
  let cardTop = 0;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    // Unplaced at the top of the margin, the card needs no scroll - and is not settled.
    await quiet();
    const beforePlacement = scrollTo.mock.calls.length;
    expect(beforePlacement).toBe(0);

    // The document's marks land and the card takes its real place, far below the scrollport.
    cardTop = 1826;
    act(() => placement.setAttribute("style", "position: absolute; top: 1826px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(beforePlacement));

    // Inside the scrollport, the margin leaves the card - and the reader's scrolling - alone.
    cardTop = 120;
    act(() => placement.setAttribute("style", "position: absolute; top: 120px;"));
    await quiet();
    const settled = scrollTo.mock.calls.length;
    act(() => placement.setAttribute("style", "position: absolute; top: 140px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(settled);

    // Cards rendered above push it back out of the scrollport: the margin brings it back.
    cardTop = 900;
    act(() => placement.setAttribute("style", "position: absolute; top: 900px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(settled));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("the margin stops correcting a linked card once the reader scrolls the margin", async () => {
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    fireEvent.wheel(screen.getByTestId("margin-sheet"));
    await quiet();
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

/** The comment link's landing beside a document that registers and reports an empty layout on
 *  the "Report layout" press: by default the spec the margin shows cards for. */
function renderLandingWithReport(artifactId?: string): RenderResult {
  return render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-1`]}
    >
      <QueryClientProvider client={landingClient([comment])}>
        <MarginProvider>
          <ReportEmptyLayoutButton artifactId={artifactId} />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a press in the document takes over once the document has reported an empty layout", async () => {
  // The signal is the document saying it reported, not the size of what it reported: an orphaned
  // comment on a document with no live mark and no typed ask block publishes empty maps, and a
  // gate that read their size would hold that landing against the reader for ever.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);
  const view = renderLandingWithReport();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    fireEvent.click(screen.getByRole("button", { name: "Report layout" }));
    await quiet();
    act(() => {
      document.body.dispatchEvent(new window.PointerEvent("pointerdown", { bubbles: true }));
    });
    await quiet();
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a press in the document leaves the hold armed while the layout reported is another document's", async () => {
  // A document the route has left can stay registered - hidden behind the next page's loading
  // view until that page's code arrives - with its layout reported. Its report says nothing
  // about where this margin's cards belong, so a press then is the reader arriving, not leaving.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);
  const view = renderLandingWithReport("artifact-handbook");

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    fireEvent.click(screen.getByRole("button", { name: "Report layout" }));
    await quiet();
    act(() => {
      document.body.dispatchEvent(new window.PointerEvent("pointerdown", { bubbles: true }));
    });
    await quiet();
    const afterPress = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(afterPress));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a scroll the margin did not perform, with nothing having changed, ends the hold", async () => {
  // The other half of the relayout window: a scroll that no layout change and no correction of
  // ours explains is the reader's, even without a gesture to recognise them by.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    // Long enough for the margin's own relayout to have settled, so the scroll below is the
    // reader's and nothing else's.
    await settled();
    const sheet = screen.getByTestId("margin-sheet");
    sheet.scrollTop = 404;
    fireEvent.scroll(sheet);
    await quiet();
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a relayout that scrolls the margin twice leaves the hold alone", async () => {
  // One relayout moves the scroll more than once: anchoring while the content changes, then
  // clamping as it settles. A rule the first of those consumed read the second as the reader.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));
    await settled();

    const sheet = screen.getByTestId("margin-sheet");
    act(() => placement.setAttribute("style", "position: absolute; top: 950px;"));
    sheet.scrollTop = 120;
    fireEvent.scroll(sheet);
    sheet.scrollTop = 240;
    fireEvent.scroll(sheet);
    await quiet();
    const beforeTheNextRelayout = scrollTo.mock.calls.length;

    cardTop = 1400;
    act(() => placement.setAttribute("style", "position: absolute; top: 1400px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(beforeTheNextRelayout));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a stream of scrolls after one relayout does not slide the window along", async () => {
  // The window is re-opened by a layout change, never by a scroll landing inside it: otherwise a
  // reader's gesture-less scrolling - find-in-page, assistive tech - would hold it open forever.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));
    await settled();

    // One relayout, then a stream of scrolls one frame apart, so each lands inside the window the
    // one before it would have extended - a reader dragging a scrollbar the page sees no pointer
    // event for, or paging through find-in-page hits. Twelve frames is four windows' worth.
    const sheet = screen.getByTestId("margin-sheet");
    act(() => placement.setAttribute("style", "position: absolute; top: 950px;"));
    for (const step of [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]) {
      sheet.scrollTop = 100 * step;
      fireEvent.scroll(sheet);
      await nextFrame();
    }
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1400;
    act(() => placement.setAttribute("style", "position: absolute; top: 1400px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a relayout's own scroll that lands after a long task leaves the hold alone", async () => {
  // The browser delivers a relayout's scroll at a rendering update, and a page busy mounting its
  // editor can go hundreds of milliseconds without one: CI's WebKit let the margin clamp to a
  // shorter content height, then delivered that clamp's scroll after a long task, and a window
  // timed on the wall clock read it as the reader. No frame has run here, so it is the layout's.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const restoreMatchMedia = stubMatchMedia(false);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));
    await settled();

    const sheet = screen.getByTestId("margin-sheet");
    placement.setAttribute("style", "position: absolute; top: 700px;");
    // The observer's callback is a microtask: it has seen the relayout before the long task.
    await Promise.resolve();
    const busyUntil = performance.now() + 500;
    while (performance.now() < busyUntil) {
      // The long task: no rendering update runs until it ends.
    }
    sheet.scrollTop = 300;
    fireEvent.scroll(sheet);
    await quiet();
    const afterClamp = scrollTo.mock.calls.length;

    cardTop = 1400;
    act(() => placement.setAttribute("style", "position: absolute; top: 1400px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(afterClamp));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});
