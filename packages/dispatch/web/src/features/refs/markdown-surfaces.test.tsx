import { afterEach, beforeEach, describe, expect, jest, type Mock, spyOn, test } from "bun:test";
import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import type { ReactNode } from "react";
import { Link, MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type {
  BroadcastSummary,
  Comment,
  CommentRead,
  IssueDetails,
  SearchResult,
} from "../../api/types";
import { AgentThread } from "../agent-view/AgentThread";
import { dispatchMetadata } from "../agent-view/dispatch-marks";
import { BroadcastsPage } from "../agents/BroadcastsPage";
import { ReplyQuote } from "../conversation/ReplyQuote";
import { ThreadCard } from "../margin/ThreadCard";
import type { Thread } from "../margin/useMarginItems";
import { SearchPalette } from "../search/SearchPalette";
import { KeymapProvider } from "../shell/KeymapProvider";
import { closeRefPreview, RefPreviewHost, referenceTriggerProps } from "./RefPreview";
import { REF_PREVIEW_OPEN_DELAY_MS } from "./ref-preview-timing";
import { Unfurl } from "./Unfurl";

/**
 * Every surface that shows text a person or an agent wrote, with one Markdown fixture, and
 * what each must never show: a literal markdown character. The fixture holds a heading, bold,
 * a list, a link, a `dispatch://` reference and a code span; a surface that passed the source
 * through as text would print `#`, `**`, `-`, `[`, `](`, a bare `dispatch://…` or a backtick,
 * and the assertion below reads the rendered text for each of those.
 */
const FIXTURE =
  "## Heading\n\n**Bold** then `code` and a [link](https://example.com/docs) to dispatch://CORE-1.\n\n- first\n- second";

/** What the fixture's text reads as once formatted on one line: no syntax, the words in order,
 *  the reference replaced by its resolved title (`Design decision`). */
const RENDERED = /Heading\s+Bold then code and a link to Design decision\.?\s+first second/;

/** The characters a raw-text surface leaks. The bare reference is listed by its scheme, so a
 *  surface that showed `dispatch://CORE-1` unresolved fails too. */
const LITERAL_SYNTAX = ["##", "**", "`", "[link](", "- first", "dispatch://"];

const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  artifacts: [],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  open_asks: [],
  progress: { tasks: null, children: null },
  parent: null,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  primary_artifact_id: "artifact-none",
  priority: null,
  project: "CORE",
  rank: "U",
  referenced_by_count: 0,
  route: null,
  status: "in_progress",
  title: "Design decision",
  updated_at: "2026-09-09T00:00:00Z",
};

const comment: Comment = {
  anchor: null,
  ask_id: null,
  turn: null,
  author: { id: "alice", kind: "user" },
  body: FIXTURE,
  created_at: "2026-09-10T00:00:00Z",
  edited_at: null,
  id: "comment-1",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  ...commentDeliveryFields(),
};

const commentRead: CommentRead = { comment, replies: [] };

function queryClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function Providers({ children }: { children: ReactNode }): ReactNode {
  return (
    <MemoryRouter>
      <QueryClientProvider client={queryClient()}>{children}</QueryClientProvider>
    </MemoryRouter>
  );
}

/** The thread harness `AgentThread.boundary.test.tsx` uses: the real runtime over stored
 *  messages, so the text and reasoning parts go through the library's own part rendering. */
function Transcript({ messages }: { messages: ThreadMessageLike[] }): ReactNode {
  const runtime = useExternalStoreRuntime({
    convertMessage: (message: ThreadMessageLike) => message,
    isRunning: false,
    messages,
    onNew: async () => undefined,
  });
  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <AgentThread empty="Nothing yet." placeholder="Message the session…" resetKey="session-1" />
    </AssistantRuntimeProvider>
  );
}

function expectNoSyntax(element: HTMLElement): void {
  const text = element.textContent ?? "";
  for (const syntax of LITERAL_SYNTAX) {
    expect(text).not.toContain(syntax);
  }
  expect(element.querySelector("strong")?.textContent).toBe("Bold");
  expect(element.querySelector("code")?.textContent).toBe("code");
}

