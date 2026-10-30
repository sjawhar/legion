import { expect, test } from "bun:test";
import type { ReplyBlock } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { ComponentProps } from "react";
import { MemoryRouter } from "react-router-dom";

import { passageItem, ReplyQuote } from "./ReplyQuote";

/** A user author, drawn as plain text, so a quote reads `Replying to Planner` with no registry
 *  read. */
const planner = { id: "Planner", kind: "user" } as const;

const passage: ReplyBlock = {
  block_id: "b2",
  node: "comment/11111111-1111-4111-8111-111111111111#b2",
  ref: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b2",
  removed: false,
  text: "Beta.",
};

/** Waits for the quote to read `text` whole once its Markdown has rendered. */
async function expectQuote(props: ComponentProps<typeof ReplyQuote>, text: string): Promise<void> {
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={new QueryClient()}>
        <ReplyQuote {...props} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() => expect(view.container.textContent).toBe(text));
  } finally {
    view.unmount();
  }
}

test("a reply that names a passage quotes the passage as its author's, not the whole parent", async () => {
  await expectQuote(
    { author: planner, block: passage, excerpt: "Alpha.\n\nBeta." },
    "Replying to Planner's passage — Beta."
  );
  // Without the parent loaded the author is unknown, and the quote still names a passage.
  await expectQuote(
    { author: undefined, block: passage, excerpt: "" },
    "Replying to a passage — Beta."
  );
  // A reply that names no block quotes its parent as before.
  await expectQuote(
    { author: planner, block: null, excerpt: "Alpha." },
    "Replying to Planner — Alpha."
  );
});

test("a passage the parent's edit removed says so in place of its last text", async () => {
  const removed = { ...passage, removed: true };
  await expectQuote(
    { author: planner, block: removed, excerpt: "Alpha." },
    "Replying to Planner's passage — This passage was removed."
  );
  await expectQuote(
    { author: undefined, block: removed, excerpt: "" },
    "Replying to a passage — This passage was removed."
  );
});

test("a passage belongs to the item its node names, which can be another reply in the thread", () => {
  const root = { author: planner, id: "11111111-1111-4111-8111-111111111111" };
  const nested = { author: planner, id: "22222222-2222-4222-8222-222222222222" };
  expect(passageItem(passage, [root, nested])).toBe(root);
  expect(passageItem({ ...passage, node: `comment/${nested.id}#b1` }, [root, nested])).toBe(nested);
  expect(passageItem({ ...passage, node: "message/elsewhere#b1" }, [root, nested])).toBeUndefined();
});
