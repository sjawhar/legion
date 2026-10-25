import {
  dispatchDocumentSubject,
  dispatchIssueSubject,
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

/** The `dispatch://` address of an ask, comment, or message under an owner's address. */
export function dispatchChildRef(
  ownerRef: string,
  kind: "ask" | "comment" | "message",
  id: string
): string {
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
  /** The item's id; for a block, the id of its document (`spec` or the slug). */
  readonly id: string;
  readonly version?: number;
  /** The document slug a project-owned ask, comment or block ref names. */
  readonly artifact?: string;
  /** The id of the block a block ref names, decoded. */
  readonly block?: string;
  /** The reference of the document a block ref names a block of. */
  readonly document?: string;
  /** The version of its document a pinned block ref (`#b-<id>@v<N>`) names. */
  readonly pin?: number;
}

/**
 * `ref` as the block reference `<document ref>#b-<block id>[@v<N>]` names, or null when it is no
 * such reference: the fragment as `parseBlockFragment` reads it, and the document an issue's spec
 * or document, or a project's document, at no version.
 */
function parseBlockRef(document: string, fragment: string): ParsedDispatchRef | null {
  const target = parseBlockFragment(fragment);
  const parsed = parseDispatchRef(document);
  if (
    target === undefined ||
    parsed === null ||
    parsed.version !== undefined ||
    (parsed.kind !== "spec" && parsed.kind !== "artifact")
  ) {
    return null;
  }
  return {
    owner: parsed.owner,
    kind: "block",
    id: parsed.id,
    block: target.block,
    document,
    ...(parsed.owner.kind === "project" ? { artifact: parsed.id } : {}),
    ...(target.pin === undefined ? {} : { pin: target.pin }),
  };
}

/** `ref` as the issue or project item it names, or null for text that is no such reference.
 *  An agent conversation's upload (`dispatch://agent/...`) is `parseAgentArtifactRef`'s. */
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
