import { afterEach, expect, test } from "bun:test";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import {
  afterTheFirstBatch,
  ReaderIsHere,
  theFirstBatchIsAwaited,
  useAfterTheFirstBatch,
} from "./first-batch";

// The gate is one page's state, so each row leaves it as it found it: nothing awaited, which is
// what every page with no document on it reads.
afterEach(() => theFirstBatchIsAwaited(false));

test("a page with no document to come holds no read", () => {
  const { result } = renderHook(() => useAfterTheFirstBatch());

  expect(afterTheFirstBatch()).toBe(true);
  expect(result.current).toBe(true);
});

test("a document address holds the reads until its first batch, and then lets every one of them go", () => {
  act(() => theFirstBatchIsAwaited(true));
  const panel = renderHook(() => useAfterTheFirstBatch());
  const other = renderHook(() => useAfterTheFirstBatch());

  expect(panel.result.current).toBe(false);
  expect(other.result.current).toBe(false);

  act(() => theFirstBatchIsAwaited(false));

  expect(panel.result.current).toBe(true);
  expect(other.result.current).toBe(true);
});

test("a reader who reaches a panel before the batch has its read started then, and keeps it when they leave", () => {
  act(() => theFirstBatchIsAwaited(true));
  const held = renderHook(() => useAfterTheFirstBatch());
  const reached = renderHook(({ at }: { at: boolean }) => useAfterTheFirstBatch(at), {
    initialProps: { at: false },
  });

  expect(reached.result.current).toBe(false);

  reached.rerender({ at: true });
  expect(reached.result.current).toBe(true);
  // Only that panel's read: the rest of the page is still waiting for the batch.
  expect(held.result.current).toBe(false);

  // Closed again before the batch, the read it already made is not taken back.
  reached.rerender({ at: false });
  expect(reached.result.current).toBe(true);
});

test("a read inside a panel the reader opened goes out without the batch", () => {
  act(() => theFirstBatchIsAwaited(true));
  const inPanel = renderHook(() => useAfterTheFirstBatch(), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <ReaderIsHere value={true}>{children}</ReaderIsHere>
    ),
  });
  const outside = renderHook(() => useAfterTheFirstBatch());

  expect(inPanel.result.current).toBe(true);
  expect(outside.result.current).toBe(false);
});
