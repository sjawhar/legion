import { expect, spyOn, test } from "bun:test";
import { act, render } from "@testing-library/react";
import { type ReactNode, useCallback, useRef } from "react";

import { type LiveFollowControl, useLiveFollow } from "./use-live-follow";

// happy-dom has no layout engine and no ResizeObserver, so the harness is the geometry: the
// viewport is `VIEWPORT_HEIGHT` tall with its top edge at `VIEWPORT_TOP` on the screen, every
// turn is `TURN_HEIGHT` tall, and the content is as tall as the turns it holds plus whatever a
// streaming turn or a collapsing fold has added (`extraHeight`). Reads of `scrollTop` clamp to
// the scrollable range, as a browser's do; writes store what they were given.
const TURN_HEIGHT = 100;
const TURN_SELECTOR = "[data-message-id]";
const VIEWPORT_HEIGHT = 300;
const VIEWPORT_TESTID = "live-viewport";
const VIEWPORT_TOP = 50;

interface Geometry {
  /** Content beyond the rendered turns: the text a turn is streaming, or a fold collapsing when
   *  negative. */
  extraHeight: number;
  /** What the element stores, before a read clamps it. */
  scrollTop: number;
}

interface Props {
  contentIds: string[];
  ownSendCount: number;
  replayCount: number;
  sessionId: string;
}

function box(top: number, height: number): DOMRect {
  return {
    bottom: top + height,
    height,
    left: 0,
    right: 100,
    toJSON: () => ({}),
    top,
    width: 100,
    x: 0,
    y: top,
  };
}

function installGeometry(node: HTMLElement, geometry: Geometry): void {
  const contentHeight = () =>
    node.querySelectorAll(TURN_SELECTOR).length * TURN_HEIGHT + geometry.extraHeight;
  Object.defineProperty(node, "clientHeight", { configurable: true, get: () => VIEWPORT_HEIGHT });
  Object.defineProperty(node, "scrollHeight", { configurable: true, get: contentHeight });
  Object.defineProperty(node, "scrollTop", {
    configurable: true,
    get: () => Math.max(0, Math.min(geometry.scrollTop, contentHeight() - VIEWPORT_HEIGHT)),
    set: (value: number) => {
      geometry.scrollTop = value;
    },
  });
}

function installRects(): () => void {
  const spy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const viewport = this.closest<HTMLElement>(`[data-testid="${VIEWPORT_TESTID}"]`);
    if (viewport === null) {
      return box(0, 0);
    }
    if (viewport === this) {
      return box(VIEWPORT_TOP, VIEWPORT_HEIGHT);
    }
    const index = [...viewport.querySelectorAll(TURN_SELECTOR)].indexOf(this);
    return index === -1
      ? box(0, 0)
      : box(VIEWPORT_TOP + index * TURN_HEIGHT - viewport.scrollTop, TURN_HEIGHT);
  });
  return () => spy.mockRestore();
}

function installResizeObserver(): { fire: () => void; restore: () => void } {
  const original = globalThis.ResizeObserver;
  const live = new Set<ObserverStub>();

  class ObserverStub {
    readonly callback: ResizeObserverCallback;

    constructor(callback: ResizeObserverCallback) {
      this.callback = callback;
      live.add(this);
    }

    disconnect(): void {
      live.delete(this);
    }

    observe(): void {}

    unobserve(): void {}
  }

  globalThis.ResizeObserver = ObserverStub as unknown as typeof ResizeObserver;
  return {
    fire: () => {
      for (const observer of [...live]) {
        observer.callback([], observer as unknown as ResizeObserver);
      }
    },
    restore: () => {
      globalThis.ResizeObserver = original;
    },
  };
}

function Harness({
  contentIds,
  control,
  geometry,
  ownSendCount,
  replayCount,
  sessionId,
}: Props & {
  control: { current: LiveFollowControl | null };
  geometry: Geometry;
}): ReactNode {
  const viewport = useRef<HTMLElement | null>(null);
  // The geometry must be on the element before the hook's own layout effects read it, and React
  // attaches a child's ref before the parent's effects run.
  const attach = useCallback(
    (node: HTMLDivElement | null) => {
      viewport.current = node;
      if (node !== null) {
        installGeometry(node, geometry);
      }
    },
    [geometry]
  );
  control.current = useLiveFollow({ contentIds, ownSendCount, replayCount, sessionId, viewport });
  return (
    <div data-testid={VIEWPORT_TESTID} ref={attach}>
      {/* The content wrapper, whose height is the transcript's: the hook observes it too. */}
      <div>
        {contentIds.map((id) => (
          <div data-message-id={id} key={id} />
        ))}
      </div>
    </div>
  );
}

