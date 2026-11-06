import {
  dispatchDocumentSubject,
  dispatchIssueSubject,
  type OpenAskOwner,
  parseBlockFragment,
} from "@legion/contracts";

/**
 * Where an owner's events go and how a session names it: the label agents read in tool
 * results and the wildcard `envoy_subscribe` takes. No write subscribes a session to it;
 * whole-owner subscription is the agent's explicit choice (D3), so a write says when none of the
 * session's subscriptions covers its owner.
 */
export interface OwnerTopic {
  readonly label: string;
  readonly topic: string;
}

/** How tool results name a project document: `<project>/<slug>`. */
export function documentLabel(project: string, slug: string): string {
  return `${project}/${slug}`;
}

/** How the open-asks summary and the per-request answers list name an item's issue or document:
 *  `<KEY>: <title>` or `<project> / <name>`. */
export function ownerTitle(owner: OpenAskOwner): string {
  return "issue" in owner
    ? `${owner.issue.key}: ${owner.issue.title}`
    : `${owner.document.project} / ${owner.document.name}`;
}

export function issueTopic(key: string): OwnerTopic {
  return { label: key, topic: dispatchIssueSubject(key, ">") };
}

export function documentTopicOf(project: string, slug: string): OwnerTopic {
  return {
    label: documentLabel(project, slug),
    topic: dispatchDocumentSubject(project, slug, ">"),
  };
}

/** The `dispatch://` address of an issue. */
export function dispatchIssueRef(key: string): string {
  return `dispatch://${key}`;
}

/** The `dispatch://` address of a document, `owner` being the issue key or project key it lives under. */
export function dispatchDocumentRef(owner: string, slug: string): string {
  return `dispatch://${owner}/artifact/${slug}`;
}

/** The kinds of item whose blocks are its own rather than a document's: an ask, whose parts are
 *  its question, its options and its answer; a comment, whose blocks its own revisions number;
 *  and an issue message, which is never edited, so its blocks carry no revision at all. They are
 *  the parents a block node naming no document has. */
export const ITEM_KINDS = ["ask", "comment", "message"] as const;

/** One of those kinds. */
export type ItemKind = (typeof ITEM_KINDS)[number];

/** The `dispatch://` address of an ask, comment, or message under an owner's address. */
export function dispatchChildRef(ownerRef: string, kind: ItemKind, id: string): string {
  return `${ownerRef}/${kind}/${id}`;
}

/** What a `dispatch://` reference lives under: an issue or a project. */
export type Owner =
  | { readonly kind: "issue"; readonly issue: string }
  | {
      readonly kind: "project";
      readonly project: string;
    };

/** A `dispatch://` reference as `parseDispatchRef` reads it. */
export interface ParsedDispatchRef {
  readonly owner: Owner;
  readonly kind:
    | "issue"
    | "spec"
    | "log"
    | "children"
    | "artifact"
    | "block"
    | "ask"
    | "comment"
    | "message";
  /** The item's id; for a block of a document, the id of its document (`spec` or the slug); for a
   *  block of an ask, a comment or a message, the item's id. */
  readonly id: string;
  readonly version?: number;
  /** The document slug a project-owned ask, comment or block ref names. */
  readonly artifact?: string;
  /** The id of the block a block ref names, decoded. */
  readonly block?: string;
  /** The reference of the document a block ref of a document names a block of. */
  readonly document?: string;
  /** The ask, comment or issue message a block ref of an item names a block of. */
  readonly item?: { readonly kind: ItemKind; readonly id: string };
  /** The version of its document, or the revision of its comment or of its ask's parts, a pinned
   *  block ref (`#b-<id>@v<N>`) names: N's decimal digits as written (`parseBlockFragment`),
   *  which a number past 2^53 would round. */
  readonly pin?: string;
}

/**
 * `ref` as the block reference `<document or item ref>#b-<block id>[@v<N>]` names, or null when
 * it is no such reference: the fragment as `parseBlockFragment` reads it, on an issue's spec or
 * document, or a project's document, at no version, pinned to a version of the document; on an
 * ask, an issue's or a project document's, pinned to a revision of the ask's parts; on a comment,
 * an issue's or a project document's, pinned to a revision of the comment; or on an issue's
 * message, which never changes, so a pinned message block is no reference.
 */
