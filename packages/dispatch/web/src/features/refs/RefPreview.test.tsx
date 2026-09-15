import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { type ReactNode, useEffect, useRef } from "react";
import { Link, MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import {
  REF_PREVIEW_OPEN_DELAY_MS,
  RefPreviewHost,
  referenceTriggerProps,
  refPreview,
} from "./RefPreview";

function issue(key: string, title: string): IssueDetails {
  return {
    artifacts: [],
    children: [],
    closed_at: null,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key,
    labels: ["ui"],
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    assignee: null,
    primary_artifact_id: "spec",
    priority: 1,
    project: "CORE",
    rank: "U",
    route: null,
    status: "in_progress",
    title,
    updated_at: "2026-09-09T00:00:00Z",
  };
}

const issues: Record<string, IssueDetails> = {
  "CORE-1": issue("CORE-1", "Design decision"),
  "CORE-2": issue("CORE-2", "Second target"),
};

function Harness({ children }: { children?: ReactNode }): ReactNode {
  return (
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Link to="/issues/CORE-1" {...referenceTriggerProps({ key: "CORE-1", kind: "issue" })}>
          first
        </Link>
        <Link to="/issues/CORE-2" {...referenceTriggerProps({ key: "CORE-2", kind: "issue" })}>
          second
        </Link>
        {children}
        <RefPreviewHost />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** A stand-in for a Markdown body or the Proof editor: anchors React did not render, inserted
 * after the host mounted, tagged the way those surfaces tag them. */
function ForeignBody(): ReactNode {
  const root = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    const element = root.current;
    if (element === null) {
      throw new Error("missing root");
    }
    element.innerHTML =
      'See <a data-dispatch-ref="dispatch://CORE-2" href="/issues/CORE-2">CORE-2</a> and' +
      ' <a data-dispatch-href="dispatch://CORE-1" href="">the proof mark</a>.';
  }, []);
  return <p ref={root} />;
}

function mockIssues() {
  return spyOn(api, "getIssue").mockImplementation((key) =>
    Promise.resolve(issues[key] as IssueDetails)
  );
}

afterEach(() => {
  jest.useRealTimers();
  refPreview.close();
});

test("the card opens only after the hover delay, loads populated, and closes when the pointer leaves", async () => {
  const getIssue = mockIssues();
  jest.useFakeTimers();
  const view = render(<Harness />);
  try {
    const first = screen.getByRole("link", { name: "first" });
    fireEvent.pointerOver(first, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS - 1);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
    // The hover itself already asked for the target, so the card opens populated.
    expect(getIssue).toHaveBeenCalledWith("CORE-1");

    act(() => {
      jest.advanceTimersByTime(1);
    });
    const card = screen.getByRole("tooltip");
    expect(first.getAttribute("aria-describedby")).toBe(card.id);
    // The mocked fetch settles on microtasks alone, which the async act flushes.
    await act(async () => {});
    expect(card.textContent).toContain("Design decision");
    expect(card.textContent).toContain("In progress");
    expect(card.textContent).toContain("P1");
    expect(card.textContent).toContain("ui");
    expect(card.querySelector("a")?.getAttribute("href")).toBe("/issues/CORE-1");

    fireEvent.pointerOut(first, { pointerType: "mouse" });
    // The hover bridge: the pointer reaching the card keeps it open…
    act(() => {
      jest.advanceTimersByTime(50);
    });
    fireEvent.pointerEnter(card, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(1000);
    });
    expect(screen.getByRole("tooltip")).toBe(card);
    // …and leaving the card closes it.
    fireEvent.pointerLeave(card, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(1000);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
    expect(first.hasAttribute("aria-describedby")).toBe(false);
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a hover that ends before the delay, a touch pointer, or a held button never opens a card", () => {
  const getIssue = mockIssues();
  jest.useFakeTimers();
  const view = render(<Harness />);
  try {
    const first = screen.getByRole("link", { name: "first" });
    fireEvent.pointerOver(first, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(100);
    });
    fireEvent.pointerOut(first, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(1000);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    fireEvent.pointerOver(first, { pointerType: "touch" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS + 100);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    // A drag-selection sweeping across the link: primary button held.
    fireEvent.pointerOver(first, { buttons: 1, pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS + 100);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("Escape closes the open card", () => {
  const getIssue = mockIssues();
  jest.useFakeTimers();
  const view = render(<Harness />);
  try {
    const first = screen.getByRole("link", { name: "first" });
    fireEvent.pointerOver(first, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip")).toBeTruthy();
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("only one card is open at a time: hovering a second reference replaces the first", async () => {
  const getIssue = mockIssues();
  jest.useFakeTimers();
  const view = render(<Harness />);
  try {
    const first = screen.getByRole("link", { name: "first" });
    const second = screen.getByRole("link", { name: "second" });
    fireEvent.pointerOver(first, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");

    fireEvent.pointerOut(first, { pointerType: "mouse" });
    fireEvent.pointerOver(second, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    const cards = screen.getAllByRole("tooltip");
    expect(cards).toHaveLength(1);
    expect(cards[0].textContent).toContain("CORE-2");
    expect(cards[0].textContent).not.toContain("CORE-1");
    await act(async () => {});
    expect(cards[0].textContent).toContain("Second target");
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("keyboard focus opens the card at once, scrolling under it repositions rather than closes, and blur closes it", () => {
  const getIssue = mockIssues();
  const view = render(<Harness />);
  try {
    const first = screen.getByRole("link", { name: "first" });
    act(() => {
      first.focus();
    });
    const card = screen.getByRole("tooltip");
    expect(card.textContent).toContain("CORE-1");
    expect(first.getAttribute("aria-describedby")).toBe(card.id);

    // Focus that scrolls the anchor into view fires a scroll: the anchor still has focus, so
    // the card stays and follows it.
    fireEvent.scroll(document);
    expect(screen.getByRole("tooltip")).toBe(card);

    act(() => {
      first.blur();
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("scrolling under a hover-opened card closes it", () => {
  const getIssue = mockIssues();
  jest.useFakeTimers();
  const view = render(<Harness />);
  try {
    const first = screen.getByRole("link", { name: "first" });
    fireEvent.pointerOver(first, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip")).toBeTruthy();
    fireEvent.scroll(document);
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("anchors React did not render are triggers too, and a card closes when its anchor leaves the DOM", async () => {
  const getIssue = mockIssues();
  jest.useFakeTimers();
  const view = render(
    <Harness>
      <ForeignBody />
    </Harness>
  );
  try {
    const markdownAnchor = screen.getByRole("link", { name: "CORE-2" });
    fireEvent.pointerOver(markdownAnchor, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-2");
    expect(markdownAnchor.getAttribute("aria-describedby")).toBe(screen.getByRole("tooltip").id);
    fireEvent.pointerOut(markdownAnchor, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(1000);
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    // A Proof link mark carries its target in data-dispatch-href, its href sanitized to "".
    const proofAnchor = screen.getByText("the proof mark");
    fireEvent.pointerOver(proofAnchor, { pointerType: "mouse" });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");

    // The body re-renders and drops the anchor: no pointerout ever fires, the card still goes.
    // (The removal is observed on a microtask, which the async act flushes.)
    await act(async () => {
      proofAnchor.remove();
    });
    expect(screen.queryByRole("tooltip")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});
