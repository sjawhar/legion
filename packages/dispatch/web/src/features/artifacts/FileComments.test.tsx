import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { Anchor, Artifact, Comment } from "../../api/types";
import { MarginProvider, useMargin } from "../margin/margin-context";
import { ArtifactBlobView } from "./ArtifactHeader";

const version = (number: number) => ({
  authors: [{ id: "alice", kind: "user" as const }],
  created_at: `2026-11-01T0${number}:00:00Z`,
  mime: "image/png",
  named: false,
  number,
  size: 100,
  summary: null,
});

const chart: Artifact = {
  created_at: "2026-11-01T01:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "chart-id",
  issue_key: "CORE-1",
  kind: "image",
  name: "chart.png",
  primary: false,
  project: "CORE",
  slug: "chart-png",
  versions: [version(1), version(2)],
};

function boxComment(
  id: string,
  createdAt: string,
  anchor: Partial<Anchor>,
  extra: Partial<Comment> = {}
): Comment {
  return {
    anchor: {
      artifact_id: "chart-id",
      block_id: null,
      kind: "file",
      mark_id: id,
      orphaned: false,
      quote: "",
      version: 2,
      ...anchor,
    },
    ask_id: null,
    author: { id: "bob", kind: "user" },
    body: `Thread ${id}`,
    created_at: createdAt,
    deliveries: [],
    edited_at: null,
    id,
    issue_key: "CORE-1",
    mentions: [],
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...extra,
  };
}

const box = { height: 0.25, left: 0.62, top: 0.1, width: 0.3 };
const comments = [
  boxComment("old", "2026-11-01T02:00:00Z", { box: { ...box, top: 0.5 }, version: 1 }),
  boxComment("lower", "2026-11-01T03:00:00Z", { box: { ...box, top: 0.6 } }),
  boxComment("upper", "2026-11-01T04:00:00Z", { box }),
  boxComment("done", "2026-11-01T05:00:00Z", { box }, { resolved: true }),
  boxComment("whole", "2026-11-01T06:00:00Z", {}),
];

let restores: Array<() => void> = [];
afterEach(() => {
  for (const restore of restores) restore();
  restores = [];
});

function stubComments(list: readonly Comment[] = comments) {
  const listComments = spyOn(api, "listComments").mockResolvedValue([...list]);
  restores.push(() => listComments.mockRestore());
  return listComments;
}

function Location(): ReactNode {
  const location = useLocation();
  return <output aria-label="Location">{`${location.pathname}${location.search}`}</output>;
}

/** What the margin was asked: the mark a box asked it to focus. */
function MarginProbe(): ReactNode {
  const { documentBridge, focusRequest } = useMargin();
  return (
    <>
      <output aria-label="Focused mark">{focusRequest?.markId ?? "none"}</output>
      <output aria-label="Registered document">{documentBridge?.artifactId ?? "none"}</output>
      <button onClick={() => documentBridge?.setActiveMarks(["upper"])} type="button">
        Choose upper
      </button>
      <button onClick={() => documentBridge?.focusMark("old")} type="button">
        Focus old
      </button>
    </>
  );
}

