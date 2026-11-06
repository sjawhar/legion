import { expect, test } from "bun:test";
import { renderHook, waitFor } from "@testing-library/react";

import type { AskBlockHost } from "./typed-block-drawing";
import { useAskCardsDrawnInTasks } from "./useAskCardsDrawnInTasks";

/** A host of the shape the hook reads: its key, and the slots the card's portal fills. The node
 *  is the node view's own and nothing here touches it. */
function host(key: number): AskBlockHost {
  return {
    footer: document.createElement("div"),
    header: document.createElement("div"),
    key,
    node: { attrs: { blockId: `block-${key}` } } as unknown as AskBlockHost["node"],
  };
}

const keysOf = (hosts: readonly AskBlockHost[]): number[] => hosts.map((drawn) => drawn.key);

test("no card is drawn in the render the hosts arrive in, and one more is drawn after each task", async () => {
  const hosts = [host(1), host(2), host(3)];
  // Every render's answer, because what this hook promises is the steps it takes rather than
  // where it ends: a `waitFor` poll would see only the last of them.
  const steps: string[] = [];
  const { result } = renderHook(() => {
    const drawn = useAskCardsDrawnInTasks(hosts);
    const step = keysOf(drawn).join(",");
    if (steps.at(-1) !== step) {
      steps.push(step);
    }
    return drawn;
  });

  expect(keysOf(result.current)).toEqual([]);
  await waitFor(() => {
    expect(keysOf(result.current)).toEqual([1, 2, 3]);
  });
  expect(steps).toEqual(["", "1", "1,2", "1,2,3"]);
});

test("a host that arrives above the drawn ones takes nobody's card off the screen", async () => {
  const first = [host(1), host(2)];
  const { result, rerender } = renderHook(
    ({ hosts }: { hosts: readonly AskBlockHost[] }) => useAskCardsDrawnInTasks(hosts),
    { initialProps: { hosts: first } }
  );
  await waitFor(() => {
    expect(keysOf(result.current)).toEqual([1, 2]);
  });

  rerender({ hosts: [host(3), ...first] });
  expect(keysOf(result.current)).toEqual([1, 2]);
  await waitFor(() => {
    expect(keysOf(result.current)).toEqual([3, 1, 2]);
  });
});

test("a host that leaves the document leaves the drawn set with it", async () => {
  const kept = host(1);
  const { result, rerender } = renderHook(
    ({ hosts }: { hosts: readonly AskBlockHost[] }) => useAskCardsDrawnInTasks(hosts),
    { initialProps: { hosts: [kept, host(2)] } }
  );
  await waitFor(() => {
    expect(keysOf(result.current)).toEqual([1, 2]);
  });

  rerender({ hosts: [kept] });
  expect(keysOf(result.current)).toEqual([1]);
  // The host that left took its key with it, so the one that arrives after it is drawn in a
  // task of its own rather than being taken for a key the set had kept.
  rerender({ hosts: [kept, host(4)] });
  expect(keysOf(result.current)).toEqual([1]);
  await waitFor(() => {
    expect(keysOf(result.current)).toEqual([1, 4]);
  });
});
