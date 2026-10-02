import { expect, test } from "bun:test";
import { renderHook } from "@testing-library/react";

import { useSubmitGuard } from "./useSubmitGuard";

test("a submit guard keeps its object and methods across rerenders", () => {
  const { result, rerender, unmount } = renderHook(() => useSubmitGuard());
  const first = result.current;

  try {
    rerender();
    expect(result.current).toBe(first);
    expect(result.current.guard).toBe(first.guard);
    expect(result.current.held).toBe(first.held);
    expect(result.current.release).toBe(first.release);
    expect(result.current.retryLast).toBe(first.retryLast);
  } finally {
    unmount();
  }
});