function renderFile({
  isClosed = false,
  path = "/issues/CORE-1/artifacts/chart-png",
  version: shown,
}: {
  isClosed?: boolean;
  path?: string;
  version?: number;
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      initialEntries={[path]}
    >
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <ArtifactBlobView
            artifact={chart}
            comments={{ isClosed, owner: { issueKey: "CORE-1", kind: "issue" } }}
            version={shown}
          />
          <MarginProbe />
          <Location />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function picture(): HTMLElement {
  return screen.getByTestId("picture-boxes");
}

/** Lays the picture out at 400 by 200 CSS pixels from (100, 50), which happy-dom does not. */
function layOut(element: HTMLElement): void {
  element.getBoundingClientRect = () =>
    ({
      bottom: 250,
      height: 200,
      left: 100,
      right: 500,
      top: 50,
      width: 400,
      x: 100,
      y: 50,
    }) as DOMRect;
}

test("the shown version's open box threads are drawn over the picture, numbered as their cards", async () => {
  stubComments();
  const view = renderFile();
  try {
    const upper = await within(picture()).findByRole("button", { name: /^Box 3: Thread upper/ });
    const lower = within(picture()).getByRole("button", { name: /^Box 2: Thread lower/ });
    expect(upper.style.left).toBe("62%");
    expect(upper.style.top).toBe("10%");
    expect(upper.style.width).toBe("30%");
    expect(upper.style.height).toBe("25%");
    expect(upper.getAttribute("data-id")).toBe("upper");
    expect(lower.style.top).toBe("60%");
    // Version 1's box, the resolved box and the whole-file thread are not drawn on version 2.
    expect(within(picture()).getAllByRole("button")).toHaveLength(2);
    expect(screen.getByLabelText("Registered document").textContent).toBe("chart-id");
  } finally {
    view.unmount();
  }
});

test("a version's own boxes are drawn on it", async () => {
  stubComments();
  const view = renderFile({ path: "/issues/CORE-1/artifacts/chart-png?v=1", version: 1 });
  try {
    await within(picture()).findByRole("button", { name: /^Box 1: Thread old/ });
    expect(within(picture()).getAllByRole("button")).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("choosing a box opens its thread in the margin, and a chosen thread lights its box", async () => {
  stubComments();
  const view = renderFile();
  try {
    const upper = await within(picture()).findByRole("button", { name: /^Box 3/ });
    fireEvent.click(upper);
    expect(screen.getByLabelText("Focused mark").textContent).toBe("upper");
    expect(upper.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: "Choose upper" }));
    expect(upper.getAttribute("aria-pressed")).toBe("true");
  } finally {
    view.unmount();
  }
});

test("choosing a thread whose box is on another version opens that version", async () => {
  stubComments();
  const view = renderFile();
  try {
    await within(picture()).findByRole("button", { name: /^Box 3/ });
    fireEvent.click(screen.getByRole("button", { name: "Focus old" }));
    expect(screen.getByLabelText("Location").textContent).toBe(
      "/issues/CORE-1/artifacts/chart-png?v=1&comment=old"
    );
  } finally {
    view.unmount();
  }
});

test("a link to a box on another version opens the version the box is on", async () => {
  stubComments();
  const view = renderFile({ path: "/issues/CORE-1/artifacts/chart-png?comment=old" });
  try {
    await waitFor(() =>
      expect(screen.getByLabelText("Location").textContent).toBe(
        "/issues/CORE-1/artifacts/chart-png?v=1&comment=old"
      )
    );
  } finally {
    view.unmount();
  }
});

test("a drag over the picture draws the box, and the Comment box sends it with the version", async () => {
  stubComments([]);
  const createComment = spyOn(api, "createComment").mockResolvedValue(
    boxComment("new", "2026-11-01T07:00:00Z", { box })
  );
  restores.push(() => createComment.mockRestore());
  const view = renderFile();
  try {
    expect(screen.getByText(/On the whole picture, version 2/)).toBeDefined();
    layOut(picture());
    fireEvent.pointerDown(picture(), {
      button: 0,
      clientX: 140,
      clientY: 70,
      pointerId: 1,
      pointerType: "mouse",
    });
    fireEvent.pointerMove(picture(), {
      clientX: 200,
      clientY: 100,
      pointerId: 1,
      pointerType: "mouse",
    });
    expect(screen.getByTestId("picture-box-draft").style.width).toBe("15%");
    fireEvent.pointerUp(picture(), {
      clientX: 260,
      clientY: 120,
      pointerId: 1,
      pointerType: "mouse",
    });
    const drawn = screen.getByTestId("picture-box-draft");
    expect([drawn.style.left, drawn.style.top, drawn.style.width, drawn.style.height]).toEqual([
      "10%",
      "10%",
      "30%",
      "25%",
    ]);
    expect(screen.getByText(/On the box you drew, version 2/)).toBeDefined();

    const form = screen.getByRole("region", { name: "Comment on chart.png" });
    fireEvent.change(within(form).getByLabelText("Comment"), {
      target: { value: "This bar looks low." },
    });
    fireEvent.click(within(form).getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          anchor: {
            artifact: "chart-id",
            box: { height: 0.25, left: 0.1, top: 0.1, width: 0.3 },
            version: 2,
          },
          body: "This bar looks low.",
        },
        keyedAttempt
      )
    );
    // A sent comment takes its box with it: the next comment is about the whole picture again.
    await waitFor(() => expect(screen.queryByTestId("picture-box-draft")).toBeNull());
  } finally {
    view.unmount();
  }
});

test("Clear box comments on the whole picture, and a touch draws only while Draw a box is on", async () => {
  stubComments([]);
  const createComment = spyOn(api, "createComment").mockResolvedValue(
    boxComment("new", "2026-11-01T07:00:00Z", {})
  );
  restores.push(() => createComment.mockRestore());
  const view = renderFile({ path: "/issues/CORE-1/artifacts/chart-png?v=1", version: 1 });
  try {
    layOut(picture());
    const touch = { pointerId: 2, pointerType: "touch" };
    fireEvent.pointerDown(picture(), { ...touch, button: 0, clientX: 140, clientY: 70 });
    fireEvent.pointerUp(picture(), { ...touch, clientX: 260, clientY: 120 });
    expect(screen.queryByTestId("picture-box-draft")).toBeNull();

    const draw = screen.getByRole("button", { name: "Draw a box" });
    fireEvent.click(draw);
    expect(draw.getAttribute("aria-pressed")).toBe("true");
    fireEvent.pointerDown(picture(), { ...touch, button: 0, clientX: 140, clientY: 70 });
    fireEvent.pointerUp(picture(), { ...touch, clientX: 260, clientY: 120 });
    expect(screen.getByTestId("picture-box-draft")).toBeDefined();
    expect(draw.getAttribute("aria-pressed")).toBe("false");

    fireEvent.click(screen.getByRole("button", { name: "Clear box" }));
    expect(screen.queryByTestId("picture-box-draft")).toBeNull();
    const form = screen.getByRole("region", { name: "Comment on chart.png" });
    fireEvent.change(within(form).getByLabelText("Comment"), { target: { value: "Which run?" } });
    await act(async () => {
      fireEvent.click(within(form).getByRole("button", { name: "Send" }));
    });
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        { anchor: { artifact: "chart-id", version: 1 }, body: "Which run?" },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
  }
});

test("a closed issue's file takes no new comment and draws no box", async () => {
  stubComments([]);
  const view = renderFile({ isClosed: true });
  try {
    layOut(picture());
    fireEvent.pointerDown(picture(), {
      button: 0,
      clientX: 140,
      clientY: 70,
      pointerId: 1,
      pointerType: "mouse",
    });
    fireEvent.pointerUp(picture(), {
      clientX: 260,
      clientY: 120,
      pointerId: 1,
      pointerType: "mouse",
    });
    expect(screen.queryByTestId("picture-box-draft")).toBeNull();
    expect(screen.queryByRole("button", { name: "Draw a box" })).toBeNull();
    expect(screen.queryByLabelText("Comment")).toBeNull();
  } finally {
    view.unmount();
  }
});