interface LiveFollowHarness {
  /** Whether the hook reports the viewport following the live end. */
  readonly atEnd: () => boolean;
  readonly count: () => number;
  readonly jumpToEnd: () => void;
  /** The reader's position, as the element reports it. */
  readonly position: () => number;
  /** A content or viewport resize the browser reports to the hook's observer. */
  readonly resize: () => void;
  /** One scroll event, after whatever it changed about the geometry. */
  readonly scroll: (change: Partial<Geometry>) => void;
  readonly stop: () => void;
  /** A render with changed props, as the view's own data would make. */
  readonly update: (next: Partial<Props>) => void;
}

function startHarness(contentIds: string[]): LiveFollowHarness {
  const geometry: Geometry = { extraHeight: 0, scrollTop: 0 };
  const control: { current: LiveFollowControl | null } = { current: null };
  const restoreRects = installRects();
  const sizes = installResizeObserver();
  let props: Props = { contentIds, ownSendCount: 0, replayCount: 0, sessionId: "session-1" };
  const view = render(<Harness {...props} control={control} geometry={geometry} />);
  const element = view.getByTestId(VIEWPORT_TESTID);
  const read = () => {
    const current = control.current;
    if (current === null) {
      throw new Error("the harness rendered no hook");
    }
    return current;
  };
  return {
    atEnd: () => read().atEnd,
    count: () => read().newCount,
    jumpToEnd: () => {
      act(() => {
        read().jumpToEnd();
      });
    },
    position: () => element.scrollTop,
    resize: () => {
      act(() => {
        sizes.fire();
      });
    },
    scroll: (change) => {
      act(() => {
        Object.assign(geometry, change);
        element.dispatchEvent(new Event("scroll"));
      });
    },
    stop: () => {
      view.unmount();
      sizes.restore();
      restoreRects();
    },
    update: (next) => {
      props = { ...props, ...next };
      act(() => {
        view.rerender(<Harness {...props} control={control} geometry={geometry} />);
      });
    },
  };
}

/** Six turns: 600 px of content in a 300 px viewport, so the live end is `scrollTop` 300. */
const SIX = ["a", "b", "c", "d", "e", "f"];

test("a fresh mount lands at the live end, following", () => {
  const live = startHarness([...SIX]);

  try {
    expect(live.position()).toBe(300);
    expect(live.atEnd()).toBe(true);
    expect(live.count()).toBe(0);
  } finally {
    live.stop();
  }
});

test("content arriving while following keeps the viewport at the live end", () => {
  const live = startHarness([...SIX]);

  try {
    live.update({ contentIds: [...SIX, "g"] });

    expect(live.position()).toBe(400);
    expect(live.atEnd()).toBe(true);
    expect(live.count()).toBe(0);
  } finally {
    live.stop();
  }
});

test("a scroll back stops following even though the content grew in the same event", () => {
  const live = startHarness([...SIX]);

  try {
    // The reader scrolls back while the newest turn is streaming: `scrollHeight` grows in the
    // same event that `scrollTop` falls. assistant-ui's rule, which demands an unchanged
    // `scrollHeight`, would keep following here and yank the reader down with the next turn.
    live.scroll({ extraHeight: 200, scrollTop: 200 });

    expect(live.atEnd()).toBe(false);
    expect(live.position()).toBe(200);

    live.update({ contentIds: [...SIX, "g"] });

    expect(live.position()).toBe(200);
  } finally {
    live.stop();
  }
});

test("a shrink that pulls scrollTop down does not stop following", () => {
  const live = startHarness([...SIX]);

  try {
    // A fold collapses: 200 px of content goes, and because most of it was above the reader the
    // browser lowers `scrollTop` by more than the clamp alone would. No reader acted, so
    // following must survive it - and the resize that follows settles back at the live end.
    live.scroll({ extraHeight: -200, scrollTop: 0 });

    expect(live.atEnd()).toBe(true);

    live.resize();

    expect(live.position()).toBe(100);
    expect(live.atEnd()).toBe(true);
  } finally {
    live.stop();
  }
});