/** A full body: its link is a real one. */
function expectBody(element: HTMLElement): void {
  expectNoSyntax(element);
  expect(element.querySelector("a[href='https://example.com/docs']")).not.toBeNull();
}

/** A one-line preview: the fixture's words in order with no syntax and nothing nested. Every
 *  surface a preview sits in is one control, so its links read as links (the reference keeps its
 *  hover card) and none is a link of its own. */
function expectFormatted(element: HTMLElement): void {
  expectNoSyntax(element);
  expect(element.textContent ?? "").toMatch(RENDERED);
  expect(element.querySelector("a")).toBeNull();
  const links = Array.from(element.querySelectorAll("[data-markdown-link]"), (link) => ({
    reference: link.getAttribute("data-dispatch-ref"),
    text: link.textContent,
  }));
  expect(links).toEqual([
    { reference: null, text: "link" },
    { reference: "dispatch://CORE-1", text: "Design decision" },
  ]);
}

/** A one-line preview whose host is a toggle, not a navigation (the margin's collapsed card):
 *  the words and formatting of `expectFormatted`, with its links kept real, so a click on a
 *  reference is the way out of the card to its target. */
function expectFormattedWithLiveLinks(element: HTMLElement): void {
  expectNoSyntax(element);
  expect(element.textContent ?? "").toMatch(RENDERED);
  expect(element.querySelector("[data-markdown-link]")).toBeNull();
  const links = Array.from(element.querySelectorAll("a"), (link) => ({
    href: link.getAttribute("href"),
    reference: link.getAttribute("data-dispatch-ref"),
    text: link.textContent,
  }));
  expect(links).toEqual([
    { href: "https://example.com/docs", reference: null, text: "link" },
    { href: "/issues/CORE-1", reference: "dispatch://CORE-1", text: "Design decision" },
  ]);
}

/** A clamped preview never nests a block inside its one line. */
function expectOneLine(element: HTMLElement): void {
  expect(element.querySelector("ul, ol, h1, h2, h3, p")).toBeNull();
}

afterEach(() => {
  cleanup();
  jest.useRealTimers();
  closeRefPreview();
});

