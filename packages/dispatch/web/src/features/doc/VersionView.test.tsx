import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import type { CreateEditor } from "./editor";
import { DocumentRuntime } from "./runtime";
import { VersionView } from "./VersionView";

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function CurrentRoute() {
  const location = useLocation();
  return <div data-testid="current-route">{location.pathname}</div>;
}

function renderVersionView({
  createEditor,
  highlight,
  markdown,
}: {
  createEditor?: (inner: CreateEditor) => CreateEditor;
  highlight: { by: string; id: string; quote: string } | undefined;
  markdown: string;
}) {
  const runtime = fakeDocumentRuntime();
  if (createEditor !== undefined) {
    runtime.runtime = {
      ...runtime.runtime,
      createEditor: createEditor(runtime.runtime.createEditor),
    };
  }
  const queryClient = createQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["artifact", "artifact-1", "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown,
    named: false,
    number: 1,
    summary: null,
  });
  const blockSchema = runtime.runtime.blockSchema;
  if (blockSchema === undefined) {
    throw new Error("VersionView test runtime must provide a block schema.");
  }
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={runtime.runtime}>
          <VersionView
            artifactId="artifact-1"
            asks={[]}
            blockSchema={blockSchema}
            createdAt="2026-09-09T00:00:00Z"
            highlight={highlight}
            owner={undefined}
            version={1}
          />
          <CurrentRoute />
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { ...runtime, blockSchema, view };
}

test("VersionView renders the version read-only with the highlighted quote focused", async () => {
  const { editors, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "current" },
    markdown: "Read the current specification",
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(
      within(view.container).getByRole("region", { name: "Document version 1" })
    ).toBeDefined();
    expect(editors[0]).toMatchObject({
      focused: ["comment-1"],
      markdown:
        'Read the <span data-proof="comment" data-id="comment-1" data-by="user:alice">current</span> specification',
      readOnly: true,
    });
    expect(editors[0]?.options.awareness).toBeNull();
  } finally {
    view.unmount();
  }
});

test("VersionView renders a typed callout with the fetched block schema", async () => {
  const markdown = ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n';
  const { blockSchema, editors, view } = renderVersionView({
    highlight: undefined,
    markdown,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(blockSchema);
    expect(editors[0]?.markdown).toBe(markdown);
  } finally {
    view.unmount();
  }
});

/** Whether the browser would follow the last click on a link in `root`, read as the click leaves
 * the link. happy-dom follows one nothing prevented by moving the test's page, so each click is
 * prevented there, after the app's own handling. */
function watchLinkClicks(root: HTMLElement): () => boolean | undefined {
  let followed: boolean | undefined;
  for (const link of root.querySelectorAll("a")) {
    link.addEventListener("click", (event) => {
      followed = !event.defaultPrevented;
      event.preventDefault();
    });
  }
  return () => {
    const last = followed;
    followed = undefined;
    return last;
  };
}

test("VersionView opens a dispatch:// link in the app, a text link or a linked picture", async () => {
  const markdown = "See [CORE-2](dispatch://CORE-2) and [![x](a.png)](dispatch://CORE-3)";
  const { editors, view } = renderVersionView({ highlight: undefined, markdown });

  try {
    await waitFor(() => expect(editors[0]?.markdown).toBe(markdown));
    const root = editors[0]?.root;
    if (root === undefined) throw new Error("editor root missing");
    // What the read-only editor draws for the markdown: each link's href sanitized to "" and its
    // target in data-dispatch-href, and a picture linked elsewhere.
    act(() => {
      root.innerHTML =
        '<p>See <a href="" data-dispatch-href="dispatch://CORE-2">CORE-2</a> and' +
        ' <a href="" data-dispatch-href="dispatch://CORE-3" contenteditable="false"><img alt="x" src="a.png"></a>' +
        ' <a href="https://ci.example/build" contenteditable="false"><img alt="build" src="badge.svg"></a></p>';
    });
    const followed = watchLinkClicks(root);
    const route = () => within(view.container).getByTestId("current-route").textContent;

    // A picture linked elsewhere is left to the browser and proof-editor's link plugin.
    fireEvent.click(within(root).getByRole("img", { name: "build" }));
    expect(followed()).toBe(true);
    expect(route()).toBe("/");

    fireEvent.click(within(root).getByText("CORE-2"));
    expect(followed()).toBe(false);
    expect(route()).toBe("/issues/CORE-2");

    // Clicking the picture selects it, which is a non-collapsed selection.
    const picture = within(root).getByRole("img", { name: "x" });
    const range = document.createRange();
    range.selectNode(picture);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
    fireEvent.click(picture);
    expect(followed()).toBe(false);
    expect(route()).toBe("/issues/CORE-3");
  } finally {
    window.getSelection()?.removeAllRanges();
    view.unmount();
  }
});

test("VersionView reports an ambiguous or missing quote as changed text", async () => {
  const { editors, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "o" },
    markdown: "o o",
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.markdown).toBe("o o");
    expect(within(view.container).getByRole("status").textContent).toBe(
      "Text changed. The selected range no longer exists in this document."
    );
  } finally {
    view.unmount();
  }
});

test("VersionView shows the parser's error instead of an empty document when a version cannot be rendered", async () => {
  const { editors, view } = renderVersionView({
    createEditor: (inner) => async (root, options) => {
      const handle = await inner(root, options);
      handle.setMarkdown = () => {
        throw new Error("text directives (:name{...}) are not supported");
      };
      return handle;
    },
    highlight: undefined,
    markdown: "held since 16:25Z",
  });

  try {
    await waitFor(() =>
      expect(within(view.container).getByRole("alert").textContent).toBe(
        "This version could not be rendered: text directives (:name{...}) are not supported"
      )
    );
    expect(editors[0]?.destroyed).toBe(true);
    expect(within(view.container).getByRole("heading").textContent).toMatch(/^Version 1 · /);
  } finally {
    view.unmount();
  }
});