test("the count holds the ids the reader has not seen", () => {
  const live = startHarness([...SIX]);

  try {
    live.scroll({ scrollTop: 100 });
    expect(live.atEnd()).toBe(false);
    expect(live.count()).toBe(0);

    live.update({ contentIds: [...SIX, "g"] });
    expect(live.count()).toBe(1);

    // The same turn re-rendered under its own id as its text grows: still one new turn.
    live.update({ contentIds: [...SIX, "g"] });
    expect(live.count()).toBe(1);

    // The oldest turn left the list; it was seen, and it is simply absent.
    live.update({ contentIds: ["b", "c", "d", "e", "f", "g"] });
    expect(live.count()).toBe(1);
    expect(live.position()).toBe(100);
  } finally {
    live.stop();
  }
});

test("scrolling back to the live end clears the count and resumes following", () => {
  const live = startHarness([...SIX]);

  try {
    live.scroll({ scrollTop: 100 });
    live.update({ contentIds: [...SIX, "g"] });
    expect(live.count()).toBe(1);

    live.scroll({ scrollTop: 400 });

    expect(live.atEnd()).toBe(true);
    expect(live.count()).toBe(0);
    expect(live.position()).toBe(400);
  } finally {
    live.stop();
  }
});

test("jumpToEnd clears the count and resumes following", () => {
  const live = startHarness([...SIX]);

  try {
    live.scroll({ scrollTop: 100 });
    live.update({ contentIds: [...SIX, "g"] });
    expect(live.count()).toBe(1);

    live.jumpToEnd();

    expect(live.position()).toBe(400);
    expect(live.atEnd()).toBe(true);
    expect(live.count()).toBe(0);
  } finally {
    live.stop();
  }
});

test("an own send and a new session each force the live end", () => {
  const live = startHarness([...SIX]);

  try {
    live.scroll({ scrollTop: 100 });
    expect(live.atEnd()).toBe(false);

    // The send's own callback can land a render before the message it sent reaches the
    // transcript, so the bump alone follows.
    live.update({ ownSendCount: 1 });
    expect(live.position()).toBe(300);
    expect(live.atEnd()).toBe(true);

    live.scroll({ scrollTop: 100 });
    expect(live.atEnd()).toBe(false);

    live.update({ sessionId: "session-2" });
    expect(live.position()).toBe(300);
    expect(live.atEnd()).toBe(true);
  } finally {
    live.stop();
  }
});

test("a rebuild that still holds the remembered turn keeps it where the reader had it", () => {
  const live = startHarness([...SIX]);

  try {
    // At `scrollTop` 150 the topmost turn on screen is `b`, its top 50 px above the viewport's
    // own top edge.
    live.scroll({ scrollTop: 150 });
    expect(live.atEnd()).toBe(false);

    // The replay drops the oldest turn and brings a newer one, so `b` is now the first turn in
    // the list: holding `scrollTop` at 150 would put it 150 px above the screen instead of 50.
    live.update({ contentIds: ["b", "c", "d", "e", "f", "g"], replayCount: 1 });

    expect(live.position()).toBe(50);
    expect(live.atEnd()).toBe(false);
    expect(live.count()).toBe(1);
  } finally {
    live.stop();
  }
});

test("a rebuild that dropped the remembered turn goes to the live end", () => {
  const live = startHarness([...SIX]);

  try {
    live.scroll({ scrollTop: 150 });
    expect(live.atEnd()).toBe(false);

    // The replay's budget dropped everything the reader was on, `b` included.
    live.update({ contentIds: ["d", "e", "f", "g", "h", "i"], replayCount: 1 });

    expect(live.position()).toBe(300);
    expect(live.atEnd()).toBe(true);
    expect(live.count()).toBe(0);
  } finally {
    live.stop();
  }
});

test("a rebuild while following stays at the live end", () => {
  const live = startHarness([...SIX]);

  try {
    live.update({ contentIds: ["c", "d", "e", "f", "g", "h", "i", "j"], replayCount: 1 });

    expect(live.position()).toBe(500);
    expect(live.atEnd()).toBe(true);
    expect(live.count()).toBe(0);
  } finally {
    live.stop();
  }
});
