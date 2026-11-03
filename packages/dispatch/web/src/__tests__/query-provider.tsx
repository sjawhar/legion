import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderOptions, type RenderResult, render } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";

/**
 * `render` with a query client around what it draws. Every Markdown surface reads what its text
 * cites - the issue keys it writes bare, the question map of its document, the item each id
 * prefix names (`useTextReferences`) - so a body needs a client wherever it is drawn, as it has
 * one everywhere in the app. Each render gets its own, as a fresh page does, and the wrapper is
 * the render's own, so a `rerender` keeps it. A wrapper the caller gives is kept, inside this
 * one; a client it provides there nests inside this one and wins.
 */
export function renderWithQueries(
  ui: Parameters<typeof render>[0],
  options?: RenderOptions
): RenderResult {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const Given = options?.wrapper;
  const Wrapper = ({ children }: { children: ReactNode }): ReactElement => (
    <QueryClientProvider client={queryClient}>
      {Given === undefined ? children : <Given>{children}</Given>}
    </QueryClientProvider>
  );
  return render(ui, { ...options, wrapper: Wrapper });
}
