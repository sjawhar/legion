import { expect, test } from "bun:test";

import { scrollBlockToTop } from "./scroll-block";

const CONTAINER_TOP = 100;

/** A scrolling container whose top edge is at `CONTAINER_TOP`, holding one block whose top edge
 * is `below` pixels under it. happy-dom lays nothing out, so both boxes are the ones stated. */
function containerHolding(blockId: string, below: number) {
  const container = document.createElement("div");
  container.getBoundingClientRect = () => ({ top: CONTAINER_TOP }) as DOMRect;
  const block = document.createElement("section");
  block.setAttribute("data-block-id", blockId);
  block.getBoundingClientRect = () => ({ top: CONTAINER_TOP + below }) as DOMRect;
  container.append(block);
  return container;
}

test("the block the reader chose is scrolled to the top of its container", () => {
  const container = containerHolding("b-1", 240);
  container.scrollTop = 60;

  expect(scrollBlockToTop("b-1", container)).toBe(true);
  expect(container.scrollTop).toBe(300);
});

test("a block the container does not hold answers false and scrolls nothing", () => {
  const container = containerHolding("b-1", 240);
  container.scrollTop = 60;

  expect(scrollBlockToTop("b-2", container)).toBe(false);
  expect(container.scrollTop).toBe(60);
});

test("a container already showing the block at its top is not moved", () => {
  const container = containerHolding("b-1", 0);
  container.scrollTop = 420;

  expect(scrollBlockToTop("b-1", container)).toBe(true);
  expect(container.scrollTop).toBe(420);
});

test("a block id a CSS selector would read as syntax is found by its own name", () => {
  const container = containerHolding("ship it.now", 120);
  container.scrollTop = 0;

  expect(scrollBlockToTop("ship it.now", container)).toBe(true);
  expect(container.scrollTop).toBe(120);
});
