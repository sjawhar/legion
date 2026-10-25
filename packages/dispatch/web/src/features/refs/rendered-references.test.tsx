import { afterAll, afterEach, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import type { ReactNode } from "react";

import { ApiError, api, isRetryableQueryError, type ReferenceSource } from "../../api/client";
import type { IssueDetails, MessageRead } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";
import { MarkdownPreview } from "./MarkdownPreview";
import { warmMarkdownRenderer } from "./markdown-engine";
import { issueQuery, referenceQuestionsQuery, referenceTitle } from "./reference-target";

/**
 * The renderers' half of the shared text table (`DISPATCH_TEXT_REFERENCES`, which the server's
 * `text.Mentions` and the scanner's own test read too): each row's body, drawn by `MarkdownBody`
 * and by the one-line `MarkdownPreview` for a comment whose document's question map is the row's
 * `questions`, with the row's `issues` the only ones that exist, links exactly the references the
 * row says a renderer links (`links`, else `refs`) - none inside code or a link, none for a key
 * whose issue does not exist or a `#id` no question holds - in order of first appearance.
 */

const origin = "https://dispatch.test";
const source: ReferenceSource = { id: "comment-1", kind: "comment" };
const testWindow = window as unknown as HappyDOMWindow;
let pageBefore = "about:blank";

function notFound(): ApiError {
  return new ApiError(404, { code: "NOT_FOUND", error: "not found" });
}

function issueNamed(key: string): IssueDetails {
  return { artifacts: [], key, status: "in_progress", title: `Title of ${key}` } as never;
}

/** The app's own query defaults (`main.tsx`), so a reference query's own retry rule is what runs
 *  and a second reader of a fresh answer asks no second time; retries wait no time here. */
function appQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: (failureCount, error) => failureCount < 2 && isRetryableQueryError(error),
        retryDelay: 0,
        staleTime: 30_000,
      },
    },
  });
}

