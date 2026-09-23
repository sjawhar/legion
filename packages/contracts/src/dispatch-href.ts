/**
 * One rule, shared by every parser that reads a Dispatch dashboard URL: `?comment=` or `?ask=`
 * on a document path names that item, not the document. The SPA emits those hrefs from
 * `documentItemPath`, and the search API emits them from `searchHref`, for an anchored comment
 * or ask on an issue document as well as on a project document.
 */

export interface DispatchHrefItem {
  id: string;
  kind: "ask" | "comment";
}

/**
 * The item a dashboard URL's query names: `undefined` when it names none, `null` when the query
 * is not a shape Dispatch emits (both parameters at once, or an id that is not a decodable
 * segment) and the caller should treat the URL as unresolvable rather than guess.
 */
export function itemFromSearch(search: string): DispatchHrefItem | undefined | null {
  const params = new URLSearchParams(search);
  const ask = params.get("ask");
  const comment = params.get("comment");
  if (ask === null && comment === null) {
    return undefined;
  }
  if (ask !== null && comment !== null) {
    return null;
  }
  const raw = ask ?? comment ?? "";
  if (raw === "" || raw.includes("/")) {
    return null;
  }
  let id: string;
  try {
    id = decodeURIComponent(raw);
  } catch {
    return null;
  }
  return ask === null ? { id, kind: "comment" } : { id, kind: "ask" };
}

/**
 * Every dashboard href Dispatch emits for a search hit or an in-app item link, with the
 * `dispatch://` reference it names today. Three parsers are tested against it - the SPA's
 * `referenceRouteFromHref`, `envoy-client`'s `dispatchRefFromUrl`, and the Go reader in
 * `packages/envoy/internal/dispatch/text` through its JSON copy of these rows - so a new href
 * shape cannot reach one of them and not the others.
 *
 * A `#b-<block>` href resolves to its document, not to the ask the block holds: it is a document
 * link with a block anchor, and naming the ask would need a block-id-to-ask lookup on the server.
 */
export const DISPATCH_HREF_REFERENCES: readonly { href: string; ref: string }[] = [
  { href: "/issues/CORE-1", ref: "dispatch://CORE-1" },
  { href: "/issues/CORE-1/spec", ref: "dispatch://CORE-1/spec" },
  { href: "/issues/CORE-1/artifacts/notes-md", ref: "dispatch://CORE-1/artifact/notes-md" },
  {
    href: "/issues/CORE-1/comments/11111111-1111-4111-8111-111111111111",
    ref: "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111",
  },
  {
    href: "/issues/CORE-1/asks/22222222-2222-4222-8222-222222222222",
    ref: "dispatch://CORE-1/ask/22222222-2222-4222-8222-222222222222",
  },
  {
    href: "/issues/CORE-1/messages/33333333-3333-4333-8333-333333333333",
    ref: "dispatch://CORE-1/message/33333333-3333-4333-8333-333333333333",
  },
  {
    href: "/issues/CORE-1/spec?comment=44444444-4444-4444-8444-444444444444",
    ref: "dispatch://CORE-1/comment/44444444-4444-4444-8444-444444444444",
  },
  {
    href: "/issues/CORE-1/artifacts/notes-md?comment=55555555-5555-4555-8555-555555555555",
    ref: "dispatch://CORE-1/comment/55555555-5555-4555-8555-555555555555",
  },
  {
    href: "/issues/CORE-1/spec?ask=66666666-6666-4666-8666-666666666666",
    ref: "dispatch://CORE-1/ask/66666666-6666-4666-8666-666666666666",
  },
  {
    href: "/issues/CORE-1/artifacts/notes-md?ask=77777777-7777-4777-8777-777777777777",
    ref: "dispatch://CORE-1/ask/77777777-7777-4777-8777-777777777777",
  },
  {
    href: "/projects/CORE/documents/handbook-md",
    ref: "dispatch://CORE/artifact/handbook-md",
  },
  {
    href: "/projects/CORE/documents/handbook-md?comment=88888888-8888-4888-8888-888888888888",
    ref: "dispatch://CORE/artifact/handbook-md/comment/88888888-8888-4888-8888-888888888888",
  },
  {
    href: "/projects/CORE/documents/handbook-md?ask=99999999-9999-4999-8999-999999999999",
    ref: "dispatch://CORE/artifact/handbook-md/ask/99999999-9999-4999-8999-999999999999",
  },
  { href: "/issues/CORE-1/spec#b-ship-it", ref: "dispatch://CORE-1/spec" },
  {
    href: "/issues/CORE-1/artifacts/notes-md#b-ship-it",
    ref: "dispatch://CORE-1/artifact/notes-md",
  },
  {
    href: "/projects/CORE/documents/handbook-md#b-ship-it",
    ref: "dispatch://CORE/artifact/handbook-md",
  },
];
