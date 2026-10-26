import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactNode } from "react";

import type { AskOption } from "../../api/types";
import { AskOptionList } from "./AskOptionList";

/** A label renders through `MarkdownBody`, which reads whether a key written in it names an
 *  issue. */
function renderList(options: AskOption[]) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  return render(<AskOptionList options={options} selected={[]} />, { wrapper });
}

test("an option label with backticks renders inline code with no wrapping <p> or literal backticks", async () => {
  const options: AskOption[] = [{ label: "Ship it `now`" }];
  const view = renderList(options);

  try {
    const code = await view.findByText("now", { selector: "code" });
    expect(code).toBeTruthy();
    expect(code.closest("p")).toBeNull();
    expect(view.getByRole("listitem").textContent).toBe("Ship it now");
  } finally {
    view.unmount();
  }
});

test("a list-only option label flattens to inline text with no nested list item", async () => {
  const options: AskOption[] = [{ label: "- a" }];
  const view = renderList(options);

  try {
    await view.findByText("a");
    const items = view.getAllByRole("listitem");
    expect(items).toHaveLength(1);
    expect(items[0]?.textContent).toBe("a");
  } finally {
    view.unmount();
  }
});