test("the agent transcript renders assistant and reasoning text as Markdown", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const messages: ThreadMessageLike[] = [
    {
      content: [
        { text: FIXTURE, type: "reasoning" },
        { text: FIXTURE, type: "text" },
      ],
      createdAt: new Date("2026-09-10T00:00:00Z"),
      id: "a1",
      role: "assistant",
      status: { reason: "stop", type: "complete" },
    },
  ];
  const view = render(
    <Providers>
      <Transcript messages={messages} />
    </Providers>
  );
  try {
    const turn = await within(view.container).findByTestId("agent-message-assistant");
    const reasoning = within(turn).getByTestId("agent-reasoning");
    await waitFor(() => expect(turn.textContent).toContain("Design decision"));
    // Both parts are full bodies, not previews: the heading and list render as blocks (so the
    // text runs together where blocks meet), and nothing of the syntax survives.
    expectBody(reasoning);
    expect(turn.querySelectorAll("h2")).toHaveLength(2);
    expect(turn.querySelectorAll("ul")).toHaveLength(2);
    expect(turn.textContent).toContain("Bold then code and a link to Design decision.");
    for (const syntax of LITERAL_SYNTAX) {
      expect(turn.textContent).not.toContain(syntax);
    }
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("the agent transcript renders the viewer's turn, another sender's turn and a Dispatch reply as Markdown", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const messages: ThreadMessageLike[] = [
    {
      content: [{ text: FIXTURE, type: "text" }],
      createdAt: new Date("2026-09-10T00:00:00Z"),
      id: "u1",
      role: "user",
    },
    {
      content: [{ text: FIXTURE, type: "text" }],
      createdAt: new Date("2026-09-10T00:00:01Z"),
      id: "u2",
      metadata: dispatchMetadata({ author: "Planner" }),
      role: "user",
    },
    {
      content: [{ text: FIXTURE, type: "text" }],
      createdAt: new Date("2026-09-10T00:00:02Z"),
      id: "a1",
      metadata: dispatchMetadata({ dispatch: true }),
      role: "assistant",
      status: { reason: "stop", type: "complete" },
    },
  ];
  const view = render(
    <Providers>
      <Transcript messages={messages} />
    </Providers>
  );
  try {
    for (const testId of ["agent-message-user", "agent-message-other", "agent-dispatch-reply"]) {
      const turn = await within(view.container).findByTestId(testId);
      await waitFor(() => expect(turn.textContent).toContain("Design decision"));
      expect(turn.querySelectorAll("h2")).toHaveLength(1);
      expect(turn.querySelectorAll("ul > li")).toHaveLength(2);
      for (const syntax of LITERAL_SYNTAX) {
        expect(turn.textContent).not.toContain(syntax);
      }
    }
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a streaming assistant turn formats what has closed and shows the rest as it stands", async () => {
  const partial: ThreadMessageLike[] = [
    {
      content: [{ text: "**Bold** so far, and a list:\n\n- first\n- sec", type: "text" }],
      createdAt: new Date("2026-09-10T00:00:00Z"),
      id: "a1",
      role: "assistant",
      status: { type: "running" },
    },
  ];
  const view = render(
    <Providers>
      <Transcript messages={partial} />
    </Providers>
  );
  try {
    const turn = await within(view.container).findByTestId("agent-message-assistant");
    await waitFor(() => expect(turn.querySelector("strong")?.textContent).toBe("Bold"));
    expect(turn.querySelectorAll("li").length).toBe(2);
    expect(turn.textContent).toContain("sec");
    expect(turn.textContent).not.toContain("**");
  } finally {
    view.unmount();
  }
});

// LEGION-540 round 2. A model's turn is prose the model wrote with single newlines between
// short lines ("First, check the config.\nThen, verify the credentials."), not hard-wrapped
// Markdown; the document editor's parser reads a single newline as a soft break and joins the
// lines with a space, which read as one run-on line on the live view. A turn keeps its line
// breaks; a list or a paragraph separated by a blank line still formats as a block.
test("a transcript turn keeps single newlines as line breaks and still formats blocks", async () => {
  const prose = "First, check the config file.\nThen, verify the credentials.\nFinally, run it.";
  const messages: ThreadMessageLike[] = [
    {
      content: [{ text: `${prose}\n\n1. one\n2. two\n\n**done**`, type: "text" }],
      createdAt: new Date("2026-09-10T00:00:00Z"),
      id: "a1",
      role: "assistant",
      status: { reason: "stop", type: "complete" },
    },
  ];
  const view = render(
    <Providers>
      <Transcript messages={messages} />
    </Providers>
  );
  try {
    const turn = await within(view.container).findByTestId("agent-message-assistant");
    await waitFor(() => expect(turn.querySelector("strong")?.textContent).toBe("done"));
    const first = turn.querySelector("p");
    if (first === null) throw new Error("the turn rendered no paragraph");
    expect(first.querySelectorAll("br")).toHaveLength(2);
    expect(first.textContent).toBe(prose.replace(/\n/g, ""));
    expect(Array.from(turn.querySelectorAll("ol > li"), (li) => li.textContent)).toEqual([
      "one",
      "two",
    ]);
  } finally {
    view.unmount();
  }
});

test("the reference hover card renders a comment's body and an ask's question formatted", async () => {
  const getComment = spyOn(api, "getComment").mockResolvedValue(commentRead);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  jest.useFakeTimers();
  const view = render(
    <Providers>
      <Link
        to="/issues/CORE-1/comments/comment-1"
        {...referenceTriggerProps({ id: "comment-1", key: "CORE-1", kind: "comment" })}
      >
        the comment
      </Link>
      <RefPreviewHost />
    </Providers>
  );
  try {
    fireEvent.pointerOver(screen.getByRole("link", { name: "the comment" }), {
      pointerType: "mouse",
    });
    act(() => {
      jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
    });
    const card = screen.getByRole("tooltip");
    // The card is open; from here the queries settle on real microtasks and `waitFor` polls
    // real timers, so the fake clock goes back before anything waits on it.
    jest.useRealTimers();
    await waitFor(() => expect(card.textContent).toContain("Design decision"));
    const body = card.querySelector<HTMLElement>("[data-markdown-preview]");
    if (body === null) throw new Error("the hover card body is not a MarkdownPreview");
    expectFormatted(body);
    expectOneLine(body);
  } finally {
    view.unmount();
    getComment.mockRestore();
    getIssue.mockRestore();
  }
});

test("the unfurl card under a bare reference renders the target's text formatted and its title without syntax", async () => {
  // Long enough that the title has to cut it, so the card shows the whole text under the title.
  const long = `${FIXTURE}\n- and a third item, which runs the title past its cut`;
  const getComment = spyOn(api, "getComment").mockResolvedValue({
    comment: { ...comment, body: long },
    replies: [],
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const view = render(
    <Providers>
      <Unfurl body="dispatch://CORE-1/comment/comment-1" />
    </Providers>
  );
  try {
    // The card is the one link; the body's own links and references inside it are inert text.
    const card = await within(view.container).findByRole("link", { name: /^Heading Bold/ });
    await waitFor(() => expect(card.textContent).toContain("Design decision"));
    const body = card.querySelector<HTMLElement>("[data-markdown-preview]");
    if (body === null) throw new Error("the unfurl body is not a MarkdownPreview");
    expectFormatted(body);
    expectOneLine(body);
    // The card's title is the comment's words as plain text, cut to a title's length: a link's
    // text can hold no formatting or nested link, so the markup is dropped and the reference
    // reads as its short form, as a body's reference link does before its title resolves.
    const title = card.firstElementChild?.textContent ?? "";
    expect(title).toStartWith("Heading Bold then code and a link to CORE-1. first second");
    expect(title).toEndWith("…");
    for (const syntax of LITERAL_SYNTAX) {
      expect(title).not.toContain(syntax);
    }
  } finally {
    view.unmount();
    getComment.mockRestore();
    getIssue.mockRestore();
  }
});

test("a reply quote renders the quoted parent formatted on one line after the app's own prefix", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const view = render(
    <Providers>
      <ReplyQuote author="Planner" excerpt={FIXTURE} to="/issues/CORE-1/messages/message-1" />
    </Providers>
  );
  try {
    // The quote is a link holding links (the body's own); it is found by its own words.
    const quote = await within(view.container).findByRole("link", { name: /^Replying to Planner/ });
    await waitFor(() => expect(quote.textContent).toContain("Design decision"));
    expect(quote.textContent?.startsWith("Replying to Planner — ")).toBe(true);
    const body = quote.querySelector<HTMLElement>("[data-markdown-preview]");
    if (body === null) throw new Error("the quote is not a MarkdownPreview");
    expectFormatted(body);
    expectOneLine(body);
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("the margin's collapsed thread preview renders the root formatted on one line", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const thread: Thread = {
    anchor: null,
    key: comment.id,
    lastReplyAt: undefined,
    replies: [],
    resolved: false,
    root: { comment, kind: "comment" },
  };
  const view = render(
    <Providers>
      <ThreadCard
        actionFailure={undefined}
        artifactSlug="spec"
        editingCommentId={undefined}
        expanded={false}
        hovered={false}
        isClosed={false}
        onAction={() => {}}
        onEdit={async () => undefined}
        onEditingChange={() => {}}
        onRetryAction={() => {}}
        onToggle={() => {}}
        owner={{ key: "CORE-1", kind: "issue" }}
        pendingAction={false}
        savingCommentEditId={undefined}
        thread={thread}
        viewerLogin="alice"
      />
    </Providers>
  );
  try {
    const card = await within(view.container).findByTestId(`margin-comment-${comment.id}`);
    await waitFor(() => expect(card.textContent).toContain("Design decision"));
    const body = card.querySelector<HTMLElement>("[data-markdown-preview]");
    if (body === null) throw new Error("the collapsed preview is not a MarkdownPreview");
    expectFormattedWithLiveLinks(body);
    expectOneLine(body);
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a search hit's snippet renders formatted with the matched words still highlighted", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const hit: SearchResult = {
    href: "/issues/CORE-1/comments/comment-1",
    id: "comment-1",
    kind: "comment",
    owner: { key: "CORE-1", kind: "issue", status: "in_progress", title: "Design decision" },
    rank: 1,
    snippet: FIXTURE.replace("**Bold**", "**<mark>Bold</mark>**").replace(
      "second",
      "<mark>second</mark>"
    ),
  };
  const search = spyOn(api, "search").mockResolvedValue({ results: [hit], took_ms: 1 });
  const view = render(
    <Providers>
      <KeymapProvider>
        <SearchPalette mode="search" onClose={() => {}} />
      </KeymapProvider>
    </Providers>
  );
  try {
    fireEvent.change(screen.getByRole("combobox", { name: "Search" }), {
      target: { value: "bold second" },
    });
    const option = await screen.findByRole("option");
    await waitFor(() => expect(option.textContent).toContain("Design decision"));
    const body = option.querySelector<HTMLElement>("[data-markdown-preview]");
    if (body === null) throw new Error("the search snippet is not a MarkdownPreview");
    expectFormatted(body);
    expectOneLine(body);
    const marks = Array.from(body.querySelectorAll("mark"), (mark) => mark.textContent);
    expect(marks).toEqual(["Bold", "second"]);
    // The highlight sits inside the formatting, not around the syntax.
    expect(body.querySelector("strong mark")?.textContent).toBe("Bold");
  } finally {
    view.unmount();
    search.mockRestore();
    getIssue.mockRestore();
  }
});

test("the broadcasts list shows each broadcast's first line formatted", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const sent: BroadcastSummary = {
    author: { id: "alice", kind: "user" },
    body: FIXTURE,
    created_at: "2026-09-10T00:00:00Z",
    delivery: "steer",
    id: "broadcast-1",
    recipients: 2,
    replies: 1,
  };
  const listBroadcasts = spyOn(api, "listBroadcasts").mockResolvedValue([sent]);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = render(
    <Providers>
      <BroadcastsPage />
    </Providers>
  );
  try {
    const row = await within(view.container).findByRole("link", { name: /answered/ });
    await waitFor(() => expect(row.textContent).toContain("Design decision"));
    const body = row.querySelector<HTMLElement>("[data-markdown-preview]");
    if (body === null) throw new Error("the broadcast row is not a MarkdownPreview");
    expectFormatted(body);
    expectOneLine(body);
  } finally {
    view.unmount();
    listBroadcasts.mockRestore();
    listAgents.mockRestore();
    getIssue.mockRestore();
  }
});

/**
 * A picture someone pasted, as the composer writes it (`uploadedFileText`): an artifact at a
 * version, captioned with its file name. A surface that renders through `MarkdownBody` shows it at
 * the column's width, named by its caption; one that renders through `MarkdownPreview` shows its
 * 40 px thumbnail beside the caption on the preview's one line. `DispatchPicture` builds the
 * address from the reference alone, so the issue read lists no such artifact: a picture that fell
 * back to its reference (`RefLink`) draws no `<img>` at all and fails every assertion below.
 */
const PICTURE = "The broken layout: ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)";

/** The version's bytes route the picture loads from (`artifactVersionPath`). */
const PICTURE_SRC = "/api/v1/issues/CORE-1/artifacts/shot-png/versions/1";

/** What a preview of `PICTURE` reads as: its words, then the caption beside the thumbnail. */
const PICTURE_WORDS = "The broken layout: shot.png";

/** The picture's syntax, none of which a surface that formats it shows. */
const PICTURE_SYNTAX = ["![", "](", "dispatch://"];

function words(element: HTMLElement): string {
  return (element.textContent ?? "").replace(/\s+/g, " ").trim();
}

/** A full body's picture: an `<img>` named by its caption, at the column's width and capped in
 *  height (`DispatchPicture`'s `block`), never the preview's thumbnail. */
async function expectBlockPicture(element: HTMLElement): Promise<void> {
  const picture = await within(element).findByRole("img", { name: "shot.png" });
  expect(picture.getAttribute("src")).toBe(PICTURE_SRC);
  const classes = picture.className.split(" ");
  expect(classes).toEqual(expect.arrayContaining(["max-h-96", "max-w-full", "object-contain"]));
  expect(classes).not.toContain("h-10");
  for (const syntax of PICTURE_SYNTAX) {
    expect(element.textContent).not.toContain(syntax);
  }
}

/** A one-line preview's picture: the 40 px thumbnail (`DispatchPicture`'s `inline`), unnamed
 *  since its caption is the text beside it, in the placeholder the preview leaves for it -
 *  link-styled text where the host is the one link, a link to the picture's page where the
 *  preview's links stay live - and the preview reading `text`, its syntax gone. */
async function expectThumbnail(
  element: HTMLElement,
  links: "inert" | "live",
  text = PICTURE_WORDS
): Promise<void> {
  const thumbnail = await waitFor(() => {
    const image = element.querySelector("img");
    if (image === null) throw new Error("the preview shows no picture");
    return image;
  });
  expect(thumbnail.getAttribute("src")).toBe(PICTURE_SRC);
  expect(thumbnail.getAttribute("alt")).toBe("");
  expect(thumbnail.className.split(" ")).toEqual(
    expect.arrayContaining(["h-10", "w-10", "object-cover"])
  );
  const placeholder = thumbnail.closest<HTMLElement>("[data-dispatch-picture]");
  if (placeholder === null) throw new Error("the thumbnail is not in the picture's placeholder");
  expect(words(placeholder)).toBe("shot.png");
  expect(words(element)).toBe(text);
  if (links === "inert") {
    expect(placeholder.tagName).toBe("SPAN");
    expect(element.querySelector("a")).toBeNull();
  } else {
    expect(placeholder.tagName).toBe("A");
    expect(placeholder.getAttribute("href")).toBe("/issues/CORE-1/artifacts/shot-png?v=1");
  }
}

describe("a Dispatch picture on every surface", () => {
  // The dashboard is served from an origin, where the picture's same-origin bytes route resolves;
  // at happy-dom's `about:blank` every picture would fail at once and fall back to its reference
  // (`MarkdownBody.test.tsx`). The test DOM is happy-dom's window (`__tests__/setup.ts`).
  const testWindow = window as unknown as HappyDOMWindow;
  let pageBefore = "about:blank";
  let getIssue: Mock<typeof api.getIssue>;

  beforeEach(() => {
    pageBefore = window.location.href;
    testWindow.happyDOM.setURL("https://dispatch.test/issues/CORE-1/conversation");
    // A picture that fell back to its reference resolves it against this issue, which lists no
    // artifact, rather than reaching the network.
    getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  });

  afterEach(() => {
    getIssue.mockRestore();
    testWindow.happyDOM.setURL(pageBefore);
  });

  test("every kind of transcript turn shows it at the column's width", async () => {
    const messages: ThreadMessageLike[] = [
      {
        content: [
          { text: PICTURE, type: "reasoning" },
          { text: PICTURE, type: "text" },
        ],
        createdAt: new Date("2026-09-10T00:00:00Z"),
        id: "a1",
        role: "assistant",
        status: { reason: "stop", type: "complete" },
      },
      {
        content: [{ text: PICTURE, type: "text" }],
        createdAt: new Date("2026-09-10T00:00:01Z"),
        id: "u1",
        role: "user",
      },
      {
        content: [{ text: PICTURE, type: "text" }],
        createdAt: new Date("2026-09-10T00:00:02Z"),
        id: "u2",
        metadata: dispatchMetadata({ author: "Planner" }),
        role: "user",
      },
      {
        content: [{ text: PICTURE, type: "text" }],
        createdAt: new Date("2026-09-10T00:00:03Z"),
        id: "a2",
        metadata: dispatchMetadata({ dispatch: true }),
        role: "assistant",
        status: { reason: "stop", type: "complete" },
      },
    ];
    const view = render(
      <Providers>
        <Transcript messages={messages} />
      </Providers>
    );
    try {
      const assistant = await within(view.container).findByTestId("agent-message-assistant");
      await expectBlockPicture(within(assistant).getByTestId("agent-reasoning"));
      expect(within(assistant).getAllByRole("img", { name: "shot.png" })).toHaveLength(2);
      for (const testId of ["agent-message-user", "agent-message-other", "agent-dispatch-reply"]) {
        await expectBlockPicture(await within(view.container).findByTestId(testId));
      }
    } finally {
      view.unmount();
    }
  });

  test("a streaming assistant turn shows it at the column's width", async () => {
    const partial: ThreadMessageLike[] = [
      {
        content: [{ text: `${PICTURE}\n\n- first\n- sec`, type: "text" }],
        createdAt: new Date("2026-09-10T00:00:00Z"),
        id: "a1",
        role: "assistant",
        status: { type: "running" },
      },
    ];
    const view = render(
      <Providers>
        <Transcript messages={partial} />
      </Providers>
    );
    try {
      await expectBlockPicture(
        await within(view.container).findByTestId("agent-message-assistant")
      );
    } finally {
      view.unmount();
    }
  });

  test("the reference hover card shows it in a comment's body as a thumbnail beside its caption", async () => {
    const getComment = spyOn(api, "getComment").mockResolvedValue({
      comment: { ...comment, body: PICTURE },
      replies: [],
    });
    jest.useFakeTimers();
    const view = render(
      <Providers>
        <Link
          to="/issues/CORE-1/comments/comment-1"
          {...referenceTriggerProps({ id: "comment-1", key: "CORE-1", kind: "comment" })}
        >
          the comment
        </Link>
        <RefPreviewHost />
      </Providers>
    );
    try {
      fireEvent.pointerOver(screen.getByRole("link", { name: "the comment" }), {
        pointerType: "mouse",
      });
      act(() => {
        jest.advanceTimersByTime(REF_PREVIEW_OPEN_DELAY_MS);
      });
      const card = screen.getByRole("tooltip");
      jest.useRealTimers();
      const body = await waitFor(() => {
        const preview = card.querySelector<HTMLElement>("[data-markdown-preview]");
        if (preview === null) throw new Error("the hover card body is not a MarkdownPreview");
        return preview;
      });
      await expectThumbnail(body, "inert");
      expectOneLine(body);
    } finally {
      view.unmount();
      getComment.mockRestore();
    }
  });

  test("the unfurl card shows it in the target's text as a thumbnail beside its caption", async () => {
    // Long enough that the title has to cut it, so the card shows the whole text under the title.
    const prose = "After the deploy every column on the board was drawn twice.";
    const long = `${prose} ${PICTURE}`;
    const getComment = spyOn(api, "getComment").mockResolvedValue({
      comment: { ...comment, body: long },
      replies: [],
    });
    const view = render(
      <Providers>
        <Unfurl body="dispatch://CORE-1/comment/comment-1" />
      </Providers>
    );
    try {
      const card = await within(view.container).findByRole("link", { name: /^After the deploy/ });
      const body = card.querySelector<HTMLElement>("[data-markdown-preview]");
      if (body === null) throw new Error("the unfurl body is not a MarkdownPreview");
      await expectThumbnail(body, "inert", `${prose} ${PICTURE_WORDS}`);
      expectOneLine(body);
    } finally {
      view.unmount();
      getComment.mockRestore();
    }
  });

  test("a reply quote shows it in the quoted parent as a thumbnail beside its caption", async () => {
    const view = render(
      <Providers>
        <ReplyQuote author="Planner" excerpt={PICTURE} to="/issues/CORE-1/messages/message-1" />
      </Providers>
    );
    try {
      const quote = await within(view.container).findByRole("link", {
        name: /^Replying to Planner/,
      });
      const body = quote.querySelector<HTMLElement>("[data-markdown-preview]");
      if (body === null) throw new Error("the quote is not a MarkdownPreview");
      await expectThumbnail(body, "inert", `Replying to Planner — ${PICTURE_WORDS}`);
      expectOneLine(body);
    } finally {
      view.unmount();
    }
  });

  test("the margin's collapsed thread preview shows it as a thumbnail linked to the picture's page", async () => {
    const thread: Thread = {
      anchor: null,
      key: comment.id,
      lastReplyAt: undefined,
      replies: [],
      resolved: false,
      root: { comment: { ...comment, body: PICTURE }, kind: "comment" },
    };
    const view = render(
      <Providers>
        <ThreadCard
          actionFailure={undefined}
          artifactSlug="spec"
          editingCommentId={undefined}
          expanded={false}
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => undefined}
          onEditingChange={() => {}}
          onRetryAction={() => {}}
          onToggle={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          savingCommentEditId={undefined}
          thread={thread}
          viewerLogin="alice"
        />
      </Providers>
    );
    try {
      const card = await within(view.container).findByTestId(`margin-comment-${comment.id}`);
      const body = card.querySelector<HTMLElement>("[data-markdown-preview]");
      if (body === null) throw new Error("the collapsed preview is not a MarkdownPreview");
      await expectThumbnail(body, "live");
      expectOneLine(body);
    } finally {
      view.unmount();
    }
  });

  test("a search hit's snippet shows it as a thumbnail beside its caption, the matched word still highlighted", async () => {
    const hit: SearchResult = {
      href: "/issues/CORE-1/comments/comment-1",
      id: "comment-1",
      kind: "comment",
      owner: { key: "CORE-1", kind: "issue", status: "in_progress", title: "Design decision" },
      rank: 1,
      snippet: PICTURE.replace("layout", "<mark>layout</mark>"),
    };
    const search = spyOn(api, "search").mockResolvedValue({ results: [hit], took_ms: 1 });
    const view = render(
      <Providers>
        <KeymapProvider>
          <SearchPalette mode="search" onClose={() => {}} />
        </KeymapProvider>
      </Providers>
    );
    try {
      fireEvent.change(screen.getByRole("combobox", { name: "Search" }), {
        target: { value: "layout" },
      });
      const option = await screen.findByRole("option");
      const body = option.querySelector<HTMLElement>("[data-markdown-preview]");
      if (body === null) throw new Error("the search snippet is not a MarkdownPreview");
      await expectThumbnail(body, "inert");
      expectOneLine(body);
      expect(Array.from(body.querySelectorAll("mark"), (mark) => mark.textContent)).toEqual([
        "layout",
      ]);
    } finally {
      view.unmount();
      search.mockRestore();
    }
  });

  test("the broadcasts list shows it in a broadcast's first line as a thumbnail beside its caption", async () => {
    const sent: BroadcastSummary = {
      author: { id: "alice", kind: "user" },
      body: PICTURE,
      created_at: "2026-09-10T00:00:00Z",
      delivery: "steer",
      id: "broadcast-1",
      recipients: 2,
      replies: 1,
    };
    const listBroadcasts = spyOn(api, "listBroadcasts").mockResolvedValue([sent]);
    const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
    const view = render(
      <Providers>
        <BroadcastsPage />
      </Providers>
    );
    try {
      const row = await within(view.container).findByRole("link", { name: /answered/ });
      const body = row.querySelector<HTMLElement>("[data-markdown-preview]");
      if (body === null) throw new Error("the broadcast row is not a MarkdownPreview");
      await expectThumbnail(body, "inert");
      expectOneLine(body);
    } finally {
      view.unmount();
      listBroadcasts.mockRestore();
      listAgents.mockRestore();
    }
  });
});