function withClient(queryClient: QueryClient, children: ReactNode): ReactNode {
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

/** Every reference a rendered element links or, in a preview, shows as link-styled text: its
 *  `dispatch://` form, in document order, each once. */
function linkedReferences(element: HTMLElement, selector: string): string[] {
  const references = Array.from(element.querySelectorAll(selector), (node) =>
    node.getAttribute(
      node.hasAttribute("data-dispatch-picture") ? "data-dispatch-picture" : "data-dispatch-ref"
    )
  );
  return [...new Set(references.filter((reference) => reference !== null))];
}

beforeAll(async () => {
  pageBefore = window.location.href;
  await warmMarkdownRenderer();
});

afterAll(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

afterEach(() => {
  cleanup();
});

/** The API as the row's server answers: the row's issues exist, nothing else a title reads
 *  does, and the comment's document's question map is the row's. */
function answerAsTheRowsServer(
  issues: readonly string[],
  questions: Readonly<Record<string, string>>
): () => void {
  const spies = [
    spyOn(api, "getIssue").mockImplementation(async (key: string) => {
      if (issues.includes(key)) {
        return issueNamed(key);
      }
      throw notFound();
    }),
    spyOn(api, "getReferenceQuestions").mockImplementation(async () => ({
      document: "dispatch://CORE-1/spec",
      questions: Object.entries(questions).map(([block_id, ask]) => ({ ask, block_id })),
    })),
    spyOn(api, "getAsk").mockRejectedValue(notFound()),
    spyOn(api, "getComment").mockRejectedValue(notFound()),
    spyOn(api, "getMessage").mockRejectedValue(notFound()),
    spyOn(api, "getProjectArtifact").mockRejectedValue(notFound()),
    spyOn(api, "getAgentArtifact").mockRejectedValue(notFound()),
  ];
  return () => {
    for (const spy of spies) {
      spy.mockRestore();
    }
  };
}

describe.each([
  ["MarkdownBody", "a[data-dispatch-ref], a[data-dispatch-picture]"],
  ["MarkdownPreview", "[data-dispatch-ref], [data-dispatch-picture]"],
] as const)("%s", (surface, selector) => {
  test.each(
    DISPATCH_TEXT_REFERENCES.map((row) => [row.body, row] as const)
  )("%s links what the table says a renderer links", async (_body, row) => {
    testWindow.happyDOM.setURL(`${row.origin ?? origin}/issues/CORE-1/conversation`);
    const issues = row.issues ?? [];
    const questions = row.questions ?? {};
    const restore = answerAsTheRowsServer(issues, questions);
    const queryClient = appQueryClient();
    // What the issue page and an earlier render already read: the existence answers are in the
    // cache, so the first paint draws what the row's server says.
    for (const key of issues) {
      queryClient.setQueryData(issueQuery(key).queryKey, issueNamed(key));
    }
    queryClient.setQueryData(referenceQuestionsQuery(source).queryKey, {
      document: "dispatch://CORE-1/spec",
      questions: Object.entries(questions).map(([block_id, ask]) => ({ ask, block_id })),
    });
    const view = render(
      withClient(
        queryClient,
        surface === "MarkdownBody" ? (
          <MarkdownBody markdown={row.body} source={source} />
        ) : (
          <MarkdownPreview lines={3} markdown={row.body} source={source} />
        )
      )
    );
    try {
      const element = view.container.firstElementChild as HTMLElement;
      await waitFor(() => expect(element.hasChildNodes()).toBe(true));
      await waitFor(() => expect(queryClient.isFetching()).toBe(0));
      expect(linkedReferences(element, selector)).toEqual([...(row.links ?? row.refs)]);
      if (surface === "MarkdownPreview") {
        // A preview shows its references as words, with no link inside it.
        expect(element.querySelector("a")).toBeNull();
      }
      for (const key of issues) {
        for (const link of element.querySelectorAll(`[data-dispatch-ref="dispatch://${key}"]`)) {
          // A linked key reads as its issue: the key in semibold, then the title.
          expect(link.textContent).toBe(`${key} Title of ${key}`);
          expect(link.querySelector(".font-semibold")?.textContent).toBe(key);
        }
      }
    } finally {
      view.unmount();
      restore();
    }
  });
});

describe("bare references", () => {
  beforeAll(() => {
    testWindow.happyDOM.setURL(`${origin}/issues/CORE-1/conversation`);
  });

  test("a key whose issue does not exist is asked about once and stays its written text", async () => {
    const getIssue = spyOn(api, "getIssue").mockRejectedValue(notFound());
    const view = render(
      withClient(appQueryClient(), <MarkdownBody markdown="Encode it as UTF-8, per CORE-404." />)
    );
    try {
      await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(2));
      // Long enough for a retry the shared rule would make at its `retryDelay` of 0.
      await Bun.sleep(50);
      expect(getIssue.mock.calls.map(([key]) => key).sort()).toEqual(["CORE-404", "UTF-8"]);
      expect(view.container.textContent).toBe("Encode it as UTF-8, per CORE-404.");
      expect(view.container.querySelector("a")).toBeNull();
    } finally {
      view.unmount();
      getIssue.mockRestore();
    }
  });

  test("a key links once its issue answers, reading as the issue's key and title", async () => {
    const getIssue = spyOn(api, "getIssue").mockResolvedValue(issueNamed("CORE-12"));
    const view = render(
      withClient(appQueryClient(), <MarkdownBody markdown="Blocked on CORE-12." />)
    );
    try {
      const link = await within(view.container).findByRole("link", {
        name: "CORE-12 Title of CORE-12",
      });
      expect(link.getAttribute("href")).toBe("/issues/CORE-12");
      expect(link.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-12");
      expect(getIssue).toHaveBeenCalledTimes(1);
    } finally {
      view.unmount();
      getIssue.mockRestore();
    }
  });

  test("a #id with no source stays text and asks for no question map", async () => {
    const getReferenceQuestions = spyOn(api, "getReferenceQuestions");
    const view = render(withClient(appQueryClient(), <MarkdownBody markdown="See #q2 first." />));
    try {
      await waitFor(() => expect(view.container.textContent).toBe("See #q2 first."));
      expect(view.container.querySelector("a")).toBeNull();
      expect(getReferenceQuestions).not.toHaveBeenCalled();
    } finally {
      view.unmount();
      getReferenceQuestions.mockRestore();
    }
  });

  test("a #id with a source links to its question, read as the question's text", async () => {
    const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
      document: "dispatch://CORE-1/spec",
      questions: [{ ask: "dispatch://CORE-1/ask/ask-2", block_id: "q2" }],
    });
    const getAsk = spyOn(api, "getAsk").mockResolvedValue({
      answers: [],
      ask: { id: "ask-2", issue_key: "CORE-1", question: "**Ship** the docs too?" },
      edits: [],
      followers: [],
      replies: [],
    } as never);
    const view = render(
      withClient(
        appQueryClient(),
        <MarkdownBody markdown="As agreed, _see #q2_." source={source} />
      )
    );
    try {
      const link = await within(view.container).findByRole("link", { name: "Ship the docs too?" });
      expect(link.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-2");
      expect(link.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1/ask/ask-2");
      expect(getReferenceQuestions).toHaveBeenCalledTimes(1);
      expect(getReferenceQuestions).toHaveBeenCalledWith(source);
    } finally {
      view.unmount();
      getAsk.mockRestore();
      getReferenceQuestions.mockRestore();
    }
  });

  test("a reference whose target does not exist shows its address as written", async () => {
    const getIssue = spyOn(api, "getIssue").mockRejectedValue(notFound());
    const view = render(
      withClient(appQueryClient(), <MarkdownBody markdown="See dispatch://CORE-999999/spec." />)
    );
    try {
      const link = await within(view.container).findByRole("link");
      await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
      await Bun.sleep(50);
      expect(getIssue).toHaveBeenCalledTimes(1);
      expect(link.textContent).toBe("dispatch://CORE-999999/spec");
    } finally {
      view.unmount();
      getIssue.mockRestore();
    }
  });
});

describe("referenceTitle", () => {
  test("reads a title from the cache by the rule the links draw with", () => {
    const queryClient = appQueryClient();
    queryClient.setQueryData(issueQuery("CORE-12").queryKey, issueNamed("CORE-12"));
    expect(referenceTitle(queryClient, { key: "CORE-12", kind: "issue" })).toMatchObject({
      title: "CORE-12 Title of CORE-12",
      titleKey: "CORE-12",
    });
    // Nothing cached is no title yet; the caller shows what was written.
    expect(referenceTitle(queryClient, { key: "CORE-13", kind: "issue" }).title).toBeUndefined();
  });

  test("a message reads as its text's headline, its whole text the description once cut", () => {
    const queryClient = appQueryClient();
    const body = `**Deploy** finished on every region, with the canary held for an hour before the rest`;
    queryClient.setQueryData(["issue", "CORE-1", "message", "m1"], {
      message: { author: { id: "s1", kind: "session" }, body, id: "m1" },
      replies: [],
    } as unknown as MessageRead);
    const target = referenceTitle(queryClient, { id: "m1", key: "CORE-1", kind: "message" });
    expect(target.title).toBe("Deploy finished on every region, with the canary held for an…");
    expect(target.titleKey).toBeUndefined();
    expect(target.description).toBe(body);
  });
});
