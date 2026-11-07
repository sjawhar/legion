import { describe, expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";

import { ContextRing, type ContextUsage, contextShare } from "./ContextRing";

/** The ring as its host drives it: the host owns the panel, as the footer row does. */
function Host({ usage }: { usage: ContextUsage | undefined }) {
  const [open, setOpen] = useState(false);
  return <ContextRing onOpenChange={setOpen} open={open} usage={usage} />;
}

describe("the share a session's figures make", () => {
  test("is the whole percent of the window in use", () => {
    expect(contextShare({ contextWindow: 200_000, tokens: 68_000 })).toBe(34);
    expect(contextShare({ contextWindow: 200_000, tokens: 120_000 })).toBe(60);
    expect(contextShare({ contextWindow: 200_000, tokens: 184_210 })).toBe(92);
  });

  test("a host that counts past its own window reads full, never above it", () => {
    expect(contextShare({ contextWindow: 200_000, tokens: 260_000 })).toBe(100);
  });

  test("there is none for a session that published nothing, nor for a window of zero", () => {
    expect(contextShare(undefined)).toBeUndefined();
    expect(contextShare({ contextWindow: 0, tokens: 0 })).toBeUndefined();
    expect(contextShare({ contextWindow: 0, tokens: 4_000 })).toBeUndefined();
    expect(contextShare({ contextWindow: Number.NaN, tokens: 4_000 })).toBeUndefined();
    expect(contextShare({ contextWindow: 200_000, tokens: Number.NaN })).toBeUndefined();
  });
});

describe("the context ring", () => {
  test("draws the share, and its panel the tokens and the window", () => {
    const view = render(<Host usage={{ contextWindow: 200_000, tokens: 68_000 }} />);
    try {
      const ring = screen.getByRole("button", { name: "Context window, 34 percent used" });
      expect(ring.getAttribute("aria-expanded")).toBe("false");
      expect(ring.getAttribute("title")).toBe("68,000 of 200,000 tokens");
      expect(screen.getByTestId("agent-context-share").textContent).toBe("34% of the window");
      expect(screen.queryByTestId("agent-context-popover")).toBeNull();

      fireEvent.click(ring);
      const panel = screen.getByRole("dialog", { name: "Context window" });
      expect(ring.getAttribute("aria-expanded")).toBe("true");
      expect(panel.textContent).toContain("68,000 of 200,000 tokens · 34%");
      expect(panel.textContent).toContain("When it is nearly full the session drops its oldest");
      // Nothing in the panel drives the session: no compaction control, no other press.
      expect(panel.querySelectorAll("button, input, select, a")).toHaveLength(0);
    } finally {
      view.unmount();
    }
  });

  test("a session that published no figures, and a window of zero, draw no ring at all", () => {
    const none = render(<Host usage={undefined} />);
    try {
      expect(screen.queryByTestId("agent-context-ring")).toBeNull();
      expect(screen.queryByRole("button")).toBeNull();
    } finally {
      none.unmount();
    }

    const zero = render(<Host usage={{ contextWindow: 0, tokens: 12_000 }} />);
    try {
      expect(screen.queryByTestId("agent-context-ring")).toBeNull();
      expect(screen.queryByText(/%/)).toBeNull();
    } finally {
      zero.unmount();
    }
  });

  test("a newer figure moves the ring and the open panel where they stand", () => {
    const view = render(<ContextRing onOpenChange={() => {}} open={true} usage={undefined} />);
    try {
      expect(screen.queryByTestId("agent-context-ring")).toBeNull();

      view.rerender(
        <ContextRing
          onOpenChange={() => {}}
          open={true}
          usage={{ contextWindow: 200_000, tokens: 68_000 }}
        />
      );
      expect(screen.getByTestId("agent-context-share").textContent).toBe("34% of the window");
      expect(screen.getByRole("dialog").textContent).toContain("68,000 of 200,000 tokens · 34%");

      view.rerender(
        <ContextRing
          onOpenChange={() => {}}
          open={true}
          usage={{ contextWindow: 200_000, tokens: 120_000 }}
        />
      );
      expect(screen.getByTestId("agent-context-share").textContent).toBe("60% of the window");
      expect(screen.getByRole("dialog").textContent).toContain("120,000 of 200,000 tokens · 60%");
    } finally {
      view.unmount();
    }
  });

  test("a nearly full window says what happens from here on", () => {
    const view = render(<Host usage={{ contextWindow: 200_000, tokens: 184_210 }} />);
    try {
      fireEvent.click(screen.getByRole("button", { name: "Context window, 92 percent used" }));
      expect(screen.getByRole("dialog").textContent).toContain("This window is nearly full.");
    } finally {
      view.unmount();
    }

    // The window below the threshold keeps the plain wording: 79 percent is not nearly full.
    const filling = render(<Host usage={{ contextWindow: 200_000, tokens: 158_000 }} />);
    try {
      fireEvent.click(screen.getByRole("button", { name: "Context window, 79 percent used" }));
      expect(screen.getByRole("dialog").textContent).not.toContain("This window is nearly full.");
    } finally {
      filling.unmount();
    }
  });

  test("Escape closes the panel and leaves the focus on the ring", () => {
    const view = render(<Host usage={{ contextWindow: 200_000, tokens: 68_000 }} />);
    try {
      const ring = screen.getByTestId("agent-context-ring");
      fireEvent.click(ring);
      expect(screen.getByTestId("agent-context-popover")).toBeTruthy();

      fireEvent.keyDown(document, { key: "Escape" });
      expect(screen.queryByTestId("agent-context-popover")).toBeNull();
      expect(ring.getAttribute("aria-expanded")).toBe("false");
      expect(document.activeElement).toBe(ring);
    } finally {
      view.unmount();
    }
  });

  test("a pointer press outside closes the panel, one inside leaves it open", () => {
    const view = render(<Host usage={{ contextWindow: 200_000, tokens: 68_000 }} />);
    try {
      fireEvent.click(screen.getByTestId("agent-context-ring"));
      const panel = screen.getByTestId("agent-context-popover");
      fireEvent.pointerDown(panel);
      expect(screen.queryByTestId("agent-context-popover")).toBeTruthy();

      fireEvent.pointerDown(document.body);
      expect(screen.queryByTestId("agent-context-popover")).toBeNull();
    } finally {
      view.unmount();
    }
  });
});