function parseBlockRef(holder: string, fragment: string): ParsedDispatchRef | null {
  // A raw `@` is only ever a pin: no key, slug or id a block's holder names holds one.
  const target = holder.includes("@") ? undefined : parseBlockFragment(fragment);
  const parsed = parseDispatchRef(holder);
  if (target === undefined || parsed === null) {
    return null;
  }
  const pin = target.pin === undefined ? {} : { pin: target.pin };
  const artifact = parsed.artifact === undefined ? {} : { artifact: parsed.artifact };
  if (
    parsed.kind === "ask" ||
    parsed.kind === "comment" ||
    (parsed.kind === "message" && target.pin === undefined)
  ) {
    return {
      owner: parsed.owner,
      kind: "block",
      id: parsed.id,
      block: target.block,
      item: { kind: parsed.kind, id: parsed.id },
      ...artifact,
      ...pin,
    };
  }
  if (parsed.kind !== "spec" && parsed.kind !== "artifact") {
    return null;
  }
  return {
    owner: parsed.owner,
    kind: "block",
    id: parsed.id,
    block: target.block,
    document: holder,
    ...(parsed.owner.kind === "project" ? { artifact: parsed.id } : {}),
    ...pin,
  };
}

/** `ref` as the issue or project item it names, or null for text that is no such reference. A
 *  tool argument is a read request, so the server's target bound, which decides only what text
 *  indexes, is no part of this: a document of any name is read by its own reference. An agent
 *  conversation's upload (`dispatch://agent/...`) is `parseAgentArtifactRef`'s. */
export function parseDispatchRef(ref: string): ParsedDispatchRef | null {
  const hash = ref.indexOf("#");
  if (hash !== -1 && ref.startsWith("b-", hash + 1)) {
    return parseBlockRef(ref.slice(0, hash), ref.slice(hash + 3));
  }
  const projectDocument = ref.match(
    /^dispatch:\/\/([A-Z][A-Z0-9]{1,9})\/artifact\/([^/@]+)(?:@v(\d+))?(?:\/(ask|comment)\/([^/]+))?$/
  );
  if (projectDocument) {
    const [, project, artifact, version, targetKind, targetID] = projectDocument;
    if (
      project === undefined ||
      artifact === undefined ||
      (version !== undefined && Number(version) < 1)
    ) {
      return null;
    }
    if (targetKind === undefined) {
      return {
        owner: { kind: "project", project },
        kind: "artifact",
        id: artifact,
        ...(version === undefined ? {} : { version: Number(version) }),
      };
    }
    // An ask or comment has no version, so a versioned one names nothing.
    if (version !== undefined) return null;
    if (targetID === undefined || (targetKind !== "ask" && targetKind !== "comment")) return null;
    return {
      owner: { kind: "project", project },
      kind: targetKind,
      id: targetID,
      artifact,
    };
  }

  // `activity` is the Activity tab's spelling of the same event list `log` has always named, so
  // both read the issue's events; it is no kind of its own.
  const issueReference = ref.match(
    /^dispatch:\/\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?:\/(spec)|\/(log|activity)|\/(children)|\/artifact\/([^/@]+)(?:@v(\d+))?|\/ask\/([^/]+)|\/comment\/([^/]+)|\/message\/([^/]+))?$/
  );
  if (!issueReference) return null;
  const [, issue, spec, log, children, artifact, version, ask, comment, message] = issueReference;
  if (!issue || (version !== undefined && Number(version) < 1)) return null;
  const owner: Owner = { kind: "issue", issue };
  if (spec) return { owner, kind: "spec", id: spec };
  if (log) return { owner, kind: "log", id: log };
  if (children) return { owner, kind: "children", id: children };
  if (artifact) {
    return {
      owner,
      kind: "artifact",
      id: artifact,
      ...(version === undefined ? {} : { version: Number(version) }),
    };
  }
  if (ask) return { owner, kind: "ask", id: ask };
  if (comment) return { owner, kind: "comment", id: comment };
  if (message) return { owner, kind: "message", id: message };
  return { owner, kind: "issue", id: issue };
}
